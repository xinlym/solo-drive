package drive

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const cookieName = "solodrive_session"

type sessionKey struct{}
type sessionData struct{ ID, CSRF string }

func tokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func (a *App) originAllowed(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	} // CLI clients have no Origin; mutations still need CSRF.
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if a.cfg.PublicURL != "" {
		expected, _ := url.Parse(a.cfg.PublicURL)
		return strings.EqualFold(u.Host, expected.Host) && u.Scheme == expected.Scheme
	}
	return strings.EqualFold(u.Host, r.Host) && (u.Scheme == "https" || u.Scheme == "http")
}
func (a *App) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(cookieName)
		if err != nil || len(cookie.Value) != 64 {
			apiError(w, 401, "请先登录")
			return
		}
		s := sessionData{ID: tokenHash(cookie.Value)}
		var expires int64
		var epoch string
		err = a.db.QueryRowContext(r.Context(), "SELECT csrf,expires_at,auth_epoch FROM sessions WHERE id=?", s.ID).Scan(&s.CSRF, &expires, &epoch)
		if err != nil || expires <= time.Now().Unix() || epoch != a.authEpoch {
			apiError(w, 401, "登录已过期，请重新登录")
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" && r.Method != "OPTIONS" {
			if !a.originAllowed(r) || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(s.CSRF)) != 1 {
				apiError(w, 403, "请求验证失败，请刷新页面")
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionKey{}, s)))
	})
}
func (a *App) login(w http.ResponseWriter, r *http.Request) {
	if !a.originAllowed(r) {
		apiError(w, 403, "不允许跨站登录")
		return
	}
	var input struct {
		Username string "json:\"username\""
		Password string "json:\"password\""
	}
	if !readJSON(w, r, &input) {
		return
	}
	if len(input.Password) > 72 || len(input.Username) > 100 {
		apiError(w, 401, "用户名或密码错误")
		return
	}
	now := time.Now()
	peer := a.loginPeer(r)
	a.loginMu.Lock()
	if a.loginBuckets == nil {
		a.loginBuckets = make(map[string]*loginBucket)
	}
	bucket := a.loginBuckets[peer]
	if bucket == nil {
		if len(a.loginBuckets) >= 2048 {
			var oldestKey string
			oldest := now
			for key, b := range a.loginBuckets {
				if b.window.Before(oldest) {
					oldest = b.window
					oldestKey = key
				}
			}
			delete(a.loginBuckets, oldestKey)
		}
		bucket = &loginBucket{window: now}
		a.loginBuckets[peer] = bucket
	}
	blocked := now.Before(bucket.blockedUntil)
	if now.Sub(bucket.window) > time.Minute {
		bucket.window = now
		bucket.failures = 0
	}
	a.loginMu.Unlock()
	if blocked {
		w.Header().Set("Retry-After", "60")
		apiError(w, 429, "尝试次数过多，请稍后再试")
		return
	}
	select {
	case a.loginSlot <- struct{}{}:
		defer func() { <-a.loginSlot }()
	default:
		w.Header().Set("Retry-After", "2")
		apiError(w, 429, "登录繁忙，请稍后重试")
		return
	}
	passwordOK := bcrypt.CompareHashAndPassword(a.passwordHash, []byte(input.Password)) == nil
	if !passwordOK || subtle.ConstantTimeCompare([]byte(input.Username), []byte(a.cfg.AdminUser)) != 1 {
		a.loginMu.Lock()
		bucket.failures++
		if bucket.failures >= 10 {
			bucket.blockedUntil = now.Add(time.Minute)
		}
		a.loginMu.Unlock()
		apiError(w, 401, "用户名或密码错误")
		return
	}
	a.loginMu.Lock()
	delete(a.loginBuckets, peer)
	a.loginMu.Unlock()
	token := randomHex(32)
	csrf := randomHex(32)
	expires := now.Add(time.Duration(a.cfg.SessionHours) * time.Hour)
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		apiError(w, 500, "登录失败")
		return
	}
	defer tx.Rollback()
	if _, err = tx.Exec("DELETE FROM sessions WHERE expires_at<=?", now.Unix()); err != nil {
		apiError(w, 500, "登录失败")
		return
	}
	if _, err = tx.Exec("INSERT INTO sessions(id,csrf,expires_at,auth_epoch) VALUES(?,?,?,?)", tokenHash(token), csrf, expires.Unix(), a.authEpoch); err != nil {
		apiError(w, 500, "登录失败")
		return
	}
	if err = tx.Commit(); err != nil {
		apiError(w, 500, "登录失败")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: token, Path: "/", HttpOnly: true, Secure: a.cfg.CookieSecure, SameSite: http.SameSiteStrictMode, Expires: expires, MaxAge: int(time.Until(expires).Seconds())})
	writeJSON(w, 200, map[string]string{"username": a.cfg.AdminUser, "csrf": csrf})
}
func (a *App) session(w http.ResponseWriter, r *http.Request) {
	s := r.Context().Value(sessionKey{}).(sessionData)
	writeJSON(w, 200, map[string]string{"username": a.cfg.AdminUser, "csrf": s.CSRF})
}
func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	s := r.Context().Value(sessionKey{}).(sessionData)
	if _, err := a.db.ExecContext(r.Context(), "DELETE FROM sessions WHERE id=?", s.ID); err != nil {
		apiError(w, 500, "退出失败")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true, Secure: a.cfg.CookieSecure, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

type loginBucket struct {
	failures     int
	window       time.Time
	blockedUntil time.Time
}

func (a *App) loginPeer(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	for _, cidr := range strings.Split(a.cfg.TrustedProxyCIDRs, ",") {
		_, network, err := net.ParseCIDR(strings.TrimSpace(cidr))
		if err != nil || !network.Contains(ip) {
			continue
		}
		// The bundled proxies replace untrusted incoming forwarding headers.
		forwarded := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
		if client := net.ParseIP(strings.TrimSpace(forwarded[len(forwarded)-1])); client != nil {
			return client.String()
		}
	}
	return host
}
