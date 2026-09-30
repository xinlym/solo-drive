package drive

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientIdentityTrustAndIPv6Aggregation(t *testing.T) {
	a := &App{cfg: Config{TrustedProxyCIDRs: "127.0.0.1/32,172.19.0.1/32"}}
	cases := []struct{ peer, forwarded, want string }{
		{"203.0.113.10:5555", "198.51.100.5", "203.0.113.10"},
		{"127.0.0.1:5555", "192.0.2.9, 198.51.100.5", "198.51.100.5"},
		{"127.0.0.1:5555", "::ffff:198.51.100.5", "198.51.100.5"},
		{"[2001:db8:abcd:1234::1]:5555", "", "2001:db8:abcd:1234::/64"},
		{"[2001:db8:abcd:1234:ffff::7]:5555", "", "2001:db8:abcd:1234::/64"},
		{"127.0.0.1:5555", "malformed", "127.0.0.1"},
	}
	for _, tc := range cases {
		r := httptest.NewRequest("GET", "https://example.test/", nil)
		r.RemoteAddr = tc.peer
		if tc.forwarded != "" {
			r.Header.Set("X-Forwarded-For", tc.forwarded)
		}
		if got := a.clientKey(r); got != tc.want {
			t.Fatalf("%s: %q != %q", tc.peer, got, tc.want)
		}
	}
	r := httptest.NewRequest("GET", "https://example.test/", nil)
	r.RemoteAddr = "127.0.0.1:4"
	r.Header.Add("X-Forwarded-For", "192.0.2.1")
	r.Header.Add("X-Forwarded-For", "192.0.2.2")
	if a.clientKey(r) != "127.0.0.1" {
		t.Fatal("ambiguous duplicate proxy headers trusted")
	}
}

func TestRequestProtectionRefillAndBoundedState(t *testing.T) {
	g := newRequestGuard(60, 2)
	now := time.Now()
	g.now = func() time.Time { return now }
	for i := 0; i < 2; i++ {
		if ok, _ := g.allow("first"); !ok {
			t.Fatal("initial burst denied")
		}
	}
	if ok, retry := g.allow("first"); ok || retry < 1 {
		t.Fatal("over-limit request accepted")
	}
	if ok, _ := g.allow("second"); !ok {
		t.Fatal("unrelated client blocked")
	}
	now = now.Add(time.Second)
	if ok, _ := g.allow("first"); !ok {
		t.Fatal("tokens did not refill")
	}
	bounded := newRequestGuard(60, 2)
	bounded.now = func() time.Time { return now }
	for i := 0; i < 4096; i++ {
		now = now.Add(10 * time.Millisecond)
		if ok, _ := bounded.allow(fmt.Sprintf("client-%d", i)); !ok {
			t.Fatal("early state rejection", i)
		}
	}
	if ok, _ := bounded.allow("overflow"); ok {
		t.Fatal("unbounded peer state")
	}
	if len(bounded.peers) != 4096 {
		t.Fatal("active peer evicted")
	}
	now = now.Add(11 * time.Minute)
	if ok, _ := bounded.allow("fresh"); !ok || len(bounded.peers) != 1 {
		t.Fatal("expired state not reclaimed")
	}
}

func TestRequestProtectionGlobalBudget(t *testing.T) {
	g := newRequestGuard(60000, 1000)
	now := time.Now()
	g.now = func() time.Time { return now }
	for i := 0; i < 400; i++ {
		if ok, _ := g.allow(fmt.Sprint(i)); !ok {
			t.Fatal("early rejection")
		}
	}
	if ok, _ := g.allow("new-address"); ok {
		t.Fatal("address rotation bypasses aggregate budget")
	}
}

func TestProtectionMiddlewareAndCrossSiteDownloads(t *testing.T) {
	a, server, client, _ := newTestApp(t)
	a.requests = newRequestGuard(60, 1)
	expectStatus(t, requestTest(t, client, "GET", server.URL+"/api/session", "", ""), 200)
	res := requestTest(t, client, "GET", server.URL+"/api/session", "", "")
	if res.Header.Get("Retry-After") == "" {
		t.Fatal("missing retry advice")
	}
	expectStatus(t, res, 429)
	for _, mode := range []string{"no-cors", "cors", "navigate"} {
		r := httptest.NewRequest("GET", "https://example.test/d/id/download", nil)
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		r.Header.Set("Sec-Fetch-Mode", mode)
		w := httptest.NewRecorder()
		allowed := allowDownloadNavigation(w, r)
		if allowed != (mode == "navigate") {
			t.Fatal("wrong cross-site navigation policy", mode)
		}
		if !allowed && w.Code != http.StatusForbidden {
			t.Fatal("wrong denial response")
		}
	}
}

func TestRejectedUnreadBodyCannotHoldConnection(t *testing.T) {
	_, server, _, _ := newTestApp(t)
	target, _ := url.Parse(server.URL)
	conn, err := net.Dial("tcp", target.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	// An unauthenticated delete must reject immediately, even while the sender
	// promises 100 bytes and only sends one. Waiting to drain is a DoS vector.
	_, err = fmt.Fprintf(conn, "DELETE /api/files/0123456789abcdef0123456789abcdef HTTP/1.1\r\nHost: %s\r\nContent-Length: 100\r\n\r\nx", target.Host)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "DELETE"})
	if err != nil {
		t.Fatal("rejection waited for unread request body:", err)
	}
	defer response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatal("unexpected rejection:", response.StatusCode)
	}
}

