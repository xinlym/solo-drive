package drive

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/tus/tusd/v2/pkg/filestore"
	"github.com/tus/tusd/v2/pkg/handler"
	_ "modernc.org/sqlite"
)

// This helper deliberately exercises the real tus handler, file store, SQLite
// and filesystem. Authentication is covered separately by the application tests.
func uploadTestApp(t *testing.T, dir string) *App {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	up := filepath.Join(dir, "uploads")
	if err := os.MkdirAll(up, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; CREATE TABLE IF NOT EXISTS files(id TEXT PRIMARY KEY,name TEXT NOT NULL,size INTEGER NOT NULL,created_at TEXT NOT NULL,status TEXT NOT NULL DEFAULT 'uploading')"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	a := &App{cfg: Config{DataDir: dir, ReserveBytes: 0}, db: db, uploadsDir: up}
	if err := a.initUploads(); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := a.reconcileUploads(); err != nil {
		db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return a
}

func tusRequest(t *testing.T, a *App, method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.Header.Set("Tus-Resumable", "1.0.0")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	a.serveUploads(w, r)
	return w
}

func tusCreate(t *testing.T, a *App, size int64, name string) string {
	t.Helper()
	w := tusRequest(t, a, "POST", uploadBase, nil, map[string]string{"Upload-Length": strconv.FormatInt(size, 10), "Upload-Metadata": "filename " + base64.StdEncoding.EncodeToString([]byte(name))})
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if loc == "" {
		t.Fatal("missing upload location")
	}
	return loc
}
func tusPatch(t *testing.T, a *App, url string, offset int64, data string) *httptest.ResponseRecorder {
	t.Helper()
	return tusRequest(t, a, "PATCH", url, []byte(data), map[string]string{"Upload-Offset": strconv.FormatInt(offset, 10), "Content-Type": "application/offset+octet-stream"})
}
func uploadID(url string) string { return url[strings.LastIndex(url, "/")+1:] }
func uploadStatus(t *testing.T, a *App, id string) string {
	t.Helper()
	var status string
	if err := a.db.QueryRow("SELECT status FROM files WHERE id=?", id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestUploadResumeRestartConflictAndPublish(t *testing.T) {
	a := uploadTestApp(t, "")
	loc := tusCreate(t, a, 11, "hello.txt")
	id := uploadID(loc)
	if w := tusPatch(t, a, loc, 0, "hello "); w.Code != 204 {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}
	if status := uploadStatus(t, a, id); status != "uploading" {
		t.Fatalf("partial status: %s", status)
	}
	if w := tusPatch(t, a, loc, 0, "WRONG"); w.Code != 409 {
		t.Fatalf("stale offset: got %d", w.Code)
	}
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}
	b := uploadTestApp(t, a.cfg.DataDir)
	head := tusRequest(t, b, "HEAD", loc, nil, nil)
	if head.Code != 200 || head.Header().Get("Upload-Offset") != "6" || head.Header().Get("Upload-Length") != "11" {
		t.Fatalf("resume HEAD: %d %v", head.Code, head.Header())
	}
	if w := tusPatch(t, b, loc, 6, "world"); w.Code != 204 {
		t.Fatalf("finish: %d %s", w.Code, w.Body.String())
	}
	if status := uploadStatus(t, b, id); status != "ready" {
		t.Fatalf("finished status: %s", status)
	}
	data, err := os.ReadFile(filepath.Join(b.uploadsDir, id))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello world" {
		t.Fatalf("data: %q", data)
	}
	if w := tusPatch(t, b, loc, 11, ""); w.Code != 204 {
		t.Fatalf("completed retry: %d", w.Code)
	}
	if w := tusRequest(t, b, "GET", loc, nil, nil); w.Code != 405 {
		t.Fatalf("tus GET must not expose file: %d", w.Code)
	}
}

func TestUploadCancellationReleasesReservation(t *testing.T) {
	a := uploadTestApp(t, "")
	loc := tusCreate(t, a, 100, "cancel.bin")
	id := uploadID(loc)
	if w := tusPatch(t, a, loc, 0, "12345"); w.Code != 204 {
		t.Fatalf("partial: %d", w.Code)
	}
	stats, err := a.storageStats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if stats.PendingBytes != 95 || stats.PartialBytes != 5 || stats.FilesBytes != 0 {
		t.Fatalf("partial capacity: %+v", stats)
	}
	if w := tusRequest(t, a, "DELETE", loc, nil, nil); w.Code != 204 {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	if w := tusRequest(t, a, "HEAD", loc, nil, nil); w.Code != 404 {
		t.Fatalf("cancelled HEAD: %d", w.Code)
	}
	for _, suffix := range []string{"", ".info"} {
		if _, err := os.Stat(filepath.Join(a.uploadsDir, id+suffix)); !os.IsNotExist(err) {
			t.Fatalf("cancelled file still exists: %s %v", suffix, err)
		}
	}
	stats, err = a.storageStats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if stats.PendingBytes != 0 || stats.PartialBytes != 0 {
		t.Fatalf("reservation not released: %+v", stats)
	}
}

func TestUploadKnownLengthEmptyAnd64Bit(t *testing.T) {
	a := uploadTestApp(t, "")
	loc := tusCreate(t, a, 0, "empty.bin")
	if got := uploadStatus(t, a, uploadID(loc)); got != "ready" {
		t.Fatalf("zero-byte status: %s", got)
	}
	for _, headers := range []map[string]string{
		{"Upload-Length": "-1"},
		{"Upload-Length": "9223372036854775808"},
		{"Upload-Defer-Length": "1"},
		{"Upload-Length": "4", "Upload-Concat": "partial"},
	} {
		if w := tusRequest(t, a, "POST", uploadBase, nil, headers); w.Code < 400 {
			t.Fatalf("accepted invalid length/extension: %+v => %d", headers, w.Code)
		}
	}
	// This reserves bytes in metadata only; it does not create a multi-GB file.
	stats, err := a.storageStats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if stats.UploadAvailable > 1<<32 {
		large := tusCreate(t, a, 1<<32, "large.bin")
		head := tusRequest(t, a, "HEAD", large, nil, nil)
		if head.Header().Get("Upload-Length") != "4294967296" {
			t.Fatalf("64-bit size: %v", head.Header())
		}
		if w := tusRequest(t, a, "DELETE", large, nil, nil); w.Code != 204 {
			t.Fatal(w.Code)
		}
	}
}

func TestUploadCapacityReservationsAndExternalConsumption(t *testing.T) {
	a := uploadTestApp(t, "")
	_, _, available, err := diskCapacity(a.uploadsDir)
	if err != nil {
		t.Fatal(err)
	}
	// Keep this test independent of the host's total disk size and avoid creating
	// large files. There is ample room for SQLite's small metadata allocations.
	const room = int64(32 << 20)
	if available < 2*room {
		t.Skip("not enough free space for reservation test")
	}
	a.cfg.ReserveBytes = available - room
	loc := tusCreate(t, a, 24<<20, "reserved.bin")
	w := tusRequest(t, a, "POST", uploadBase, nil, map[string]string{"Upload-Length": strconv.FormatInt(16<<20, 10)})
	if w.Code != http.StatusInsufficientStorage {
		t.Fatalf("over-reservation: %d %s", w.Code, w.Body.String())
	}
	_, _, now, err := diskCapacity(a.uploadsDir)
	if err != nil {
		t.Fatal(err)
	}
	// Raising the reserve models another process consuming the reserved space,
	// without actually filling the server's disk.
	a.cfg.ReserveBytes = now
	w = tusPatch(t, a, loc, 0, "abc")
	if w.Code != http.StatusInsufficientStorage {
		t.Fatalf("PATCH safety reserve: %d %s", w.Code, w.Body.String())
	}
	head := tusRequest(t, a, "HEAD", loc, nil, nil)
	if head.Header().Get("Upload-Offset") != "0" {
		t.Fatalf("rejected write changed offset: %v", head.Header())
	}
	a.cfg.ReserveBytes = 0
	if w := tusRequest(t, a, "DELETE", loc, nil, nil); w.Code != 204 {
		t.Fatal(w.Code)
	}
}

func TestUploadReconcileOrphanCompletionAndFailedCreate(t *testing.T) {
	a := uploadTestApp(t, "")
	loc := tusCreate(t, a, 4, "recovered.bin")
	id := uploadID(loc)
	// Simulate a process exit after the final write but before the completion
	// callback commits to SQLite.
	if err := os.WriteFile(filepath.Join(a.uploadsDir, id), []byte("done"), 0600); err != nil {
		t.Fatal(err)
	}
	failed := randomHex(16)
	if _, err := a.db.Exec("INSERT INTO files VALUES(?,?,?,?,?)", failed, "failed.bin", 32, "2026-01-01T00:00:00Z", "uploading"); err != nil {
		t.Fatal(err)
	}
	orphan := randomHex(16)
	info := handler.FileInfo{ID: orphan, Size: 7, MetaData: handler.MetaData{"filename": "orphan.bin"}}
	content, _ := json.Marshal(info)
	if err := os.WriteFile(filepath.Join(a.uploadsDir, orphan+".info"), content, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.uploadsDir, orphan), []byte("part"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.reconcileUploads(); err != nil {
		t.Fatal(err)
	}
	if got := uploadStatus(t, a, id); got != "ready" {
		t.Fatalf("lost finish not reconciled: %s", got)
	}
	var count int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM files WHERE id=?", failed).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed creation reservation: %d %v", count, err)
	}
	head := tusRequest(t, a, "HEAD", uploadBase+orphan, nil, nil)
	if head.Code != 200 || head.Header().Get("Upload-Offset") != "4" {
		t.Fatalf("orphan recovery: %d %v", head.Code, head.Header())
	}
	if err := a.reconcileUploads(); err != nil {
		t.Fatalf("reconcile not idempotent: %v", err)
	}
	if w := tusPatch(t, a, uploadBase+orphan, 4, "ial"); w.Code != 204 {
		t.Fatalf("orphan finish: %d %s", w.Code, w.Body.String())
	}
}

func TestUploadReconcileDeletingAndMissingMetadata(t *testing.T) {
	a := uploadTestApp(t, "")
	active := tusCreate(t, a, 5, "resume.bin")
	activeID := uploadID(active)
	if w := tusPatch(t, a, active, 0, "12"); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if err := os.Remove(filepath.Join(a.uploadsDir, activeID+".info")); err != nil {
		t.Fatal(err)
	}
	deleted := tusCreate(t, a, 8, "delete.bin")
	deletedID := uploadID(deleted)
	if _, err := a.db.Exec("UPDATE files SET status='deleting' WHERE id=?", deletedID); err != nil {
		t.Fatal(err)
	}
	if err := a.reconcileUploads(); err != nil {
		t.Fatal(err)
	}
	if w := tusRequest(t, a, "HEAD", active, nil, nil); w.Header().Get("Upload-Offset") != "2" {
		t.Fatalf("metadata reconstruction: %d %v", w.Code, w.Header())
	}
	if w := tusRequest(t, a, "HEAD", deleted, nil, nil); w.Code != 404 {
		t.Fatalf("deletion recovery: %d", w.Code)
	}
}

func TestUploadListAndCapacityHTTP(t *testing.T) {
	a := uploadTestApp(t, "")
	loc := tusCreate(t, a, 10, "active.bin")
	if w := tusPatch(t, a, loc, 0, "123"); w.Code != 204 {
		t.Fatal(w.Code)
	}
	tusCreate(t, a, 0, "ready.bin")
	list := httptest.NewRecorder()
	a.handleUploadList(list, httptest.NewRequest("GET", uploadBase, nil))
	var result struct {
		Uploads []uploadEntry `json:"uploads"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Uploads) != 1 || result.Uploads[0].Offset != 3 {
		t.Fatalf("list: %s", list.Body.String())
	}
	capacity := httptest.NewRecorder()
	a.handleStorage(capacity, httptest.NewRequest("GET", "/api/storage", nil))
	var stats storageSnapshot
	if err := json.Unmarshal(capacity.Body.Bytes(), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Total <= 0 || stats.Used < 0 || stats.Available <= 0 || stats.PendingBytes != 7 || stats.PartialBytes != 3 {
		t.Fatalf("capacity: %+v", stats)
	}
	if stats.UploadAvailable != max(int64(0), stats.Available-stats.Reserve-stats.PendingBytes) {
		t.Fatalf("capacity arithmetic: %+v", stats)
	}
}

// A body error emulates a broken connection halfway through a PATCH. The
// accepted prefix must survive and HEAD must report it, even after restart.
func TestUploadInterruptedBodyPreservesPrefix(t *testing.T) {
	a := uploadTestApp(t, "")
	loc := tusCreate(t, a, 10, "interrupted.bin")
	r := httptest.NewRequest("PATCH", loc, nil)
	r.Body = io.NopCloser(&brokenUploadReader{})
	r.ContentLength = 10
	r.Header.Set("Tus-Resumable", "1.0.0")
	r.Header.Set("Upload-Offset", "0")
	r.Header.Set("Content-Type", "application/offset+octet-stream")
	w := httptest.NewRecorder()
	a.serveUploads(w, r)
	if w.Code < 400 {
		t.Fatalf("broken body unexpectedly succeeded: %d", w.Code)
	}
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}
	b := uploadTestApp(t, a.cfg.DataDir)
	head := tusRequest(t, b, "HEAD", loc, nil, nil)
	if head.Header().Get("Upload-Offset") != "3" {
		t.Fatalf("lost accepted prefix: %d %v", head.Code, head.Header())
	}
	if w := tusPatch(t, b, loc, 3, "4567890"); w.Code != 204 {
		t.Fatalf("resume: %d %s", w.Code, w.Body.String())
	}
}

type brokenUploadReader struct{ read bool }

func (r *brokenUploadReader) Read(p []byte) (int, error) {
	if r.read {
		return 0, fmt.Errorf("simulated disconnect")
	}
	r.read = true
	return copy(p, "123"), nil
}

func TestUploadFailedCreateRollsBackReservation(t *testing.T) {
	a := uploadTestApp(t, "")
	_, changes, err := a.beforeUploadCreate(handler.HookEvent{Context: context.Background(), Upload: handler.FileInfo{Size: 15, MetaData: handler.MetaData{"filename": "failed.bin"}}})
	if err != nil {
		t.Fatal(err)
	}
	id := changes.ID
	// Cause the .info creation to fail after the raw data file was created.
	if err := os.Mkdir(filepath.Join(a.uploadsDir, id+".info"), 0700); err != nil {
		t.Fatal(err)
	}
	store := &durableUploadStore{app: a, files: filestore.New(a.uploadsDir)}
	if _, err := store.NewUpload(t.Context(), handler.FileInfo{ID: id, Size: 15, MetaData: changes.MetaData}); err == nil {
		t.Fatal("expected metadata creation failure")
	}
	var n int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM files WHERE id=?", id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("failed creation left a reservation")
	}
	if _, err := os.Stat(filepath.Join(a.uploadsDir, id)); !os.IsNotExist(err) {
		t.Fatalf("failed creation left raw data: %v", err)
	}
}

func TestUploadCompletedFileCannotBeAppended(t *testing.T) {
	a := uploadTestApp(t, "")
	loc := tusCreate(t, a, 4, "immutable.bin")
	if w := tusPatch(t, a, loc, 0, "safe"); w.Code != 204 {
		t.Fatal(w.Code)
	}
	// tus permits an idempotent retry at the completed offset, but it must not
	// write any supplied data once the declared length has been reached.
	tusPatch(t, a, loc, 4, "changed")
	if w := tusPatch(t, a, loc, 0, "EVIL"); w.Code != 409 {
		t.Fatalf("completed stale PATCH: %d", w.Code)
	}
	data, err := os.ReadFile(filepath.Join(a.uploadsDir, uploadID(loc)))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "safe" {
		t.Fatalf("completed content changed: %q", data)
	}
	if got := uploadStatus(t, a, uploadID(loc)); got != "ready" {
		t.Fatalf("completed status changed: %s", got)
	}
	if _, err := os.Stat(filepath.Join(a.uploadsDir, uploadID(loc)+".info")); err != nil {
		t.Fatalf("completion lost resume metadata: %v", err)
	}
}

func TestUploadFinalResponseFailureRecoveredByHEAD(t *testing.T) {
	a := uploadTestApp(t, "")
	loc := tusCreate(t, a, 4, "finished.bin")
	if err := os.WriteFile(filepath.Join(a.uploadsDir, uploadID(loc)), []byte("done"), 0600); err != nil {
		t.Fatal(err)
	}
	head := tusRequest(t, a, "HEAD", loc, nil, nil)
	if head.Code != 200 || head.Header().Get("Upload-Offset") != "4" {
		t.Fatalf("completion HEAD: %d %v", head.Code, head.Header())
	}
	if got := uploadStatus(t, a, uploadID(loc)); got != "ready" {
		t.Fatalf("HEAD failed to reconcile completion: %s", got)
	}
}

func checksumHeader(algorithm, content string) string {
	if algorithm == "sha1" {
		h := sha1.Sum([]byte(content))
		return "sha1 " + base64.StdEncoding.EncodeToString(h[:])
	}
	h := sha256.Sum256([]byte(content))
	return "sha256 " + base64.StdEncoding.EncodeToString(h[:])
}
func checksumPatch(t *testing.T, a *App, loc string, offset int64, body, checksum string) *httptest.ResponseRecorder {
	return tusRequest(t, a, "PATCH", loc, []byte(body), map[string]string{"Content-Type": "application/offset+octet-stream", "Upload-Offset": strconv.FormatInt(offset, 10), "Upload-Checksum": checksum})
}
func TestUploadChecksumAcceptRollbackAndRetry(t *testing.T) {
	a := uploadTestApp(t, "")
	options := tusRequest(t, a, "OPTIONS", uploadBase, nil, nil)
	if !strings.Contains(options.Header().Get("Tus-Extension"), "checksum") || options.Header().Get("Tus-Checksum-Algorithm") != "sha1,sha256" {
		t.Fatalf("missing checksum extension: %v", options.Header())
	}
	loc := tusCreate(t, a, 6, "checksum.bin")
	id := uploadID(loc)
	if w := checksumPatch(t, a, loc, 0, "abc", checksumHeader("sha256", "abc")); w.Code != 204 {
		t.Fatalf("correct SHA256: %d %s", w.Code, w.Body.String())
	}
	if w := checksumPatch(t, a, loc, 3, "BAD", checksumHeader("sha256", "def")); w.Code != 460 {
		t.Fatalf("mismatch: %d %s", w.Code, w.Body.String())
	}
	head := tusRequest(t, a, "HEAD", loc, nil, nil)
	if head.Header().Get("Upload-Offset") != "3" {
		t.Fatalf("bad final chunk not rolled back: %v", head.Header())
	}
	if got := uploadStatus(t, a, id); got != "uploading" {
		t.Fatalf("bad final chunk published: %s", got)
	}
	data, err := os.ReadFile(filepath.Join(a.uploadsDir, id))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "abc" {
		t.Fatalf("bad bytes persisted: %q", data)
	}
	if w := checksumPatch(t, a, loc, 3, "def", checksumHeader("sha1", "def")); w.Code != 204 {
		t.Fatalf("SHA1 retry: %d %s", w.Code, w.Body.String())
	}
	if got := uploadStatus(t, a, id); got != "ready" {
		t.Fatalf("verified retry not published: %s", got)
	}
	if _, err := os.Stat(filepath.Join(a.uploadsDir, id+".checksum-pending")); !os.IsNotExist(err) {
		t.Fatalf("checksum marker left behind: %v", err)
	}
}

func TestUploadChecksumRejectsMalformedHeader(t *testing.T) {
	a := uploadTestApp(t, "")
	loc := tusCreate(t, a, 3, "headers.bin")
	for _, header := range []string{"sha256", "md5 YWJj", "sha256 !!!", "sha256 YWJj", "sha1 YWJj extra"} {
		if w := checksumPatch(t, a, loc, 0, "abc", header); w.Code != 400 {
			t.Fatalf("invalid header %q: %d", header, w.Code)
		}
	}
	head := tusRequest(t, a, "HEAD", loc, nil, nil)
	if head.Header().Get("Upload-Offset") != "0" {
		t.Fatalf("invalid checksum changed offset: %v", head.Header())
	}
}

func TestUploadChecksumInterruptedBodyKeepsPrefix(t *testing.T) {
	for _, length := range []int64{10, -1} {
		t.Run(strconv.FormatInt(length, 10), func(t *testing.T) {
			a := uploadTestApp(t, "")
			loc := tusCreate(t, a, 10, "interrupted-checksum.bin")
			r := httptest.NewRequest("PATCH", loc, nil)
			r.Body = io.NopCloser(&brokenUploadReader{})
			r.ContentLength = length
			r.Header.Set("Tus-Resumable", "1.0.0")
			r.Header.Set("Upload-Offset", "0")
			r.Header.Set("Content-Type", "application/offset+octet-stream")
			r.Header.Set("Upload-Checksum", checksumHeader("sha256", "1234567890"))
			w := httptest.NewRecorder()
			a.serveUploads(w, r)
			if w.Code < 400 || w.Code == 460 {
				t.Fatalf("interrupted body mistaken for a complete bad checksum: %d %s", w.Code, w.Body.String())
			}
			if err := a.db.Close(); err != nil {
				t.Fatal(err)
			}
			b := uploadTestApp(t, a.cfg.DataDir)
			head := tusRequest(t, b, "HEAD", loc, nil, nil)
			if head.Header().Get("Upload-Offset") != "3" {
				t.Fatalf("partial prefix lost: %v", head.Header())
			}
			if w := checksumPatch(t, b, loc, 3, "4567890", checksumHeader("sha256", "4567890")); w.Code != 204 {
				t.Fatalf("checksum resume: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestUploadChecksumUnknownLengthHTTPBody(t *testing.T) {
	a := uploadTestApp(t, "")
	loc := tusCreate(t, a, 3, "chunked.bin")
	r := httptest.NewRequest("PATCH", loc, strings.NewReader("abc"))
	r.ContentLength = -1
	r.Header.Set("Tus-Resumable", "1.0.0")
	r.Header.Set("Upload-Offset", "0")
	r.Header.Set("Content-Type", "application/offset+octet-stream")
	r.Header.Set("Upload-Checksum", checksumHeader("sha256", "abc"))
	w := httptest.NewRecorder()
	a.serveUploads(w, r)
	if w.Code != 204 {
		t.Fatalf("chunked body checksum: %d %s", w.Code, w.Body.String())
	}
	if got := uploadStatus(t, a, uploadID(loc)); got != "ready" {
		t.Fatalf("chunked verified status: %s", got)
	}
}

func TestUploadChecksumCrashCannotPublishUncheckedFinalChunk(t *testing.T) {
	a := uploadTestApp(t, "")
	loc := tusCreate(t, a, 6, "crash-checksum.bin")
	id := uploadID(loc)
	if w := checksumPatch(t, a, loc, 0, "abc", checksumHeader("sha256", "abc")); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if err := a.beginChecksumWrite(id, 3); err != nil {
		t.Fatal(err)
	}
	// Simulate abrupt process exit after writing the complete final chunk but
	// before its digest was validated. Size alone must not publish these bytes.
	if err := os.WriteFile(filepath.Join(a.uploadsDir, id), []byte("abcBAD"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}
	b := uploadTestApp(t, a.cfg.DataDir)
	head := tusRequest(t, b, "HEAD", loc, nil, nil)
	if head.Header().Get("Upload-Offset") != "3" {
		t.Fatalf("unchecked final bytes survived crash: %v", head.Header())
	}
	if got := uploadStatus(t, b, id); got != "uploading" {
		t.Fatalf("unchecked final bytes published: %s", got)
	}
	if w := checksumPatch(t, b, loc, 3, "def", checksumHeader("sha256", "def")); w.Code != 204 {
		t.Fatalf("crash retry: %d %s", w.Code, w.Body.String())
	}
}
