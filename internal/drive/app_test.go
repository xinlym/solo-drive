package drive

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

const testPassword = "correct-horse-battery-solodrive"

func testConfig(dir string) Config {
	return Config{Addr: "127.0.0.1:0", DataDir: dir, AdminUser: "admin", AdminPassword: testPassword, ReserveBytes: 0, CookieSecure: false, SessionHours: 24}
}
func newTestApp(t *testing.T) (*App, *httptest.Server, *http.Client, string) {
	t.Helper()
	app, err := New(testConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(func() { srv.Close(); app.Close() })
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	csrf := testLogin(t, client, srv.URL, testPassword)
	return app, srv, client, csrf
}
func testLogin(t *testing.T, c *http.Client, base, password string) string {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"username": "admin", "password": password})
	req, _ := http.NewRequest("POST", base+"/api/login", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("login: %d %s", res.StatusCode, body)
	}
	var v struct{ CSRF string }
	if err = json.NewDecoder(res.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	if v.CSRF == "" {
		t.Fatal("empty csrf")
	}
	return v.CSRF
}
func requestTest(t *testing.T, c *http.Client, method, url, csrf, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}
func expectStatus(t *testing.T, res *http.Response, status int) {
	t.Helper()
	defer res.Body.Close()
	if res.StatusCode != status {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("wanted %d, got %d: %s", status, res.StatusCode, body)
	}
	_, _ = io.Copy(io.Discard, res.Body)
}
func fixtureFile(t *testing.T, a *App, name string, content []byte) File {
	t.Helper()
	f := File{ID: randomHex(16), Name: name, Size: int64(len(content)), CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := os.WriteFile(filepath.Join(a.uploadsDir, f.ID), content, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec("INSERT INTO files(id,name,size,created_at,status) VALUES(?,?,?,?,'ready')", f.ID, f.Name, f.Size, f.CreatedAt); err != nil {
		t.Fatal(err)
	}
	return f
}
func fixtureShare(t *testing.T, c *http.Client, base, csrf, id string) Share {
	t.Helper()
	res := requestTest(t, c, "POST", base+"/api/files/"+id+"/shares", csrf, "{\"expires_in_hours\":0}")
	defer res.Body.Close()
	if res.StatusCode != 201 {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("share: %d %s", res.StatusCode, b)
	}
	var s Share
	if err := json.NewDecoder(res.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	return s
}
func TestAuthCSRFAndPrivateFiles(t *testing.T) {
	a, s, c, csrf := newTestApp(t)
	f := fixtureFile(t, a, "private.txt", []byte("private"))
	expectStatus(t, requestTest(t, http.DefaultClient, "GET", s.URL+"/api/files", "", ""), 401)
	expectStatus(t, requestTest(t, http.DefaultClient, "GET", s.URL+"/api/storage", "", ""), 401)
	expectStatus(t, requestTest(t, http.DefaultClient, "GET", s.URL+"/api/files/"+f.ID+"/download", "", ""), 401)
	expectStatus(t, requestTest(t, c, "DELETE", s.URL+"/api/files/"+f.ID, "", ""), 403)
	req, _ := http.NewRequest("DELETE", s.URL+"/api/files/"+f.ID, nil)
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("Origin", "https://evil.invalid")
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	expectStatus(t, res, 403)
	expectStatus(t, requestTest(t, c, "GET", s.URL+"/api/files", "", ""), 200)
	expectStatus(t, requestTest(t, c, "POST", s.URL+"/api/logout", csrf, ""), 200)
	expectStatus(t, requestTest(t, c, "GET", s.URL+"/api/session", "", ""), 401)
}
func TestLoginOriginAndRateLimit(t *testing.T) {
	a, s, _, _ := newTestApp(t)
	clock := setLoginClockForTest(a)
	badBody := `{"username":"admin","password":"wrong-password-value"}`
	req, _ := http.NewRequest("POST", s.URL+"/api/login", strings.NewReader(badBody))
	req.Header.Set("Origin", "https://evil.invalid")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	expectStatus(t, res, 403)
	for range 5 {
		clock.Advance(2 * time.Second)
		expectStatus(t, requestTest(t, http.DefaultClient, "POST", s.URL+"/api/login", "", badBody), 401)
	}
	res = requestTest(t, http.DefaultClient, "POST", s.URL+"/api/login", "", badBody)
	if res.Header.Get("Retry-After") != "900" {
		t.Fatalf("unexpected initial lockout: %s", res.Header.Get("Retry-After"))
	}
	expectStatus(t, res, 429)
}

func TestShareRangesRenameRevokeAndExpiry(t *testing.T) {
	a, s, c, csrf := newTestApp(t)
	data := bytes.Repeat([]byte("large-file-content-"), 4096)
	f := fixtureFile(t, a, "测试 数据.bin", data)
	share := fixtureShare(t, c, s.URL, csrf, f.ID)
	res := requestTest(t, http.DefaultClient, "HEAD", s.URL+share.DownloadURL, "", "")
	if res.Header.Get("Accept-Ranges") != "bytes" || res.Header.Get("Content-Length") != strconv.Itoa(len(data)) || res.Header.Get("ETag") == "" {
		t.Fatal("missing range/length/etag headers")
	}
	etag := res.Header.Get("ETag")
	expectStatus(t, res, 200)
	var wg sync.WaitGroup
	parts := make([][]byte, 8)
	failures := make(chan error, 8)
	for i := range parts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start, end := i*len(data)/8, (i+1)*len(data)/8-1
			req, _ := http.NewRequest("GET", s.URL+share.DownloadURL, nil)
			req.Header.Set("Range", "bytes="+strconv.Itoa(start)+"-"+strconv.Itoa(end))
			req.Header.Set("If-Range", etag)
			r, err := http.DefaultClient.Do(req)
			if err != nil {
				failures <- err
				return
			}
			defer r.Body.Close()
			if r.StatusCode != 206 {
				failures <- &statusError{r.StatusCode}
				return
			}
			parts[i], err = io.ReadAll(r.Body)
			if err != nil {
				failures <- err
			}
		}(i)
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	actual := bytes.Join(parts, nil)
	if sha256.Sum256(actual) != sha256.Sum256(data) {
		t.Fatal("parallel download checksum mismatch")
	}
	expectStatus(t, requestTest(t, c, "POST", s.URL+"/api/files/"+f.ID+"/rename", csrf, "{\"name\":\"renamed.bin\"}"), 200)
	res = requestTest(t, http.DefaultClient, "GET", s.URL+share.DownloadURL, "", "")
	if !strings.Contains(res.Header.Get("Content-Disposition"), "renamed.bin") {
		t.Fatal("renamed filename not reflected")
	}
	expectStatus(t, res, 200)
	expectStatus(t, requestTest(t, c, "DELETE", s.URL+"/api/shares/"+share.ID, csrf, ""), 200)
	expectStatus(t, requestTest(t, http.DefaultClient, "GET", s.URL+share.DownloadURL, "", ""), 404)
	expectStatus(t, requestTest(t, http.DefaultClient, "GET", s.URL+"/api/public/shares/"+share.ID, "", ""), 404)
	second := fixtureShare(t, c, s.URL, csrf, f.ID)
	if _, err := a.db.Exec("UPDATE shares SET expires_at=? WHERE id=?", time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), second.ID); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, requestTest(t, http.DefaultClient, "GET", s.URL+second.DownloadURL, "", ""), 404)
}

