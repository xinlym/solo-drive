package drive

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type loginTestClock struct {
	mu      sync.Mutex
	current time.Time
}

func (c *loginTestClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.current }
func (c *loginTestClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.current = c.current.Add(d)
	c.mu.Unlock()
}
func setLoginClockForTest(a *App) *loginTestClock {
	c := &loginTestClock{current: time.Now()}
	a.loginSecurity.mu.Lock()
	a.loginSecurity.now = c.Now
	a.loginSecurity.refill = c.Now()
	a.loginSecurity.tokens = float64(a.loginSecurity.policy.globalBurst)
	a.loginSecurity.mu.Unlock()
	return c
}
func loginSecurityForTest(t *testing.T, policy loginPolicy) (*loginSecurity, *loginTestClock) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "login.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	c := &loginTestClock{current: time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)}
	s, err := newLoginSecurity(db, policy, c.Now)
	if err != nil {
		t.Fatal(err)
	}
	return s, c
}
func loginAdmit(t *testing.T, s *loginSecurity, peer string) *loginAttempt {
	t.Helper()
	a, retry, err := s.admit(context.Background(), peer)
	if err != nil || a == nil {
		t.Fatalf("admit %q: retry=%s error=%v", peer, retry, err)
	}
	return a
}
func failLoginAttempts(t *testing.T, s *loginSecurity, c *loginTestClock, peer string, n int) {
	t.Helper()
	for range n {
		c.Advance(s.policy.globalInterval)
		if err := loginAdmit(t, s, peer).finish(false); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoginSecurityEscalatesAndSurvivesRestart(t *testing.T) {
	s, c := loginSecurityForTest(t, defaultLoginPolicy())
	peer := "198.51.100.7/32"
	failLoginAttempts(t, s, c, peer, 5)
	attempt, retry, err := s.admit(t.Context(), peer)
	if err != nil || attempt != nil || retry != 15*time.Minute {
		t.Fatalf("first ban: %v %s %v", attempt, retry, err)
	}
	restarted, err := newLoginSecurity(s.db, s.policy, c.Now)
	if err != nil {
		t.Fatal(err)
	}
	if attempt, retry, err := restarted.admit(t.Context(), peer); err != nil || attempt != nil || retry != 15*time.Minute {
		t.Fatalf("restart lost ban: %v %s %v", attempt, retry, err)
	}
	c.Advance(15*time.Minute + time.Second)
	failLoginAttempts(t, restarted, c, peer, 5)
	if attempt, retry, err := restarted.admit(t.Context(), peer); err != nil || attempt != nil || retry != 30*time.Minute {
		t.Fatalf("second ban did not double: %v %s %v", attempt, retry, err)
	}
	c.Advance(30*time.Minute + time.Second)
	if err := loginAdmit(t, restarted, peer).finish(true); err != nil {
		t.Fatal(err)
	}
	c.Advance(restarted.policy.globalInterval)
	if err := loginAdmit(t, restarted, peer).finish(false); err != nil {
		t.Fatal(err)
	}
	if b := restarted.peers[peer]; b.failures != 1 || b.strikes != 0 || !b.knownGood {
		t.Fatalf("success did not reset failures: %+v", b)
	}
	if got := restarted.banDuration(32); got != 24*time.Hour {
		t.Fatalf("ban maximum: %s", got)
	}
}

func TestLoginSecurityAdmissionPersistsBeforeVerification(t *testing.T) {
	s, c := loginSecurityForTest(t, defaultLoginPolicy())
	peer := "198.51.100.8/32"
	failLoginAttempts(t, s, c, peer, 4)
	c.Advance(s.policy.globalInterval)
	_ = loginAdmit(t, s, peer) // Emulate process exit before finish(passwordOK).
	restarted, err := newLoginSecurity(s.db, s.policy, c.Now)
	if err != nil {
		t.Fatal(err)
	}
	if attempt, _, err := restarted.admit(t.Context(), peer); err != nil || attempt != nil {
		t.Fatalf("interrupted verification erased the fifth attempt: %v %v", attempt, err)
	}
}

func TestLoginSecurityDistributedRateAndRefill(t *testing.T) {
	policy := defaultLoginPolicy()
	policy.globalBurst = 3
	s, c := loginSecurityForTest(t, policy)
	admitted := 0
	for i := range 30 {
		peer := string(rune('a' + i))
		attempt, _, err := s.admit(t.Context(), peer)
		if err != nil {
			t.Fatal(err)
		}
		if attempt != nil {
			admitted++
			attempt.finish(false)
		}
	}
	if admitted != 3 || len(s.peers) != 3 {
		t.Fatalf("distributed attempts bypassed global budget: admitted=%d states=%d", admitted, len(s.peers))
	}
	c.Advance(time.Second)
	if a, retry, err := s.admit(t.Context(), "another"); err != nil || a != nil || retry != time.Second {
		t.Fatalf("partial refill: %v %s %v", a, retry, err)
	}
	c.Advance(time.Second)
	loginAdmit(t, s, "another").finish(false)
}

func TestLoginSecurityConcurrentAdmissionIsBounded(t *testing.T) {
	policy := defaultLoginPolicy()
	policy.globalBurst = 100
	s, _ := loginSecurityForTest(t, policy)
	run := func(samePeer bool) []*loginAttempt {
		start := make(chan struct{})
		results := make(chan *loginAttempt, 32)
		errs := make(chan error, 32)
		var wg sync.WaitGroup
		for i := range 32 {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				peer := "same-peer"
				if !samePeer {
					peer = string(rune('a' + i))
				}
				a, _, err := s.admit(context.Background(), peer)
				if err != nil {
					errs <- err
				}
				if a != nil {
					results <- a
				}
			}(i)
		}
		close(start)
		wg.Wait()
		close(results)
		close(errs)
		for err := range errs {
			t.Error(err)
		}
		var admitted []*loginAttempt
		for a := range results {
			admitted = append(admitted, a)
		}
		return admitted
	}
	first := run(true)
	if len(first) != 1 {
		t.Fatalf("parallel guesses for one source: %d", len(first))
	}
	first[0].finish(false)
	other := run(false)
	if len(other) != policy.concurrency {
		t.Fatalf("global concurrent bcrypt admissions: %d", len(other))
	}
	for _, a := range other {
		a.finish(false)
		a.finish(false)
	}
	if s.running != 0 {
		t.Fatalf("admission slots leaked: %d", s.running)
	}
}

func TestLoginSecurityCapacityDoesNotEvictActiveBansOrOwner(t *testing.T) {
	policy := defaultLoginPolicy()
	policy.maxPeers = 2
	policy.failures = 1
	policy.globalBurst = 10
	s, c := loginSecurityForTest(t, policy)
	loginAdmit(t, s, "owner").finish(true)
	loginAdmit(t, s, "attacker").finish(false)
	if a, _, err := s.admit(t.Context(), "third-source"); a != nil || !errors.Is(err, errLoginStateFull) {
		t.Fatalf("capacity did not fail closed: %v %v", a, err)
	}
	if a, _, err := s.admit(t.Context(), "attacker"); err != nil || a != nil {
		t.Fatalf("active ban was evicted: %v %v", a, err)
	}
	loginAdmit(t, s, "owner").finish(true)
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM login_security").Scan(&count); err != nil || count != 2 {
		t.Fatalf("persistent capacity: %d %v", count, err)
	}
	c.Advance(policy.retention + time.Second)
	loginAdmit(t, s, "new-owner").finish(true)
	if len(s.peers) != 1 {
		t.Fatalf("expired sources not reclaimed: %d", len(s.peers))
	}
}

