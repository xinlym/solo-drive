package drive

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tus/tusd/v2/pkg/filelocker"
	"github.com/tus/tusd/v2/pkg/filestore"
	"github.com/tus/tusd/v2/pkg/handler"
)

const uploadBase = "/api/uploads/"

var errDiskReserve = handler.NewError("ERR_INSUFFICIENT_STORAGE", "Not enough free disk space after the system reserve and unfinished uploads", http.StatusInsufficientStorage)

type uploadEntry struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	Offset    int64  `json:"offset"`
	CreatedAt string `json:"created_at"`
	Status    string `json:"status"`
	UploadURL string `json:"upload_url"`
}

type storageSnapshot struct {
	Total           int64 `json:"total"`
	Used            int64 `json:"used"`
	Available       int64 `json:"available"`
	Reserve         int64 `json:"reserve"`
	UploadAvailable int64 `json:"upload_available"`
	FilesBytes      int64 `json:"files_bytes"`
	PartialBytes    int64 `json:"partial_bytes"`
	PendingBytes    int64 `json:"pending_bytes"`
}

func validUploadID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil && id == strings.ToLower(id)
}

func (a *App) initUploads() error {
	store := filestore.New(a.uploadsDir)
	store.FileModePerm = 0600
	store.DirModePerm = 0700
	durable := &durableUploadStore{app: a, files: store}
	composer := handler.NewStoreComposer()
	composer.UseCore(durable)
	composer.UseTerminater(durable)
	locker := filelocker.New(a.uploadsDir)
	locker.UseIn(composer)
	base := strings.TrimRight(a.cfg.PublicURL, "/") + uploadBase
	h, err := handler.NewHandler(handler.Config{
		BasePath: base, StoreComposer: composer,
		DisableDownload: true, DisableConcatenation: true,
		Cors:                    &handler.CorsConfig{Disable: true},
		NetworkTimeout:          2 * time.Minute,
		PreUploadCreateCallback: a.beforeUploadCreate,
		PreFinishResponseCallback: func(e handler.HookEvent) (handler.HTTPResponse, error) {
			return handler.HTTPResponse{}, a.publishUpload(e.Context, e.Upload.ID)
		},
	})
	if err != nil {
		return err
	}
	a.uploader = h
	return nil
}

// All tus operations and normal file deletions share uploadMu. This serializes
// mutations on a small, single-owner server and makes capacity reservations and
// delete-versus-append ordering deterministic. Browser PATCHes are bounded by
// the client chunk size; the server imposes no fixed file-size limit.
func (a *App) serveUploads(w http.ResponseWriter, r *http.Request) {
	w = &checksumResponseWriter{ResponseWriter: w}
	if r.Method != http.MethodPost && r.Method != http.MethodPatch && r.Method != http.MethodHead && r.Method != http.MethodDelete && r.Method != http.MethodOptions {
		w.Header().Set("Allow", "POST, HEAD, PATCH, DELETE, OPTIONS")
		apiError(w, http.StatusMethodNotAllowed, "unsupported upload method")
		return
	}
	if r.Header.Get("Upload-Concat") != "" || r.Header.Get("Upload-Defer-Length") != "" {
		apiError(w, http.StatusBadRequest, "a known file length is required; concatenation is not supported")
		return
	}
	// Reject alternate method routing before tusd can interpret override headers.
	if r.Header.Get("X-HTTP-Method-Override") != "" {
		apiError(w, http.StatusBadRequest, "method override is not supported")
		return
	}
	if r.Method == http.MethodPatch && r.Header.Get("Upload-Checksum") != "" {
		check, err := parseUploadChecksum(r.Header.Get("Upload-Checksum"), r.ContentLength)
		if err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		if r.Body != nil {
			r.Body = &checksumBody{ReadCloser: r.Body, state: check}
		}
		r = r.WithContext(context.WithValue(r.Context(), uploadChecksumKey{}, check))
	}
	a.uploadMu.Lock()
	defer a.uploadMu.Unlock()
	if r.Method == http.MethodPatch {
		stats, err := a.storageStats(r.Context())
		if err != nil {
			apiError(w, 500, "cannot inspect disk capacity")
			return
		}
		if stats.Available < stats.Reserve || stats.PendingBytes > stats.Available-stats.Reserve {
			apiError(w, http.StatusInsufficientStorage, errDiskReserve.Message)
			return
		}
	}
	http.StripPrefix(strings.TrimSuffix(uploadBase, "/"), a.uploader).ServeHTTP(w, r)
}

