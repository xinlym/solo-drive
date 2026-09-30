package drive

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const downloadBufferSize = 32 << 10
const maxDownloadRangeBytes = 128

type downloadPolicy struct {
	MaxPublic, MaxOwner, MaxPerIP, MaxPerShare, MaxShareStates, ShareBurst int
	ShareRate                                                              float64
	WriteTimeout                                                           time.Duration
}

type downloadRejection struct {
	Status     int
	RetryAfter int
	Message    string
}

func (e *downloadRejection) Write(w http.ResponseWriter) {
	if e.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(e.RetryAfter))
	}
	apiError(w, e.Status, e.Message)
}

// Reject amplification before ServeContent: it otherwise supports multipart
// ranges and may ignore a pathological range header by returning the whole file.
func checkDownloadRange(r *http.Request) *downloadRejection {
	values := r.Header.Values("Range")
	if len(values) > 1 || (len(values) == 1 && (len(values[0]) > maxDownloadRangeBytes || strings.Contains(values[0], ","))) {
		return &downloadRejection{Status: http.StatusBadRequest, Message: "一次请求只能下载一个字节范围，请使用多个独立连接下载"}
	}
	return nil
}

type downloadShareState struct {
	tokens  float64
	updated time.Time
	leases  map[*downloadLease]struct{}
}

type downloadGuard struct {
	mu            sync.Mutex
	policy        downloadPolicy
	now           func() time.Time
	public, owner int
	peers         map[string]int
	shares        map[string]*downloadShareState
}

func newDownloadGuard(p downloadPolicy) *downloadGuard {
	if p.MaxPublic <= 0 {
		p.MaxPublic = 48
	}
	if p.MaxOwner <= 0 {
		p.MaxOwner = 16
	}
	if p.MaxPerIP <= 0 {
		p.MaxPerIP = 16
	}
	if p.MaxPerShare <= 0 {
		p.MaxPerShare = 32
	}
	if p.MaxShareStates <= 0 {
		p.MaxShareStates = 4096
	}
	if p.ShareBurst <= 0 {
		p.ShareBurst = 32
	}
	if p.ShareRate <= 0 {
		p.ShareRate = 1
	}
	if p.WriteTimeout <= 0 {
		p.WriteTimeout = 30 * time.Second
	}
	return &downloadGuard{policy: p, now: time.Now, peers: make(map[string]int), shares: make(map[string]*downloadShareState)}
}

// peer must already be canonicalized by App.clientKey (IPv6 clients grouped
// by /64). Public and authenticated traffic use separate capacity pools and
// separate peer counters, so anonymous requests cannot occupy owner slots.
// For shared files: validate in the DB, Acquire, then validate again. Deletion
// must commit before RevokeShare, closing the admission/revocation race.
func (g *downloadGuard) Acquire(ctx context.Context, peer, shareID string, owner bool) (*downloadLease, *downloadRejection) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if ctx.Err() != nil {
		return nil, &downloadRejection{Status: http.StatusRequestTimeout, Message: "下载请求已取消或分享已过期"}
	}
	if !owner && shareID == "" {
		return nil, &downloadRejection{Status: http.StatusNotFound, Message: "分享不存在"}
	}
	if peer == "" {
		peer = "unknown"
	}
	prefix := "public:"
	if owner {
		prefix = "owner:"
	}
	peer = prefix + peer
	busy := func() (*downloadLease, *downloadRejection) {
		return nil, &downloadRejection{Status: http.StatusTooManyRequests, RetryAfter: 1, Message: "下载连接过多，请稍后重试或减少下载器连接数"}
	}
	if g.peers[peer] >= g.policy.MaxPerIP {
		return busy()
	}
	if owner && g.owner >= g.policy.MaxOwner || !owner && g.public >= g.policy.MaxPublic {
		return busy()
	}
	var state *downloadShareState
	if !owner {
		now := g.now()
		state = g.shares[shareID]
		if state == nil {
			// Only fully refilled inactive buckets may be evicted. Evicting a recently
			// exhausted bucket would reset its rate limit and allow a cycling bypass.
			if len(g.shares) >= g.policy.MaxShareStates {
				for id, candidate := range g.shares {
					if len(candidate.leases) == 0 && g.refilledTokens(candidate, now) >= float64(g.policy.ShareBurst) {
						delete(g.shares, id)
					}
				}
			}
			if len(g.shares) >= g.policy.MaxShareStates {
				return nil, &downloadRejection{Status: http.StatusServiceUnavailable, RetryAfter: 1, Message: "下载服务繁忙，请稍后重试"}
			}
			state = &downloadShareState{tokens: float64(g.policy.ShareBurst), updated: now, leases: make(map[*downloadLease]struct{})}
			g.shares[shareID] = state
		}
		if len(state.leases) >= g.policy.MaxPerShare {
			return busy()
		}
		state.tokens = g.refilledTokens(state, now)
		state.updated = now
		if state.tokens < 1 {
			wait := int((1-state.tokens)/g.policy.ShareRate) + 1
			return nil, &downloadRejection{Status: http.StatusTooManyRequests, RetryAfter: wait, Message: "这条分享的下载请求过于频繁，请稍后重试"}
		}
		state.tokens--
	}
	leaseCtx, cancel := context.WithCancel(ctx)
	lease := &downloadLease{guard: g, ctx: leaseCtx, cancel: cancel, peer: peer, shareID: shareID, owner: owner}
	g.peers[peer]++
	if owner {
		g.owner++
	} else {
		g.public++
		state.leases[lease] = struct{}{}
	}
	return lease, nil
}