func TestLoginSecurityPersistenceFailureFailsClosed(t *testing.T) {
	s, _ := loginSecurityForTest(t, defaultLoginPolicy())
	if _, err := s.db.Exec("DROP TABLE login_security"); err != nil {
		t.Fatal(err)
	}
	if a, _, err := s.admit(t.Context(), "198.51.100.1/32"); a != nil || err == nil {
		t.Fatalf("admitted without durable accounting: %v %v", a, err)
	}
	if s.running != 0 || len(s.peers) != 0 {
		t.Fatalf("failed admission leaked state: running=%d peers=%d", s.running, len(s.peers))
	}
}

func TestLoginSecuritySuccessWriteFailureDoesNotIssueSession(t *testing.T) {
	s, _ := loginSecurityForTest(t, defaultLoginPolicy())
	attempt := loginAdmit(t, s, "198.51.100.1/32")
	if _, err := s.db.Exec("DROP TABLE login_security"); err != nil {
		t.Fatal(err)
	}
	if err := attempt.finish(true); err == nil {
		t.Fatal("accepted successful login without durable state reset")
	}
	if s.running != 0 || s.peers["198.51.100.1/32"].inflight {
		t.Fatal("failed success path leaked concurrency slot")
	}
}

type countedLoginBody struct{ reads int }