func (a *App) beforeUploadCreate(e handler.HookEvent) (handler.HTTPResponse, handler.FileInfoChanges, error) {
	var response handler.HTTPResponse
	var changes handler.FileInfoChanges
	if e.Upload.Size < 0 || e.Upload.SizeIsDeferred || e.Upload.IsPartial || e.Upload.IsFinal {
		return response, changes, handler.NewError("ERR_UPLOAD_LENGTH", "a known nonnegative file length is required", 400)
	}
	stats, err := a.storageStats(e.Context)
	if err != nil {
		return response, changes, err
	}
	if stats.Available <= stats.Reserve || e.Upload.Size > stats.UploadAvailable {
		return response, changes, errDiskReserve
	}
	id := randomHex(16)
	name := safeFilename(e.Upload.MetaData["filename"])
	created := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = a.db.ExecContext(e.Context, "INSERT INTO files(id,name,size,created_at,status) VALUES(?,?,?,?, 'uploading')", id, name, e.Upload.Size, created)
	if err != nil {
		return response, changes, err
	}
	return response, handler.FileInfoChanges{ID: id, MetaData: handler.MetaData{"filename": name, "created_at": created}}, nil
}

func (a *App) uploadEntries(ctx context.Context) ([]uploadEntry, error) {
	rows, err := a.db.QueryContext(ctx, "SELECT id,name,size,created_at,status FROM files ORDER BY created_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := make([]uploadEntry, 0)
	for rows.Next() {
		var e uploadEntry
		if err := rows.Scan(&e.ID, &e.Name, &e.Size, &e.CreatedAt, &e.Status); err != nil {
			return nil, err
		}
		if !validUploadID(e.ID) || e.Size < 0 {
			return nil, fmt.Errorf("invalid upload record")
		}
		st, err := os.Stat(filepath.Join(a.uploadsDir, e.ID))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if err == nil {
			if !st.Mode().IsRegular() {
				return nil, fmt.Errorf("upload is not a regular file")
			}
			e.Offset = st.Size()
		}
		e.UploadURL = strings.TrimRight(a.cfg.PublicURL, "/") + uploadBase + e.ID
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

func addBytes(a, b int64) int64 {
	if b > math.MaxInt64-a {
		return math.MaxInt64
	}
	return a + b
}

// available is raw f_bavail; reserve is the additional application safety
// margin. pending_bytes represents unreceived bytes already promised to active
// uploads, so upload_available is what a NEW upload may reserve.
func (a *App) storageStats(ctx context.Context) (storageSnapshot, error) {
	var s storageSnapshot
	total, used, available, err := diskCapacity(a.uploadsDir)
	if err != nil {
		return s, err
	}
	s.Total, s.Used, s.Available, s.Reserve = total, used, available, a.cfg.ReserveBytes
	entries, err := a.uploadEntries(ctx)
	if err != nil {
		return s, err
	}
	for _, e := range entries {
		if e.Status == "ready" {
			s.FilesBytes = addBytes(s.FilesBytes, e.Offset)
		} else {
			s.PartialBytes = addBytes(s.PartialBytes, e.Offset)
			if e.Status == "uploading" && e.Size > e.Offset {
				s.PendingBytes = addBytes(s.PendingBytes, e.Size-e.Offset)
			}
		}
	}
	if s.Available > s.Reserve && s.Available-s.Reserve > s.PendingBytes {
		s.UploadAvailable = s.Available - s.Reserve - s.PendingBytes
	}
	return s, nil
}

func (a *App) handleUploadList(w http.ResponseWriter, r *http.Request) {
	entries, err := a.uploadEntries(r.Context())
	if err != nil {
		apiError(w, 500, "cannot list uploads")
		return
	}
	active := make([]uploadEntry, 0)
	for _, e := range entries {
		if e.Status == "uploading" {
			active = append(active, e)
		}
	}
	writeJSON(w, 200, map[string]any{"uploads": active})
}

func (a *App) handleStorage(w http.ResponseWriter, r *http.Request) {
	s, err := a.storageStats(r.Context())
	if err != nil {
		apiError(w, 500, "cannot inspect disk capacity")
		return
	}
	writeJSON(w, 200, s)
}

func syncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	return errors.Join(syncErr, closeErr)
}

func (a *App) publishUpload(ctx context.Context, id string) error {
	if !validUploadID(id) {
		return handler.ErrNotFound
	}
	var size int64
	var status string
	if err := a.db.QueryRowContext(ctx, "SELECT size,status FROM files WHERE id=?", id).Scan(&size, &status); err != nil {
		return err
	}
	if status == "deleting" {
		return handler.ErrNotFound
	}
	st, err := os.Stat(filepath.Join(a.uploadsDir, id))
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Size() != size {
		return fmt.Errorf("cannot publish incomplete upload %s", id)
	}
	if err := syncPath(filepath.Join(a.uploadsDir, id)); err != nil {
		return err
	}
	_, err = a.db.ExecContext(ctx, "UPDATE files SET status='ready' WHERE id=? AND status='uploading'", id)
	return err
}

type durableUploadStore struct {
	app   *App
	files filestore.FileStore
}
type durableUpload struct {
	handler.Upload
	store *durableUploadStore
	id    string
}

func (s *durableUploadStore) NewUpload(ctx context.Context, info handler.FileInfo) (handler.Upload, error) {
	raw, err := s.files.NewUpload(ctx, info)
	if err == nil {
		for _, name := range []string{info.ID, info.ID + ".info"} {
			if err = syncPath(filepath.Join(s.app.uploadsDir, name)); err != nil {
				break
			}
		}
		if err == nil {
			err = syncPath(s.app.uploadsDir)
		}
	}
	if err != nil {
		// Creation was not acknowledged. Remove both the disk artifact and its
		// reservation; a crash in this cleanup is repaired during startup.
		cleanErr := s.app.removeUploadFiles(info.ID)
		if cleanErr == nil {
			_, cleanErr = s.app.db.ExecContext(ctx, "DELETE FROM files WHERE id=? AND status='uploading'", info.ID)
		}
		return nil, errors.Join(err, cleanErr)
	}
	return &durableUpload{Upload: raw, store: s, id: info.ID}, nil
}

func (s *durableUploadStore) GetUpload(ctx context.Context, id string) (handler.Upload, error) {
	if !validUploadID(id) {
		return nil, handler.ErrNotFound
	}
	var status string
	if err := s.app.db.QueryRowContext(ctx, "SELECT status FROM files WHERE id=?", id).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, handler.ErrNotFound
		}
		return nil, err
	}
	if status == "deleting" {
		return nil, handler.ErrNotFound
	}
	if err := s.app.recoverChecksumWrite(id); err != nil {
		return nil, err
	}
	u, err := s.files.GetUpload(ctx, id)
	if err != nil {
		return nil, err
	}
	// A final response may have failed after all bytes were received. HEAD or
	// the next retry must repair completion too, not require a process restart.
	if status == "uploading" {
		info, err := u.GetInfo(ctx)
		if err != nil {
			return nil, err
		}
		if !info.SizeIsDeferred && info.Offset == info.Size {
			if err := s.app.publishUpload(ctx, id); err != nil {
				return nil, err
			}
		}
	}
	return &durableUpload{Upload: u, store: s, id: id}, nil
}
func (s *durableUploadStore) AsTerminatableUpload(u handler.Upload) handler.TerminatableUpload {
	return u.(*durableUpload)
}

