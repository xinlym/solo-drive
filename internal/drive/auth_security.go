package drive

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// These limits only govern login attempts. Authenticated uploads, downloads and
// HTTP Range requests never acquire this limiter.
type loginPolicy struct {
	failures       int
	window         time.Duration
	initialBan     time.Duration
	maximumBan     time.Duration
	strikeReset    time.Duration
	retention      time.Duration
	maxPeers       int
	globalInterval time.Duration
	globalBurst    int
	concurrency    int
}

func defaultLoginPolicy() loginPolicy {
	return loginPolicy{failures: 5, window: 15 * time.Minute, initialBan: 15 * time.Minute, maximumBan: 24 * time.Hour, strikeReset: 24 * time.Hour, retention: 7 * 24 * time.Hour, maxPeers: 4096, globalInterval: 2 * time.Second, globalBurst: 5, concurrency: 2}
}

type loginPeerState struct {
	failures     int
	window       time.Time
	blockedUntil time.Time
	strikes      int
	lastSeen     time.Time
	knownGood    bool
	inflight     bool
}

type loginSecurity struct {
	mu      sync.Mutex
	db      *sql.DB
	policy  loginPolicy
	now     func() time.Time
	peers   map[string]loginPeerState
	tokens  float64
	refill  time.Time
	running int
}

type loginAttempt struct {
	security *loginSecurity
	peer     string
	finished bool
}

var errLoginStateFull = errors.New("login protection state capacity reached")

func (a *App) initLoginSecurity() error {
	s, err := newLoginSecurity(a.db, defaultLoginPolicy(), time.Now)
	if err != nil {
		return err
	}
	a.loginSecurity = s
	return nil
}

func newLoginSecurity(db *sql.DB, policy loginPolicy, now func() time.Time) (*loginSecurity, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS login_security (
  peer TEXT PRIMARY KEY,
  failures INTEGER NOT NULL,
  window_start INTEGER NOT NULL,
  blocked_until INTEGER NOT NULL,
  strikes INTEGER NOT NULL,
  last_seen INTEGER NOT NULL,
  known_good INTEGER NOT NULL DEFAULT 0
 ) WITHOUT ROWID`); err != nil {
		return nil, fmt.Errorf("initialize login protection: %w", err)
	}
	t := now()
	if _, err := db.ExecContext(ctx, "DELETE FROM login_security WHERE last_seen < ? AND blocked_until <= ?", t.Add(-policy.retention).Unix(), t.Unix()); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, "SELECT peer,failures,window_start,blocked_until,strikes,last_seen,known_good FROM login_security LIMIT ?", policy.maxPeers+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	s := &loginSecurity{db: db, policy: policy, now: now, peers: make(map[string]loginPeerState), tokens: float64(policy.globalBurst), refill: t}
	for rows.Next() {
		var peer string
		var b loginPeerState
		var window, blocked, seen int64
		if err := rows.Scan(&peer, &b.failures, &window, &blocked, &b.strikes, &seen, &b.knownGood); err != nil {
			return nil, err
		}
		if peer == "" || len(peer) > 128 || b.failures < 0 || b.strikes < 0 {
			return nil, errors.New("invalid login protection state")
		}
		b.window = time.Unix(window, 0)
		b.blockedUntil = time.Unix(blocked, 0)
		b.lastSeen = time.Unix(seen, 0)
		s.peers[peer] = b
		if len(s.peers) > policy.maxPeers {
			return nil, errLoginStateFull
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *loginSecurity) save(ctx context.Context, peer string, b loginPeerState) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO login_security(peer,failures,window_start,blocked_until,strikes,last_seen,known_good) VALUES(?,?,?,?,?,?,?)
 ON CONFLICT(peer) DO UPDATE SET failures=excluded.failures,window_start=excluded.window_start,blocked_until=excluded.blocked_until,strikes=excluded.strikes,last_seen=excluded.last_seen,known_good=excluded.known_good`, peer, b.failures, b.window.Unix(), b.blockedUntil.Unix(), b.strikes, b.lastSeen.Unix(), b.knownGood)
	return err
}

