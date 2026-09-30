package drive

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type File struct {
	ID        string "json:\"id\""
	Name      string "json:\"name\""
	Size      int64  "json:\"size\""
	CreatedAt string "json:\"created_at\""
}
type Share struct {
	ID          string  "json:\"id\""
	FileID      string  "json:\"file_id\""
	FileName    string  "json:\"file_name\""
	Size        int64   "json:\"size\""
	CreatedAt   string  "json:\"created_at\""
	ExpiresAt   *string "json:\"expires_at\""
	URL         string  "json:\"url\""
	DownloadURL string  "json:\"download_url\""
}

func (a *App) listFiles(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) > 512 {
		apiError(w, 400, "搜索内容过长")
		return
	}
	rows, err := a.db.QueryContext(r.Context(), "SELECT id,name,size,created_at FROM files WHERE status='ready' AND instr(lower(name),lower(?))>0 ORDER BY created_at DESC,id DESC", q)
	if err != nil {
		apiError(w, 500, "无法读取文件列表")
		return
	}
	defer rows.Close()
	result := []File{}
	for rows.Next() {
		var f File
		if err = rows.Scan(&f.ID, &f.Name, &f.Size, &f.CreatedAt); err != nil {
			apiError(w, 500, "无法读取文件列表")
			return
		}
		result = append(result, f)
	}
	if rows.Err() != nil {
		apiError(w, 500, "无法读取文件列表")
		return
	}
	writeJSON(w, 200, map[string]any{"files": result})
}
func (a *App) getFile(id string) (File, error) {
	var f File
	if !validID(id) {
		return f, sql.ErrNoRows
	}
	err := a.db.QueryRow("SELECT id,name,size,created_at FROM files WHERE id=? AND status='ready'", id).Scan(&f.ID, &f.Name, &f.Size, &f.CreatedAt)
	return f, err
}
func (a *App) renameFile(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name string "json:\"name\""
	}
	if !readJSON(w, r, &input) {
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || safeFilename(input.Name) != input.Name {
		apiError(w, 400, "文件名无效，不能包含路径或控制字符")
		return
	}
	result, err := a.db.ExecContext(r.Context(), "UPDATE files SET name=? WHERE id=? AND status='ready'", input.Name, r.PathValue("id"))
	if err != nil {
		apiError(w, 500, "重命名失败")
		return
	}
	if n, _ := result.RowsAffected(); n == 0 {
		apiError(w, 404, "文件不存在")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (a *App) deleteFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		apiError(w, 404, "文件不存在")
		return
	}
	a.uploadMu.Lock()
	defer a.uploadMu.Unlock()
	result, err := a.db.ExecContext(r.Context(), "UPDATE files SET status='deleting' WHERE id=? AND status IN ('ready','deleting')", id)
	if err != nil {
		apiError(w, 500, "删除失败")
		return
	}
	if n, _ := result.RowsAffected(); n == 0 {
		apiError(w, 404, "文件不存在或仍在上传")
		return
	}
	if err = a.stopFileDownloads(r.Context(), id); err != nil {
		apiError(w, 500, "删除未完成，请重试")
		return
	}
	if err = a.removeFileData(id); err != nil {
		slog.Error("delete file data", "id", id, "error", err)
		apiError(w, 500, "删除未完成，请重试")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// Call only after the file is marked deleting. New shares/streams cannot be
// admitted afterwards, so this snapshot includes every previously active share.
func (a *App) stopFileDownloads(ctx context.Context, id string) error {
	rows, err := a.db.QueryContext(ctx, "SELECT id FROM shares WHERE file_id=?", id)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var shareID string
		if err = rows.Scan(&shareID); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, shareID)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, shareID := range ids {
		a.downloads.RevokeShare(shareID)
	}
	return nil
}

func (a *App) removeFileData(id string) error {
	if err := a.removeUploadFiles(id); err != nil {
		return err
	}
	_, err := a.db.Exec("DELETE FROM files WHERE id=?", id)
	return err
}

func (a *App) recoverDeletes() error {
	rows, err := a.db.Query("SELECT id FROM files WHERE status='deleting'")
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = a.removeFileData(id); err != nil {
			return err
		}
	}
	return nil
}
func (a *App) ownerDownload(w http.ResponseWriter, r *http.Request) {
	if !allowDownloadNavigation(w, r) {
		return
	}
	if rejection := checkDownloadRange(r); rejection != nil {
		rejection.Write(w)
		return
	}
	f, err := a.getFile(r.PathValue("id"))
	if err != nil {
		apiError(w, 404, "文件不存在")
		return
	}
	lease, rejection := a.downloads.Acquire(r.Context(), a.clientKey(r), "", true)
	if rejection != nil {
		rejection.Write(w)
		return
	}
	defer lease.Release()
	a.sendFile(w, r, f, lease)
}
func (a *App) sendFile(w http.ResponseWriter, r *http.Request, f File, lease *downloadLease) {
	file, err := os.Open(filepath.Join(a.uploadsDir, f.ID))
	if err != nil {
		apiError(w, 404, "文件不存在")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != f.Size {
		apiError(w, 409, "文件状态不一致，请联系管理员")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": f.Name}))
	w.Header().Set("ETag", fmt.Sprintf("\"%s-%d\"", f.ID, f.Size))
	w.Header().Set("Cache-Control", "private, no-store")
	// Immutable content IDs make all Range requests refer to the same file version.
	if err := lease.ServeContent(w, r, f.Name, info.ModTime(), file); err != nil {
		panic(http.ErrAbortHandler)
	}
}
func (a *App) createShare(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ExpiresInHours int64 "json:\"expires_in_hours\""
	}
	if !readJSON(w, r, &input) {
		return
	}
	if input.ExpiresInHours < 0 || input.ExpiresInHours > 87600 {
		apiError(w, 400, "有效期必须为 0（永久）或不超过十年的小时数")
		return
	}
	f, err := a.getFile(r.PathValue("id"))
	if err != nil {
		apiError(w, 404, "文件不存在")
		return
	}
	s := Share{ID: randomHex(24), FileID: f.ID, FileName: f.Name, Size: f.Size, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	if input.ExpiresInHours > 0 {
		expiry := time.Now().Add(time.Duration(input.ExpiresInHours) * time.Hour).UTC().Format(time.RFC3339)
		s.ExpiresAt = &expiry
	}
	result, err := a.db.ExecContext(r.Context(), "INSERT INTO shares(id,file_id,created_at,expires_at) SELECT ?,id,?,? FROM files WHERE id=? AND status='ready'", s.ID, s.CreatedAt, s.ExpiresAt, s.FileID)
	if err != nil {
		apiError(w, 500, "创建分享失败")
		return
	}
	if n, _ := result.RowsAffected(); n == 0 {
		apiError(w, 409, "文件已被删除或正在删除")
		return
	}
	a.shareURLs(&s)
	writeJSON(w, 201, s)
}
func (a *App) shareURLs(s *Share) {
	s.URL = a.publicPath("/s/" + s.ID)
	// A stable URL independent of the display name also survives renames.
	s.DownloadURL = a.publicPath("/d/" + s.ID + "/download")
}
func (a *App) listShares(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.QueryContext(r.Context(), "SELECT s.id,s.file_id,f.name,f.size,s.created_at,s.expires_at FROM shares s JOIN files f ON s.file_id=f.id WHERE f.status='ready' ORDER BY s.created_at DESC,s.id DESC")
	if err != nil {
		apiError(w, 500, "无法读取分享列表")
		return
	}
	defer rows.Close()
	result := []Share{}
	for rows.Next() {
		var s Share
		if err = rows.Scan(&s.ID, &s.FileID, &s.FileName, &s.Size, &s.CreatedAt, &s.ExpiresAt); err != nil {
			apiError(w, 500, "无法读取分享列表")
			return
		}
		a.shareURLs(&s)
		result = append(result, s)
	}
	if rows.Err() != nil {
		apiError(w, 500, "无法读取分享列表")
		return
	}
	writeJSON(w, 200, map[string]any{"shares": result})
}
func (a *App) deleteShare(w http.ResponseWriter, r *http.Request) {
	result, err := a.db.ExecContext(r.Context(), "DELETE FROM shares WHERE id=?", r.PathValue("id"))
	if err != nil {
		apiError(w, 500, "撤销失败")
		return
	}
	if n, _ := result.RowsAffected(); n == 0 {
		apiError(w, 404, "分享不存在")
		return
	}
	a.downloads.RevokeShare(r.PathValue("id"))
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (a *App) getShare(id string) (Share, error) {
	var s Share
	if len(id) != 48 {
		return s, sql.ErrNoRows
	}
	if _, err := hex.DecodeString(id); err != nil {
		return s, sql.ErrNoRows
	}
	err := a.db.QueryRow("SELECT s.id,s.file_id,f.name,f.size,s.created_at,s.expires_at FROM shares s JOIN files f ON s.file_id=f.id WHERE s.id=? AND f.status='ready'", id).Scan(&s.ID, &s.FileID, &s.FileName, &s.Size, &s.CreatedAt, &s.ExpiresAt)
	if err != nil {
		return s, err
	}
	if s.ExpiresAt != nil {
		expiry, err := time.Parse(time.RFC3339, *s.ExpiresAt)
		if err != nil || !time.Now().Before(expiry) {
			return s, sql.ErrNoRows
		}
	}
	a.shareURLs(&s)
	return s, nil
}
func (a *App) publicShare(w http.ResponseWriter, r *http.Request) {
	s, err := a.getShare(r.PathValue("id"))
	if err != nil {
		apiError(w, 404, "分享不存在、已过期或已撤销")
		return
	}
	writeJSON(w, 200, map[string]any{"file_name": s.FileName, "size": s.Size, "expires_at": s.ExpiresAt, "download_url": s.DownloadURL})
}
func (a *App) sharedDownload(w http.ResponseWriter, r *http.Request) {
	if !allowDownloadNavigation(w, r) {
		return
	}
	if rejection := checkDownloadRange(r); rejection != nil {
		rejection.Write(w)
		return
	}
	s, err := a.getShare(r.PathValue("id"))
	if err != nil {
		apiError(w, 404, "分享不存在、已过期或已撤销")
		return
	}
	ctx := r.Context()
	if s.ExpiresAt != nil {
		expiry, _ := time.Parse(time.RFC3339, *s.ExpiresAt)
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, expiry)
		defer cancel()
	}
	lease, rejection := a.downloads.Acquire(ctx, a.clientKey(r), s.ID, false)
	if rejection != nil {
		rejection.Write(w)
		return
	}
	defer lease.Release()
	// Recheck after registering the stream: a concurrent revocation either
	// fails this lookup or cancels this exact lease after its DB commit.
	s, err = a.getShare(s.ID)
	if err != nil {
		apiError(w, 404, "分享不存在、已过期或已撤销")
		return
	}
	a.sendFile(w, r, File{ID: s.FileID, Name: s.FileName, Size: s.Size, CreatedAt: s.CreatedAt}, lease)
}