func (u *durableUpload) WriteChunk(ctx context.Context, offset int64, src io.Reader) (int64, error) {
	// The filestore appends incrementally. Check free space again during the
	// stream because other services can consume space after upload creation.
	a := u.store.app
	check, _ := ctx.Value(uploadChecksumKey{}).(*uploadChecksum)
	var digest hash.Hash
	if check != nil {
		if err := a.beginChecksumWrite(u.id, offset); err != nil {
			return 0, err
		}
		if check.algorithm == "sha256" {
			digest = sha256.New()
		} else {
			digest = sha1.New()
		}
		src = io.TeeReader(src, digest)
	}
	reader := &reserveReader{src: src, path: a.uploadsDir, reserve: a.cfg.ReserveBytes}
	n, writeErr := u.Upload.WriteChunk(ctx, offset, reader)
	path := filepath.Join(a.uploadsDir, u.id)
	if check != nil {
		// tusd's bodyReader hides transport errors from data stores. Only the
		// original HTTP body can tell us whether a chunked request reached EOF.
		complete := writeErr == nil && check.readErr == nil && ((check.length >= 0 && n == check.length) || (check.length < 0 && check.eof))
		if complete && subtle.ConstantTimeCompare(digest.Sum(nil), check.expected) != 1 {
			if err := a.recoverChecksumWrite(u.id); err != nil {
				return 0, err
			}
			// tusd may call FinishUpload even when WriteChunk returns an error;
			// returning zero prevents the rejected bytes reaching that path.
			return 0, handler.NewError("ERR_CHECKSUM_MISMATCH", "upload chunk checksum mismatch", 460)
		}
	}
	syncErr := syncPath(path)
	if check != nil && syncErr == nil {
		// Keep a received prefix after a cleanly handled network interruption.
		// That prefix is resumable but is not a fully verified checksum chunk.
		syncErr = a.clearChecksumWrite(u.id)
		check.committed = syncErr == nil
	}
	return n, errors.Join(writeErr, syncErr)
}
func (u *durableUpload) FinishUpload(ctx context.Context) error {
	if check, _ := ctx.Value(uploadChecksumKey{}).(*uploadChecksum); check != nil && !check.committed {
		return fmt.Errorf("checksum chunk has not been durably committed")
	}
	if err := u.Upload.FinishUpload(ctx); err != nil {
		return err
	}
	return u.store.app.publishUpload(ctx, u.id)
}
func (u *durableUpload) Terminate(ctx context.Context) error {
	a := u.store.app
	if _, err := a.db.ExecContext(ctx, "UPDATE files SET status='deleting' WHERE id=?", u.id); err != nil {
		return err
	}
	if err := a.removeUploadFiles(u.id); err != nil {
		return err
	}
	_, err := a.db.ExecContext(ctx, "DELETE FROM files WHERE id=?", u.id)
	return err
}