// The underlying connection here is real TCP, rather than a recorder. Delay
// an immediate deadline very slightly so a mistakenly interrupted background
// read has time to cancel the connection, making the old race reproducible.
type observeReadDeadlineWriter struct {
	http.ResponseWriter
	immediate *atomic.Int32
}

func (w *observeReadDeadlineWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *observeReadDeadlineWriter) SetReadDeadline(deadline time.Time) error {
	err := http.NewResponseController(w.ResponseWriter).SetReadDeadline(deadline)
	if !deadline.IsZero() && !deadline.After(time.Now()) {
		w.immediate.Add(1)
		time.Sleep(time.Millisecond)
	}
	return err
}

func TestConsumedBodiesPreserveOneTCPConnection(t *testing.T) {
	app, err := New(testConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	app.requests = newRequestGuard(60000, 1000)
	var immediate atomic.Int32
	handler := app.Handler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(&observeReadDeadlineWriter{ResponseWriter: w, immediate: &immediate}, r)
	}))
	defer server.Close()
	fixture := fixtureFile(t, app, "reuse.bin", []byte("body deadline reuse"))
	target, _ := url.Parse(server.URL)
	conn, err := net.Dial("tcp", target.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReader(conn)
	readResponse := func(method string) (*http.Response, []byte) {
		t.Helper()
		response, err := http.ReadResponse(reader, &http.Request{Method: method})
		if err != nil {
			t.Fatal("reusable connection failed:", err)
		}
		body, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil || response.StatusCode != 200 || response.Close {
			t.Fatalf("reused %s: status=%d close=%v read=%v body=%s", method, response.StatusCode, response.Close, readErr, body)
		}
		return response, body
	}
	// Login replaces r.Body with MaxBytesReader, so cleanup must retain the
	// original tracker instead of inspecting only the final wrapper's type.
	loginBody, err := json.Marshal(map[string]string{"username": "admin", "password": testPassword})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = fmt.Fprintf(conn, "POST /api/login HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", target.Host, len(loginBody), loginBody); err != nil {
		t.Fatal(err)
	}
	response, responseBody := readResponse("POST")
	var session struct{ CSRF string }
	if err = json.Unmarshal(responseBody, &session); err != nil || session.CSRF == "" {
		t.Fatalf("login session: %v, body=%s", err, responseBody)
	}
	csrf := session.CSRF
	var cookies []string
	for _, cookie := range response.Cookies() {
		cookies = append(cookies, cookie.Name+"="+cookie.Value)
	}
	cookieHeader := strings.Join(cookies, "; ")
	if cookieHeader == "" {
		t.Fatal("login omitted session cookie")
	}
	// Alternate declared-length and chunked bodies, then read an authenticated
	// endpoint on exactly the same socket. No transport can hide a broken socket
	// by automatically creating a replacement connection.
	for n := 0; n < 24; n++ {
		body := fmt.Sprintf(`{"name":"reuse-%d.bin"}`, n)
		headers := fmt.Sprintf("POST /api/files/%s/rename HTTP/1.1\r\nHost: %s\r\nCookie: %s\r\nX-CSRF-Token: %s\r\nContent-Type: application/json\r\n", fixture.ID, target.Host, cookieHeader, csrf)
		if n%2 == 0 {
			_, err = fmt.Fprintf(conn, "%sContent-Length: %d\r\n\r\n%s", headers, len(body), body)
		} else {
			_, err = fmt.Fprintf(conn, "%sTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n0\r\n\r\n", headers, len(body), body)
		}
		if err != nil {
			t.Fatal(err)
		}
		readResponse("POST")
		_, err = fmt.Fprintf(conn, "GET /api/session HTTP/1.1\r\nHost: %s\r\nCookie: %s\r\n\r\n", target.Host, cookieHeader)
		if err != nil {
			t.Fatal(err)
		}
		readResponse("GET")
	}
	if immediate.Load() != 0 {
		t.Fatalf("fully consumed body triggered %d immediate read deadlines", immediate.Load())
	}
}

func TestRequestBodyTrackerKnowsExactLengthAndChunkedEOF(t *testing.T) {
	for _, length := range []int64{3, -1} {
		request := httptest.NewRequest("POST", "http://example.test", strings.NewReader("abc"))
		request.ContentLength = length
		trackRequestBody(request)
		body := request.Body.(*requestBodyTracker)
		if body.consumedOrClosed() {
			t.Fatal("unread body considered consumed")
		}
		if length >= 0 {
			var bytes [3]byte
			if _, err := io.ReadFull(body, bytes[:]); err != nil {
				t.Fatal(err)
			}
		} else {
			if _, err := io.ReadAll(body); err != nil {
				t.Fatal(err)
			}
		}
		if !body.consumedOrClosed() {
			t.Fatalf("complete body length=%d remained unread", length)
		}
		if err := body.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
