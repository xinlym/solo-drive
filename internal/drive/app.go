package drive

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tus/tusd/v2/pkg/handler"
	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

type App struct {
	cfg          Config
	db           *sql.DB
	uploadsDir   string
	uploadMu     sync.Mutex
	uploader     *handler.Handler
	passwordHash []byte
	authEpoch    string
	lockFile     *os.File
	loginMu      sync.Mutex
	loginBuckets map[string]*loginBucket
	loginSlot    chan struct{}
	staticFS     fs.FS
}

func New(cfg Config) (*App, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	cfg.DataDir = abs
	if err = os.MkdirAll(abs, 0700); err != nil {
		return nil, err
	}
	a := &App{cfg: cfg, uploadsDir: filepath.Join(abs, "uploads"), loginSlot: make(chan struct{}, 2)}
	if a.lockFile, err = acquireLock(filepath.Join(abs, ".lock")); err != nil {
		return nil, err
	}
	fail := func(e error) (*App, error) { a.Close(); return nil, e }
	if err = os.MkdirAll(a.uploadsDir, 0700); err != nil {
		return fail(err)
	}
	dbURL := url.URL{Scheme: "file", Path: filepath.Join(abs, "drive.db")}
	a.db, err = sql.Open("sqlite", dbURL.String())
	if err != nil {
		return fail(err)
	}
	a.db.SetMaxOpenConns(1)
	for _, statement := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=FULL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
		"CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY,value TEXT NOT NULL)",
		"CREATE TABLE IF NOT EXISTS files (id TEXT PRIMARY KEY,name TEXT NOT NULL,size INTEGER NOT NULL CHECK(size>=0),created_at TEXT NOT NULL,status TEXT NOT NULL DEFAULT 'uploading' CHECK(status IN ('uploading','ready','deleting')))",
		"CREATE TABLE IF NOT EXISTS shares (id TEXT PRIMARY KEY,file_id TEXT NOT NULL REFERENCES files(id) ON DELETE CASCADE,created_at TEXT NOT NULL,expires_at TEXT)",
		"CREATE INDEX IF NOT EXISTS shares_file ON shares(file_id)",
		"CREATE TABLE IF NOT EXISTS sessions (id TEXT PRIMARY KEY,csrf TEXT NOT NULL,expires_at INTEGER NOT NULL,auth_epoch TEXT NOT NULL)",
	} {
		if _, err = a.db.Exec(statement); err != nil {
			return fail(err)
		}
	}
	if err = a.initPassword(); err != nil {
		return fail(err)
	}
	if err = a.initUploads(); err != nil {
		return fail(err)
	}
	if err = a.recoverDeletes(); err != nil {
		return fail(err)
	}
	if err = a.reconcileUploads(); err != nil {
		return fail(err)
	}
	return a, nil
}
func (a *App) Close() error {
	var err error
	if a.db != nil {
		err = a.db.Close()
	}
	if a.lockFile != nil {
		_ = releaseLock(a.lockFile)
		a.lockFile = nil
	}
	return err
}
func (a *App) SetStaticFS(f fs.FS) { a.staticFS = f }