// reserveReader checks at most every MiB, bounding both memory and the amount
// written after the last free-space observation. External disk writers cannot
// be reserved atomically; the safety margin absorbs that race.
type reserveReader struct {
	src     io.Reader
	path    string
	reserve int64
	budget  int64
}

func (r *reserveReader) Read(p []byte) (int, error) {
	if r.budget <= 0 {
		_, _, available, err := diskCapacity(r.path)
		if err != nil {
			return 0, err
		}
		if available <= r.reserve {
			return 0, errDiskReserve
		}
		r.budget = available - r.reserve
		if r.budget > 1<<20 {
			r.budget = 1 << 20
		}
	}
	if int64(len(p)) > r.budget {
		p = p[:int(r.budget)]
	}
	n, err := r.src.Read(p)
	r.budget -= int64(n)
	return n, err
}

func (a *App) removeUploadFiles(id string) error {
	if !validUploadID(id) {
		return fmt.Errorf("invalid upload id")
	}
	for _, suffix := range []string{"", ".info", ".checksum-pending"} {
		err := os.Remove(filepath.Join(a.uploadsDir, id+suffix))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return syncPath(a.uploadsDir)
}

// Reconcile is called before serving requests, not while tusd is writing. It
// recovers both sides of the non-transactional SQLite/filesystem boundary and
// never publishes data until its actual size matches the declared length.
func (a *App) reconcileUploads() error {
	a.uploadMu.Lock()
	defer a.uploadMu.Unlock()
	ctx := context.Background()
	entries, err := a.uploadEntries(ctx)
	if err != nil {
		return err
	}
	known := make(map[string]uploadEntry, len(entries))
	for _, e := range entries {
		known[e.ID] = e
	}
	disk, err := os.ReadDir(a.uploadsDir)
	if err != nil {
		return err
	}
	for _, d := range disk {
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".info") {
			continue
		}
		id := strings.TrimSuffix(d.Name(), ".info")
		if !validUploadID(id) {
			continue
		}
		if _, ok := known[id]; ok {
			continue
		}
		f, err := os.Open(filepath.Join(a.uploadsDir, d.Name()))
		if err != nil {
			return err
		}
		var info handler.FileInfo
		decodeErr := json.NewDecoder(io.LimitReader(f, 64<<10)).Decode(&info)
		f.Close()
		if decodeErr != nil || info.ID != id || info.Size < 0 || info.SizeIsDeferred || info.IsPartial || info.IsFinal {
			return fmt.Errorf("invalid orphan upload metadata %s", id)
		}
		st, err := os.Stat(filepath.Join(a.uploadsDir, id))
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Remove(filepath.Join(a.uploadsDir, d.Name())); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() || st.Size() > info.Size {
			return fmt.Errorf("invalid orphan upload data %s", id)
		}
		created := info.MetaData["created_at"]
		if _, err := time.Parse(time.RFC3339Nano, created); err != nil {
			created = st.ModTime().UTC().Format(time.RFC3339Nano)
		}
		e := uploadEntry{ID: id, Name: safeFilename(info.MetaData["filename"]), Size: info.Size, Offset: st.Size(), CreatedAt: created, Status: "uploading"}
		if _, err := a.db.ExecContext(ctx, "INSERT INTO files(id,name,size,created_at,status) VALUES(?,?,?,?, 'uploading')", id, e.Name, e.Size, e.CreatedAt); err != nil {
			return err
		}
		known[id] = e
	}
	for id, e := range known {
		if e.Status == "deleting" {
			if err := a.removeUploadFiles(id); err != nil {
				return err
			}
			if _, err := a.db.ExecContext(ctx, "DELETE FROM files WHERE id=?", id); err != nil {
				return err
			}
			continue
		}
		path := filepath.Join(a.uploadsDir, id)
		if err := a.recoverChecksumWrite(id); err != nil {
			return err
		}
		st, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) && e.Status == "uploading" {
			if err := a.removeUploadFiles(id); err != nil {
				return err
			}
			if _, err := a.db.ExecContext(ctx, "DELETE FROM files WHERE id=?", id); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("upload %s: %w", id, err)
		}
		if !st.Mode().IsRegular() || st.Size() > e.Size {
			return fmt.Errorf("invalid upload size for %s", id)
		}
		// Rebuild absent/invalid metadata from the authoritative database. Paths are
		// always generated by the server, never taken from upload metadata.
		info := handler.FileInfo{ID: id, Size: e.Size, MetaData: handler.MetaData{"filename": e.Name, "created_at": e.CreatedAt}, Storage: map[string]string{"Type": "filestore", "Path": path, "InfoPath": path + ".info"}}
		content, err := json.Marshal(info)
		if err != nil {
			return err
		}
		tmp := path + ".info.tmp"
		if err := os.WriteFile(tmp, content, 0600); err != nil {
			return err
		}
		if err := syncPath(tmp); err != nil {
			return err
		}
		if err := os.Rename(tmp, path+".info"); err != nil {
			return err
		}
		if err := syncPath(a.uploadsDir); err != nil {
			return err
		}
		if st.Size() == e.Size {
			if err := a.publishUpload(ctx, id); err != nil {
				return err
			}
		} else if e.Status == "ready" {
			if _, err := a.db.ExecContext(ctx, "UPDATE files SET status='uploading' WHERE id=?", id); err != nil {
				return err
			}
		}
	}
	// An interrupted NewUpload may leave an empty data file before .info exists.
	// Nonempty unknown files are deliberately preserved for manual recovery.
	for _, d := range disk {
		id := d.Name()
		if !validUploadID(id) {
			continue
		}
		if _, ok := known[id]; ok {
			continue
		}
		st, err := d.Info()
		if err != nil {
			return err
		}
		if st.Mode().IsRegular() && st.Size() == 0 {
			if err := os.Remove(filepath.Join(a.uploadsDir, id)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return syncPath(a.uploadsDir)
}

// checksum support is implemented by SoloDrive around tusd; tusd itself does
// not implement the tus checksum extension. SHA-1 is retained for protocol
// interoperability only; the browser uses SHA-256.
type uploadChecksumKey struct{}
type uploadChecksum struct {
	algorithm string
	expected  []byte
	length    int64
	eof       bool
	readErr   error
	committed bool
}

func parseUploadChecksum(value string, length int64) (*uploadChecksum, error) {
	parts := strings.Fields(value)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid Upload-Checksum header")
	}
	expectedSize := 0
	switch parts[0] {
	case "sha256":
		expectedSize = sha256.Size
	case "sha1":
		expectedSize = sha1.Size
	default:
		return nil, fmt.Errorf("unsupported upload checksum algorithm")
	}
	digest, err := base64.StdEncoding.Strict().DecodeString(parts[1])
	if err != nil || len(digest) != expectedSize {
		return nil, fmt.Errorf("invalid upload checksum digest")
	}
	return &uploadChecksum{algorithm: parts[0], expected: digest, length: length}, nil
}

type checksumBody struct {
	io.ReadCloser
	state *uploadChecksum
}

func (r *checksumBody) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if err == io.EOF {
		r.state.eof = true
	} else if err != nil {
		r.state.readErr = err
	}
	return n, err
}

type checksumResponseWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *checksumResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *checksumResponseWriter) WriteHeader(status int) {
	if !w.wroteHeader {
		h := w.Header()
		ext := h.Get("Tus-Extension")
		present := false
		for _, v := range strings.Split(ext, ",") {
			if strings.TrimSpace(v) == "checksum" {
				present = true
			}
		}
		if !present {
			if ext != "" {
				ext += ","
			}
			h.Set("Tus-Extension", ext+"checksum")
		}
		h.Set("Tus-Checksum-Algorithm", "sha1,sha256")
		// Informational responses must not suppress the eventual final response.
		if status >= 200 {
			w.wroteHeader = true
		}
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *checksumResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

func (a *App) beginChecksumWrite(id string, offset int64) error {
	marker := filepath.Join(a.uploadsDir, id+".checksum-pending")
	if err := os.WriteFile(marker, []byte(strconv.FormatInt(offset, 10)), 0600); err != nil {
		return err
	}
	if err := syncPath(marker); err != nil {
		return err
	}
	return syncPath(a.uploadsDir)
}
func (a *App) clearChecksumWrite(id string) error {
	err := os.Remove(filepath.Join(a.uploadsDir, id+".checksum-pending"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncPath(a.uploadsDir)
}

// A process exit before validating a whole chunk leaves this durable marker.
// Roll back to the last accepted boundary before any completion reconciliation.
func (a *App) recoverChecksumWrite(id string) error {
	marker := filepath.Join(a.uploadsDir, id+".checksum-pending")
	data, err := os.ReadFile(marker)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	offset, err := strconv.ParseInt(string(data), 10, 64)
	if err != nil || offset < 0 {
		return fmt.Errorf("invalid pending checksum offset for %s", id)
	}
	path := filepath.Join(a.uploadsDir, id)
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return a.clearChecksumWrite(id)
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() < offset {
		return fmt.Errorf("invalid pending checksum file for %s", id)
	}
	if err := os.Truncate(path, offset); err != nil {
		return err
	}
	if err := syncPath(path); err != nil {
		return err
	}
	return a.clearChecksumWrite(id)
}
