package drive

import (
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The direct peer is authoritative unless it is one of our explicitly trusted
// reverse proxies. Bundled proxies overwrite X-Forwarded-For at each hop.
func (a *App) clientIP(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	direct, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	direct = direct.Unmap().WithZone("")
	for _, cidr := range strings.Split(a.cfg.TrustedProxyCIDRs, ",") {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(cidr))
		if err != nil || !prefix.Contains(direct) {
			continue
		}
		values := r.Header.Values("X-Forwarded-For")
		// Multiple header lines and oversized values are ambiguous and untrusted.
		if len(values) != 1 || len(values[0]) > 1024 {
			return direct
		}
		parts := strings.Split(values[0], ",")
		ip, err := netip.ParseAddr(strings.TrimSpace(parts[len(parts)-1]))
		if err == nil && ip.Zone() == "" {
			return ip.Unmap()
		}
		return direct
	}
	return direct
}

func (a *App) clientKey(r *http.Request) string {
	ip := a.clientIP(r)
	if !ip.IsValid() {
		return "unknown"
	}
	if ip.Is4() {
		return ip.String()
	}
	return netip.PrefixFrom(ip, 64).Masked().String()
}

type requestBucket struct {
	tokens  float64
	updated time.Time
}
type requestGuard struct {
	mu          sync.Mutex
	peers       map[string]*requestBucket
	global      requestBucket
	rate, burst float64
	now         func() time.Time
	sweep       time.Time
}

func newRequestGuard(perMinute, burst int) *requestGuard {
	return &requestGuard{peers: make(map[string]*requestBucket), rate: float64(perMinute) / 60, burst: float64(burst), now: time.Now}
}

func refillRequestBucket(b *requestBucket, now time.Time, rate, burst float64) {
	if b.updated.IsZero() {
		b.tokens = burst
	} else if now.After(b.updated) {
		b.tokens = math.Min(burst, b.tokens+now.Sub(b.updated).Seconds()*rate)
	}
	b.updated = now
}

func (g *requestGuard) allow(peer string) (bool, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	// Bound aggregate work even if a botnet uses many separate addresses.
	refillRequestBucket(&g.global, now, 200, 400)
	if g.global.tokens < 1 {
		return false, 1
	}
	g.global.tokens--
	if g.sweep.IsZero() || now.Sub(g.sweep) >= time.Minute {
		for k, b := range g.peers {
			if now.Sub(b.updated) >= 10*time.Minute {
				delete(g.peers, k)
			}
		}
		g.sweep = now
	}
	b := g.peers[peer]
	if b == nil {
		// Do not let rotating addresses evict another client's active limit.
		if len(g.peers) >= 4096 {
			return false, 60
		}
		b = &requestBucket{}
		g.peers[peer] = b
	}
	refillRequestBucket(b, now, g.rate, g.burst)
	if b.tokens < 1 {
		return false, max(1, int(math.Ceil((1-b.tokens)/g.rate)))
	}
	b.tokens--
	return true, 0
}

func (a *App) allowRequest(w http.ResponseWriter, r *http.Request) bool {
	if len(r.URL.RequestURI()) > 4096 {
		apiError(w, http.StatusRequestURITooLong, "请求地址过长")
		return false
	}
	if ok, retry := a.requests.allow(a.clientKey(r)); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		apiError(w, http.StatusTooManyRequests, "请求过于频繁，请稍后重试")
		return false
	}
	return true
}

// Stop browser embedding from silently turning shared downloads into hotlinks.
// Navigations from another website and ordinary download clients remain valid.
func allowDownloadNavigation(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" && r.Header.Get("Sec-Fetch-Mode") != "navigate" {
		apiError(w, http.StatusForbidden, "请通过分享页面或下载链接访问文件")
		return false
	}
	return true
}

// requestBodyTracker distinguishes a fully consumed body from a rejected body
// that net/http might otherwise drain after the handler returns. It must wrap
// the original body before auth/tusd create shallow request copies.
type requestBodyTracker struct {
	io.ReadCloser
	expected  int64
	mu        sync.Mutex
	read      int64
	eof       bool
	closing   bool
	closeOnce sync.Once
	closeErr  error
}

func (b *requestBodyTracker) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.mu.Lock()
	b.read += int64(n)
	if err == io.EOF {
		b.eof = true
	}
	b.mu.Unlock()
	return n, err
}
func (b *requestBodyTracker) Close() error {
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closing = true
		b.mu.Unlock()
		b.closeErr = b.ReadCloser.Close()
	})
	return b.closeErr
}
func (b *requestBodyTracker) consumedOrClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closing || b.eof || (b.expected >= 0 && b.read >= b.expected)
}
func trackRequestBody(r *http.Request) *requestBodyTracker {
	if r.Body == nil || r.Body == http.NoBody {
		return nil
	}
	if tracked, ok := r.Body.(*requestBodyTracker); ok {
		return tracked
	}
	tracked := &requestBodyTracker{ReadCloser: r.Body, expected: r.ContentLength}
	r.Body = tracked
	return tracked
}

// Never set an immediate read deadline after EOF: HTTP/1 may already be doing
// its background read for the next request, and a timeout there permanently
// cancels the connection context even when the deadline is subsequently reset.
// Only an unread body needs forced cleanup to prevent a slow-body drain.
func closeRequestBody(w http.ResponseWriter, r *http.Request, tracked *requestBodyTracker) {
	if r.Body == nil || r.Body == http.NoBody {
		return
	}
	// readJSON and tusd may replace r.Body with another wrapper. The original
	// tracker is retained by the outer handler, independently of that wrapper.
	if tracked != nil && tracked.consumedOrClosed() {
		_ = r.Body.Close()
		return
	}
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now())
	_ = r.Body.Close()
	_ = controller.SetReadDeadline(time.Time{})
}