func (s *loginSecurity) pruneLocked(ctx context.Context, now time.Time) error {
	var expired []string
	for peer, b := range s.peers {
		if !b.inflight && !now.Before(b.blockedUntil) && now.Sub(b.lastSeen) > s.policy.retention {
			expired = append(expired, peer)
		}
	}
	if len(expired) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, peer := range expired {
		if _, err := tx.ExecContext(ctx, "DELETE FROM login_security WHERE peer=?", peer); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, peer := range expired {
		delete(s.peers, peer)
	}
	return nil
}

func (s *loginSecurity) banDuration(strikes int) time.Duration {
	duration := s.policy.initialBan
	for n := 1; n < strikes && duration < s.policy.maximumBan; n++ {
		if duration > s.policy.maximumBan/2 {
			return s.policy.maximumBan
		}
		duration *= 2
	}
	if duration > s.policy.maximumBan {
		duration = s.policy.maximumBan
	}
	return duration
}

// admit persists the attempt before password verification. An interrupted
// request or process restart therefore cannot erase failed-attempt accounting.
// A successful password verification clears this provisional failure below.
func (s *loginSecurity) admit(ctx context.Context, peer string) (*loginAttempt, time.Duration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	b, exists := s.peers[peer]
	if exists && now.Before(b.blockedUntil) {
		return nil, b.blockedUntil.Sub(now), nil
	}
	if exists && b.inflight {
		return nil, time.Second, nil
	}
	if s.running >= s.policy.concurrency {
		return nil, s.policy.globalInterval, nil
	}
	elapsed := now.Sub(s.refill)
	if elapsed > 0 {
		s.tokens = math.Min(float64(s.policy.globalBurst), s.tokens+float64(elapsed)/float64(s.policy.globalInterval))
		s.refill = now
	}
	if s.tokens < 1 {
		return nil, time.Duration((1 - s.tokens) * float64(s.policy.globalInterval)), nil
	}
	// Consume the cheap global admission token before touching persistent state,
	// including when capacity or disk failures will reject this attempt.
	s.tokens--
	opctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if !exists && len(s.peers) >= s.policy.maxPeers {
		if err := s.pruneLocked(opctx, now); err != nil {
			return nil, 0, err
		}
		if len(s.peers) >= s.policy.maxPeers {
			return nil, 0, errLoginStateFull
		}
	}
	if !exists || now.Sub(b.lastSeen) >= s.policy.strikeReset {
		b.failures = 0
		b.strikes = 0
		b.blockedUntil = time.Time{}
		b.window = now
	}
	if now.Sub(b.window) >= s.policy.window {
		b.window = now
		b.failures = 0
	}
	b.failures++
	b.lastSeen = now
	if b.failures >= s.policy.failures {
		// Bound the counter even if policy values change in a future release.
		if b.strikes < 32 {
			b.strikes++
		}
		b.blockedUntil = now.Add(s.banDuration(b.strikes))
	}
	if err := s.save(opctx, peer, b); err != nil {
		return nil, 0, err
	}
	b.inflight = true
	s.peers[peer] = b
	s.running++
	return &loginAttempt{security: s, peer: peer}, 0, nil
}

// finish is idempotent so an early-return defer and the explicit success path
// can share it. Active bans are never evicted just to admit a new source.
func (a *loginAttempt) finish(success bool) error {
	s := a.security
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.finished {
		return nil
	}
	a.finished = true
	b := s.peers[a.peer]
	b.inflight = false
	s.running--
	s.peers[a.peer] = b
	if !success {
		return nil
	}
	reset := b
	reset.failures = 0
	reset.strikes = 0
	reset.blockedUntil = time.Time{}
	reset.window = s.now()
	reset.lastSeen = reset.window
	reset.knownGood = true
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.save(ctx, a.peer, reset); err != nil {
		return err
	}
	// Retain successful source records too. A distributed state-filling attack
	// cannot displace a recently successful owner's existing admission record.
	s.peers[a.peer] = reset
	return nil
}

func (a *App) admitLogin(w http.ResponseWriter, r *http.Request) *loginAttempt {
	if a.loginSecurity == nil {
		apiError(w, 503, "登录防护暂不可用，请稍后重试")
		return nil
	}
	attempt, retry, err := a.loginSecurity.admit(r.Context(), a.clientKey(r))
	if err != nil {
		w.Header().Set("Retry-After", "60")
		apiError(w, 503, "登录防护暂不可用，请稍后重试")
		return nil
	}
	if attempt == nil {
		seconds := max(1, int(math.Ceil(retry.Seconds())))
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		apiError(w, 429, "登录尝试过于频繁，请稍后重试")
	}
	return attempt
}
