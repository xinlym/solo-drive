package drive

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func takeDownload(t *testing.T, g *downloadGuard, ctx context.Context, peer, share string, owner bool) *downloadLease {
	t.Helper()
	lease, rejection := g.Acquire(ctx, peer, share, owner)
	if rejection != nil {
		t.Fatalf("unexpected rejection: %+v", rejection)
	}
	t.Cleanup(lease.Release)
	return lease
}

func TestDownloadIndependentOwnerCapacity(t *testing.T) {
	g := newDownloadGuard(downloadPolicy{MaxPublic: 2, MaxOwner: 1, MaxPerIP: 2, ShareBurst: 100})
	a := takeDownload(t, g, context.Background(), "same-peer", "a", false)
	b := takeDownload(t, g, context.Background(), "same-peer", "b", false)
	if _, r := g.Acquire(context.Background(), "other-peer", "c", false); r == nil || r.Status != 429 {
		t.Fatal("public pool not capped")
	}
	owner := takeDownload(t, g, context.Background(), "same-peer", "", true)
	if _, r := g.Acquire(context.Background(), "other-peer", "", true); r == nil {
		t.Fatal("owner pool not capped")
	}
	a.Release()
	a.Release()
	b.Release()
	owner.Release()
	if g.public != 0 || g.owner != 0 || len(g.peers) != 0 {
		t.Fatalf("leases leaked: public=%d owner=%d peers=%d", g.public, g.owner, len(g.peers))
	}
}
func TestDownloadDefaultEightRangesAndPeerLimit(t *testing.T) {
	g := newDownloadGuard(downloadPolicy{})
	for i := 0; i < 16; i++ {
		takeDownload(t, g, context.Background(), "2001:db8::/64", "shared", false)
	}
	if _, r := g.Acquire(context.Background(), "2001:db8::/64", "another", false); r == nil {
		t.Fatal("IPv6 /64 peer cap was not shared across shares")
	}
	takeDownload(t, g, context.Background(), "2001:db8:1::/64", "shared", false)
}
func TestDownloadShareConcurrency(t *testing.T) {
	g := newDownloadGuard(downloadPolicy{MaxPerShare: 2})
	takeDownload(t, g, context.Background(), "one", "shared", false)
	takeDownload(t, g, context.Background(), "two", "shared", false)
	if _, r := g.Acquire(context.Background(), "three", "shared", false); r == nil {
		t.Fatal("share concurrency cap missing")
	}
	takeDownload(t, g, context.Background(), "three", "other", false)
}
func TestDownloadShareRateAndBoundedState(t *testing.T) {
	now := time.Unix(1000, 0)
	g := newDownloadGuard(downloadPolicy{ShareBurst: 2, ShareRate: 1, MaxShareStates: 2})
	g.now = func() time.Time { return now }
	for i := 0; i < 2; i++ {
		takeDownload(t, g, context.Background(), "a", "a", false).Release()
	}
	if _, r := g.Acquire(context.Background(), "a", "a", false); r == nil || r.Status != 429 {
		t.Fatal("exhausted share rate accepted")
	}
	takeDownload(t, g, context.Background(), "b", "b", false).Release()
	if _, r := g.Acquire(context.Background(), "c", "c", false); r == nil || r.Status != 503 {
		t.Fatal("recent buckets were evicted or state map exceeded cap")
	}
	if len(g.shares) != 2 {
		t.Fatal("unexpected state count")
	}
	now = now.Add(2 * time.Second)
	takeDownload(t, g, context.Background(), "c", "c", false).Release()
	if len(g.shares) > 2 {
		t.Fatal("state map exceeded cap")
	}
}
func TestDownloadRevokeAndExpiryCancelLeases(t *testing.T) {
	g := newDownloadGuard(downloadPolicy{})
	a := takeDownload(t, g, context.Background(), "one", "a", false)
	b := takeDownload(t, g, context.Background(), "two", "b", false)
	g.RevokeShare("a")
	if !errors.Is(a.Context().Err(), context.Canceled) {
		t.Fatal("revoked lease stayed live")
	}
	if b.Context().Err() != nil {
		t.Fatal("unrelated share was canceled")
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(20*time.Millisecond))
	defer cancel()
	expired := takeDownload(t, g, ctx, "three", "expires", false)
	select {
	case <-expired.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("expiry did not propagate")
	}
	if _, r := g.Acquire(ctx, "four", "expires", false); r == nil {
		t.Fatal("expired context admitted")
	}
}

func TestDownloadRejectAmplifyingRange(t *testing.T) {
	cases := []struct {
		name   string
		values []string
		bad    bool
	}{
		{"none", nil, false}, {"single", []string{"bytes=0-10"}, false}, {"suffix", []string{"bytes=-1024"}, false},
		{"multiple", []string{"bytes=0-1,3-4"}, true}, {"duplicate", []string{"bytes=0-1", "bytes=3-4"}, true},
		{"long", []string{"bytes=" + strings.Repeat("0", 129) + "-"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/file", nil)
			for _, v := range tc.values {
				req.Header.Add("Range", v)
			}
			rejection := checkDownloadRange(req)
			if (rejection != nil) != tc.bad {
				t.Fatalf("unexpected validation: %+v", rejection)
			}
			if rejection != nil {
				w := httptest.NewRecorder()
				rejection.Write(w)
				if w.Code != 400 {
					t.Fatalf("bad range became %d", w.Code)
				}
			}
		})
	}
}

type downloadSpyReader struct {
	*bytes.Reader
	maxRead  int
	writerTo bool
}

func (r *downloadSpyReader) Read(p []byte) (int, error) {
	if len(p) > r.maxRead {
		r.maxRead = len(p)
	}
	return r.Reader.Read(p)
}
func (r *downloadSpyReader) WriteTo(io.Writer) (int64, error) {
	r.writerTo = true
	return 0, errors.New("unbounded fast path used")
}