func (a *App) initPassword() error {
	var hash string
	err := a.db.QueryRow("SELECT value FROM settings WHERE key='password_hash'").Scan(&hash)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	same := err == nil && bcrypt.CompareHashAndPassword([]byte(hash), []byte(a.cfg.AdminPassword)) == nil
	if !same {
		b, err := bcrypt.GenerateFromPassword([]byte(a.cfg.AdminPassword), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
		hash = string(b)
		tx, err := a.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for _, kv := range [][2]string{{"password_hash", hash}, {"auth_epoch", randomHex(16)}, {"admin_user", a.cfg.AdminUser}} {
			if _, err = tx.Exec("INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", kv[0], kv[1]); err != nil {
				return err
			}
		}
		if _, err = tx.Exec("DELETE FROM sessions"); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	var oldUser string
	if err = a.db.QueryRow("SELECT value FROM settings WHERE key='admin_user'").Scan(&oldUser); err != nil {
		return err
	}
	if oldUser != a.cfg.AdminUser {
		tx, err := a.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err = tx.Exec("UPDATE settings SET value=? WHERE key='admin_user'", a.cfg.AdminUser); err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE settings SET value=? WHERE key='auth_epoch'", randomHex(16)); err != nil {
			return err
		}
		if _, err = tx.Exec("DELETE FROM sessions"); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	if err = a.db.QueryRow("SELECT value FROM settings WHERE key='auth_epoch'").Scan(&a.authEpoch); err != nil {
		return err
	}
	a.passwordHash = []byte(hash)
	a.cfg.AdminPassword = "" // Do not retain the clear-text credential.
	return nil
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.health)
	mux.HandleFunc("POST /api/login", a.login)
	mux.Handle("GET /api/session", a.auth(http.HandlerFunc(a.session)))
	mux.Handle("POST /api/logout", a.auth(http.HandlerFunc(a.logout)))
	mux.Handle("GET /api/storage", a.auth(http.HandlerFunc(a.handleStorage)))
	mux.Handle("GET /api/files", a.auth(http.HandlerFunc(a.listFiles)))
	mux.Handle("POST /api/files/{id}/rename", a.auth(http.HandlerFunc(a.renameFile)))
	mux.Handle("DELETE /api/files/{id}", a.auth(http.HandlerFunc(a.deleteFile)))
	mux.Handle("GET /api/files/{id}/download", a.auth(http.HandlerFunc(a.ownerDownload)))
	mux.Handle("POST /api/files/{id}/shares", a.auth(http.HandlerFunc(a.createShare)))
	mux.Handle("GET /api/shares", a.auth(http.HandlerFunc(a.listShares)))
	mux.Handle("DELETE /api/shares/{id}", a.auth(http.HandlerFunc(a.deleteShare)))
	mux.Handle("GET /api/uploads", a.auth(http.HandlerFunc(a.handleUploadList)))
	mux.Handle("/api/uploads/", a.auth(http.HandlerFunc(a.serveUploads)))
	mux.HandleFunc("GET /api/public/shares/{id}", a.publicShare)
	mux.HandleFunc("GET /d/{id}/{name...}", a.sharedDownload)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { apiError(w, 404, "接口不存在") })
	mux.HandleFunc("/", a.static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; worker-src 'self' blob:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		w.Header().Set("Cache-Control", "no-store")
		defer func() {
			if err := recover(); err != nil {
				slog.Error("request panic", "error", fmt.Sprint(err))
				apiError(w, 500, "服务器内部错误")
			}
		}()
		mux.ServeHTTP(w, r)
	})
}
func (a *App) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := a.db.PingContext(ctx); err != nil {
		apiError(w, 503, "数据库不可用")
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}
func (a *App) static(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" && r.Method != "HEAD" {
		apiError(w, 405, "不支持的请求")
		return
	}
	if a.staticFS == nil {
		apiError(w, 503, "前端尚未构建")
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" || strings.HasPrefix(name, "s/") {
		name = "index.html"
	}
	if !fs.ValidPath(name) {
		http.NotFound(w, r)
		return
	}
	if _, err := fs.Stat(a.staticFS, name); err != nil {
		http.NotFound(w, r)
		return
	}
	r2 := r.Clone(r.Context())
	u := *r.URL
	u.Path = "/" + name
	if name == "index.html" {
		u.Path = "/"
	}
	r2.URL = &u
	if strings.HasPrefix(name, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}
	http.FileServer(http.FS(a.staticFS)).ServeHTTP(w, r2)
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func apiError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		apiError(w, 400, "请求内容无效")
		return false
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		apiError(w, 400, "请求内容无效")
		return false
	}
	return true
}
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func safeFilename(s string) string {
	s = filepath.Base(strings.ReplaceAll(s, "\\", "/"))
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	for len(s) > 240 {
		_, n := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-n]
	}
	if s == "" || s == "." || s == ".." {
		return "upload.bin"
	}
	return s
}
func validID(s string) bool {
	if len(s) != 32 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
func (a *App) publicPath(path string) string { return strings.TrimRight(a.cfg.PublicURL, "/") + path }