func (b *countedLoginBody) Read(p []byte) (int, error) { b.reads++; return 0, io.EOF }
func (b *countedLoginBody) Close() error               { return nil }
func TestLoginSecurityRejectsBeforeReadingRequestBody(t *testing.T) {
	s, c := loginSecurityForTest(t, defaultLoginPolicy())
	a := &App{loginSecurity: s}
	r := httptest.NewRequest("POST", "http://drive.test/api/login", nil)
	r.RemoteAddr = "198.51.100.19:4000"
	peer := a.clientKey(r)
	failLoginAttempts(t, s, c, peer, 5)
	body := &countedLoginBody{}
	r.Body = body
	w := httptest.NewRecorder()
	a.login(w, r)
	if w.Code != 429 || body.reads != 0 || w.Header().Get("Retry-After") != "900" {
		t.Fatalf("blocked body consumed: status=%d reads=%d retry=%s", w.Code, body.reads, w.Header().Get("Retry-After"))
	}
}

func TestLoginSecurityIPv6RotationSharesBan(t *testing.T) {
	s, c := loginSecurityForTest(t, defaultLoginPolicy())
	a := &App{loginSecurity: s}
	req := func(addr string) *http.Request {
		r := httptest.NewRequest("POST", "http://drive.test/api/login", nil)
		r.RemoteAddr = addr
		return r
	}
	first := a.clientKey(req("[2001:db8:7:8::1]:3000"))
	rotated := a.clientKey(req("[2001:db8:7:8:ffff::2]:3001"))
	different := a.clientKey(req("[2001:db8:7:9::1]:3002"))
	if first != rotated || first == different {
		t.Fatalf("incorrect IPv6 identity: %q %q %q", first, rotated, different)
	}
	failLoginAttempts(t, s, c, first, 5)
	if attempt, _, err := s.admit(t.Context(), rotated); err != nil || attempt != nil {
		t.Fatalf("IPv6 rotation bypassed ban: %v %v", attempt, err)
	}
	c.Advance(s.policy.globalInterval)
	loginAdmit(t, s, different).finish(false)
}

func TestLoginSecurityOtherSourceCanLoginWhilePeerBanned(t *testing.T) {
	s, c := loginSecurityForTest(t, defaultLoginPolicy())
	if _, err := s.db.Exec("CREATE TABLE sessions(id TEXT PRIMARY KEY,csrf TEXT,expires_at INTEGER,auth_epoch TEXT)"); err != nil {
		t.Fatal(err)
	}
	password := "valid-secret-password"
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	a := &App{cfg: Config{AdminUser: "admin", SessionHours: 1}, db: s.db, passwordHash: passwordHash, authEpoch: "test-epoch", loginSecurity: s}
	attacker := httptest.NewRequest("POST", "http://drive.test/api/login", nil)
	attacker.RemoteAddr = "198.51.100.1:1234"
	failLoginAttempts(t, s, c, a.clientKey(attacker), 5)
	c.Advance(s.policy.globalInterval)
	owner := httptest.NewRequest("POST", "http://drive.test/api/login", strings.NewReader(`{"username":"admin","password":"valid-secret-password"}`))
	owner.RemoteAddr = "198.51.100.2:1234"
	w := httptest.NewRecorder()
	a.login(w, owner)
	if w.Code != 200 || len(w.Result().Cookies()) != 1 {
		t.Fatalf("peer ban blocked owner: %d %s", w.Code, w.Body.String())
	}
}