type downloadSpyWriter struct {
	*httptest.ResponseRecorder
	mu        sync.Mutex
	deadlines []time.Time
	maxWrite  int
}

func (w *downloadSpyWriter) SetWriteDeadline(deadline time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deadlines = append(w.deadlines, deadline)
	return nil
}
func (w *downloadSpyWriter) Write(p []byte) (int, error) {
	if len(p) > w.maxWrite {
		w.maxWrite = len(p)
	}
	return w.ResponseRecorder.Write(p)
}

func TestDownloadServeContentPreservesRangeAndBoundedCopy(t *testing.T) {
	data := bytes.Repeat([]byte("abcdefgh"), 20000)
	for _, tc := range []struct {
		name, method, rangeValue, ifRange string
		status, length                    int
	}{
		{"full", "GET", "", "", 200, len(data)},
		{"partial", "GET", "bytes=3-99999", "", 206, 99997},
		{"if-range-match", "GET", "bytes=3-99999", "\"version\"", 206, 99997},
		{"if-range-mismatch", "GET", "bytes=3-99999", "\"other\"", 200, len(data)},
		{"head", "HEAD", "", "", 200, 0},
		{"unsatisfiable", "GET", "bytes=999999-", "", 416, -1},
		{"multi-refused", "GET", "bytes=0-3,10-20", "", 400, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newDownloadGuard(downloadPolicy{})
			lease := takeDownload(t, g, context.Background(), "peer", "share", false)
			req := httptest.NewRequest(tc.method, "/file", nil)
			if tc.rangeValue != "" {
				req.Header.Set("Range", tc.rangeValue)
			}
			if tc.ifRange != "" {
				req.Header.Set("If-Range", tc.ifRange)
			}
			w := &downloadSpyWriter{ResponseRecorder: httptest.NewRecorder()}
			w.Header().Set("ETag", "\"version\"")
			reader := &downloadSpyReader{Reader: bytes.NewReader(data)}
			if err := lease.ServeContent(w, req, "file.bin", time.Unix(1000, 0), reader); err != nil {
				t.Fatal(err)
			}
			if w.Code != tc.status {
				t.Fatalf("status %d expected %d", w.Code, tc.status)
			}
			if tc.length >= 0 && w.Body.Len() != tc.length {
				t.Fatalf("body length %d expected %d", w.Body.Len(), tc.length)
			}
			if tc.status == 206 && !bytes.Equal(w.Body.Bytes(), data[3:100000]) {
				t.Fatal("range body incorrect")
			}
			if reader.writerTo || reader.maxRead > downloadBufferSize || w.maxWrite > downloadBufferSize {
				t.Fatalf("unbounded transfer: fast=%v read=%d write=%d", reader.writerTo, reader.maxRead, w.maxWrite)
			}
			if tc.status == 400 && reader.maxRead != 0 {
				t.Fatal("rejected multi-range read the file")
			}
			if len(w.deadlines) > 0 && !w.deadlines[len(w.deadlines)-1].IsZero() {
				t.Fatal("deadline leaked into next request")
			}
			if tc.name == "full" && len(w.deadlines) < 3 {
				t.Fatal("deadline was not refreshed between bounded writes")
			}
		})
	}
}

type blockedDownloadWriter struct {
	header                 http.Header
	started, unblock       chan struct{}
	startOnce, unblockOnce sync.Once
	mu                     sync.Mutex
	lastDeadline           time.Time
}

func (w *blockedDownloadWriter) Header() http.Header { return w.header }
func (w *blockedDownloadWriter) WriteHeader(int)     {}
func (w *blockedDownloadWriter) Write([]byte) (int, error) {
	w.startOnce.Do(func() { close(w.started) })
	<-w.unblock
	return 0, os.ErrDeadlineExceeded
}
func (w *blockedDownloadWriter) SetWriteDeadline(deadline time.Time) error {
	w.mu.Lock()
	w.lastDeadline = deadline
	w.mu.Unlock()
	if !deadline.IsZero() && !deadline.After(time.Now()) {
		w.unblockOnce.Do(func() { close(w.unblock) })
	}
	return nil
}
func TestDownloadRevocationInterruptsBlockedWriteAndResetsDeadline(t *testing.T) {
	g := newDownloadGuard(downloadPolicy{})
	lease := takeDownload(t, g, context.Background(), "peer", "share", false)
	w := &blockedDownloadWriter{header: make(http.Header), started: make(chan struct{}), unblock: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		result <- lease.ServeContent(w, httptest.NewRequest("GET", "/file", nil), "f.bin", time.Time{}, bytes.NewReader(make([]byte, 1<<20)))
	}()
	select {
	case <-w.started:
	case <-time.After(time.Second):
		t.Fatal("write never started")
	}
	g.RevokeShare("share")
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("canceled write reported success")
		}
	case <-time.After(time.Second):
		t.Fatal("revocation failed to unblock Write")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.lastDeadline.IsZero() {
		t.Fatal("write deadline not cleared after cancellation")
	}
}
func TestDownloadRequestCancellationStopsStream(t *testing.T) {
	g := newDownloadGuard(downloadPolicy{})
	lease := takeDownload(t, g, context.Background(), "peer", "share", false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := httptest.NewRecorder()
	err := lease.ServeContent(w, httptest.NewRequest("GET", "/file", nil).WithContext(ctx), "f.bin", time.Time{}, bytes.NewReader([]byte("file")))
	if !errors.Is(err, context.Canceled) || w.Body.Len() != 0 {
		t.Fatalf("request cancel lost: %v body=%q", err, w.Body.String())
	}
}