type statusError struct{ code int }

func (e *statusError) Error() string { return "unexpected status " + strconv.Itoa(e.code) }

func TestHundredGiBOffsetsWithSparseFixture(t *testing.T) {
	a, s, c, _ := newTestApp(t)
	size := int64(100) << 30
	f := fixtureFile(t, a, "100GiB.bin", nil)
	handle, err := os.OpenFile(filepath.Join(a.uploadsDir, f.ID), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = handle.Truncate(size); err != nil {
		handle.Close()
		t.Fatal(err)
	}
	tail := []byte("end-of-big-file!!")
	if _, err = handle.WriteAt(tail, size-int64(len(tail))); err != nil {
		handle.Close()
		t.Fatal(err)
	}
	handle.Close()
	if _, err = a.db.Exec("UPDATE files SET size=? WHERE id=?", size, f.ID); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", s.URL+"/api/files/"+f.ID+"/download", nil)
	req.Header.Set("Range", "bytes="+strconv.FormatInt(size-int64(len(tail)), 10)+"-")
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	actual, _ := io.ReadAll(res.Body)
	if res.StatusCode != 206 || !bytes.Equal(actual, tail) || !strings.HasSuffix(res.Header.Get("Content-Range"), "/"+strconv.FormatInt(size, 10)) {
		t.Fatalf("64-bit range failed: %d %q", res.StatusCode, res.Header.Get("Content-Range"))
	}
}

func TestPasswordRotationInvalidatesSessions(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(a.Handler())
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	_ = testLogin(t, c, s.URL, testPassword)
	cookies := jar.Cookies(mustURL(s.URL))
	s.Close()
	a.Close()
	cfg.AdminPassword = "a-different-long-password"
	a, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	s = httptest.NewServer(a.Handler())
	defer s.Close()
	jar.SetCookies(mustURL(s.URL), cookies)
	expectStatus(t, requestTest(t, c, "GET", s.URL+"/api/session", "", ""), 401)
	_ = testLogin(t, c, s.URL, cfg.AdminPassword)
}
func TestDeleteCascadeAndProcessLock(t *testing.T) {
	a, s, c, csrf := newTestApp(t)
	if other, err := New(testConfig(a.cfg.DataDir)); err == nil {
		other.Close()
		t.Fatal("same data directory opened twice")
	}
	f := fixtureFile(t, a, "a.txt", []byte("ok"))
	share := fixtureShare(t, c, s.URL, csrf, f.ID)
	expectStatus(t, requestTest(t, c, "DELETE", s.URL+"/api/files/"+f.ID, csrf, ""), 200)
	if _, err := os.Stat(filepath.Join(a.uploadsDir, f.ID)); !os.IsNotExist(err) {
		t.Fatal("file not deleted")
	}
	expectStatus(t, requestTest(t, http.DefaultClient, "GET", s.URL+share.DownloadURL, "", ""), 404)
	var count int
	if err := a.db.QueryRow("SELECT count(*) FROM shares").Scan(&count); err != nil || count != 0 {
		t.Fatal("share not removed")
	}
}
func TestSafeFilenameAndSessionTokenHash(t *testing.T) {
	for _, name := range []string{"../a.txt", "..\\a.txt", "\x00a.txt"} {
		if safeFilename(name) != "a.txt" {
			t.Errorf("filename %q -> %q", name, safeFilename(name))
		}
	}
	if safeFilename("..") != "upload.bin" {
		t.Fatal("dot filename")
	}
	h := sha256.Sum256([]byte("abc"))
	if tokenHash("abc") != hex.EncodeToString(h[:]) {
		t.Fatal("token hash")
	}
}
func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

func TestSPAEntryAndUnknownPaths(t *testing.T) {
	a, s, c, _ := newTestApp(t)
	a.SetStaticFS(fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<!doctype html><title>SoloDrive</title>")}, "assets/app.js": &fstest.MapFile{Data: []byte("console.log(1)")}})
	for _, path := range []string{"/", "/s/abc", "/assets/app.js"} {
		res := requestTest(t, c, "GET", s.URL+path, "", "")
		if res.Request.URL.Path != path {
			t.Fatalf("unexpected redirect from %s to %s", path, res.Request.URL.Path)
		}
		expectStatus(t, res, 200)
	}
	expectStatus(t, requestTest(t, c, "GET", s.URL+"/unknown", "", ""), 404)
	expectStatus(t, requestTest(t, c, "GET", s.URL+"/api/unknown", "", ""), 404)
}

func TestLoginLimiterIsPerPeerAndTrustExplicit(t *testing.T) {
	a, s, _, _ := newTestApp(t)
	a.cfg.TrustedProxyCIDRs = "127.0.0.0/8"
	clock := setLoginClockForTest(a)
	blocked := httptest.NewRequest("POST", s.URL+"/api/login", nil)
	blocked.RemoteAddr = "127.0.0.1:1234"
	blocked.Header.Set("X-Forwarded-For", "198.51.100.1")
	failLoginAttempts(t, a.loginSecurity, clock, a.clientKey(blocked), 5)
	body, _ := json.Marshal(map[string]string{"username": "admin", "password": testPassword})
	request := func(ip string) *http.Response {
		r, _ := http.NewRequest("POST", s.URL+"/api/login", bytes.NewReader(body))
		r.Header.Set("X-Forwarded-For", ip)
		res, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	expectStatus(t, request("198.51.100.1"), 429)
	clock.Advance(2 * time.Second)
	expectStatus(t, request("198.51.100.2"), 200)
	r := httptest.NewRequest("POST", "http://example.test", nil)
	r.RemoteAddr = "203.0.113.5:1111"
	r.Header.Set("X-Forwarded-For", "198.51.100.8")
	if peer := a.clientIP(r).String(); peer != "203.0.113.5" {
		t.Fatal("untrusted forwarded address accepted", peer)
	}
}