func (g *downloadGuard) refilledTokens(s *downloadShareState, now time.Time) float64 {
	elapsed := now.Sub(s.updated).Seconds()
	if elapsed < 0 {
		elapsed = 0
	}
	return min(float64(g.policy.ShareBurst), s.tokens+elapsed*g.policy.ShareRate)
}

// RevokeShare keeps no unbounded tombstones. The database is the durable source
// of truth; acquiring callers recheck it after their lease is registered.
func (g *downloadGuard) RevokeShare(id string) {
	g.mu.Lock()
	state := g.shares[id]
	var cancel []context.CancelFunc
	if state != nil {
		for lease := range state.leases {
			cancel = append(cancel, lease.cancel)
		}
		if len(state.leases) == 0 {
			delete(g.shares, id)
		}
	}
	g.mu.Unlock()
	for _, fn := range cancel {
		fn()
	}
}

type downloadLease struct {
	guard         *downloadGuard
	ctx           context.Context
	cancel        context.CancelFunc
	peer, shareID string
	owner         bool
	once          sync.Once
}

func (l *downloadLease) Context() context.Context { return l.ctx }
func (l *downloadLease) Release() {
	l.once.Do(func() {
		l.cancel()
		g := l.guard
		g.mu.Lock()
		defer g.mu.Unlock()
		g.peers[l.peer]--
		if g.peers[l.peer] == 0 {
			delete(g.peers, l.peer)
		}
		if l.owner {
			g.owner--
		} else {
			g.public--
			if state := g.shares[l.shareID]; state != nil {
				delete(state.leases, l)
			}
		}
	})
}

// ServeContent preserves stdlib conditional requests, HEAD and single ranges.
// Both adapters hide optional ReaderFrom/WriterTo fast paths so every byte
// crosses bounded writes, cancellation checks and sliding write deadlines.
// A returned error must not be followed by a JSON/error body: file headers or
// part of the body may already have been sent. The caller still owns Release.
func (l *downloadLease) ServeContent(w http.ResponseWriter, r *http.Request, name string, modtime time.Time, content io.ReadSeeker) error {
	if rejection := checkDownloadRange(r); rejection != nil {
		rejection.Write(w)
		return nil
	}
	ctx, cancel := context.WithCancel(r.Context())
	stopLease := context.AfterFunc(l.ctx, cancel)
	if l.ctx.Err() != nil {
		cancel()
	}
	writer := &downloadWriter{ResponseWriter: w, ctx: ctx, controller: http.NewResponseController(w), timeout: l.guard.policy.WriteTimeout}
	interrupted := make(chan struct{})
	stopInterrupt := context.AfterFunc(ctx, func() { writer.interrupt(); close(interrupted) })
	defer func() {
		stopLease()
		cancel()
		if !stopInterrupt() {
			<-interrupted
		}
		// Avoid leaving a deadline on a reusable HTTP/1 connection or HTTP/2 stream.
		writer.deadlineMu.Lock()
		_ = writer.controller.SetWriteDeadline(time.Time{})
		writer.deadlineMu.Unlock()
	}()
	reader := &downloadReader{source: content, ctx: ctx}
	if err := ctx.Err(); err != nil {
		return err
	}
	http.ServeContent(writer, r.WithContext(ctx), name, modtime, reader)
	if writer.err != nil {
		return writer.err
	}
	return ctx.Err()
}

type downloadReader struct {
	source io.ReadSeeker
	ctx    context.Context
}

func (r *downloadReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) > downloadBufferSize {
		p = p[:downloadBufferSize]
	}
	return r.source.Read(p)
}
func (r *downloadReader) Seek(offset int64, whence int) (int64, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.source.Seek(offset, whence)
}

type downloadWriter struct {
	http.ResponseWriter
	ctx        context.Context
	controller *http.ResponseController
	timeout    time.Duration
	deadlineMu sync.Mutex
	err        error
}

func (w *downloadWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *downloadWriter) interrupt() {
	w.deadlineMu.Lock()
	defer w.deadlineMu.Unlock()
	_ = w.controller.SetWriteDeadline(time.Now())
}
func (w *downloadWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		size := min(len(p), downloadBufferSize)
		w.deadlineMu.Lock()
		err := w.ctx.Err()
		if err == nil {
			err = w.controller.SetWriteDeadline(time.Now().Add(w.timeout))
			if errors.Is(err, http.ErrNotSupported) {
				err = nil
			}
		}
		w.deadlineMu.Unlock()
		if err != nil {
			w.err = err
			return written, err
		}
		n, err := w.ResponseWriter.Write(p[:size])
		written += n
		if err == nil && n != size {
			err = io.ErrShortWrite
		}
		if err != nil {
			w.err = err
			return written, err
		}
		p = p[size:]
	}
	return written, nil
}
