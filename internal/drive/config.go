package drive

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	RequestsPerMinute    int
	RequestBurst         int
	MaxDownloadsPerIP    int
	MaxPublicDownloads   int
	MaxDownloadsPerShare int
	TrustedProxyCIDRs    string
	Addr                 string
	DataDir              string
	PublicURL            string
	AdminUser            string
	AdminPassword        string
	ReserveBytes         int64
	CookieSecure         bool
	SessionHours         int
}

func ConfigFromEnv() (Config, error) {
	c := Config{Addr: env("SOLODRIVE_ADDR", "127.0.0.1:8091"), DataDir: env("SOLODRIVE_DATA_DIR", "./data"), PublicURL: strings.TrimRight(os.Getenv("SOLODRIVE_PUBLIC_URL"), "/"), AdminUser: env("SOLODRIVE_ADMIN_USER", "admin"), ReserveBytes: 5 << 30, CookieSecure: true, SessionHours: 168}
	c = securityDefaults(c)
	for key, target := range map[string]*int{
		"SOLODRIVE_REQUESTS_PER_MINUTE": &c.RequestsPerMinute,
		"SOLODRIVE_REQUEST_BURST":       &c.RequestBurst,
		"SOLODRIVE_DOWNLOADS_PER_IP":    &c.MaxDownloadsPerIP,
		"SOLODRIVE_PUBLIC_DOWNLOADS":    &c.MaxPublicDownloads,
		"SOLODRIVE_DOWNLOADS_PER_SHARE": &c.MaxDownloadsPerShare,
	} {
		if value := os.Getenv(key); value != "" {
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 {
				return c, fmt.Errorf("%s must be a positive integer", key)
			}
			*target = n
		}
	}
	var err error
	if v := os.Getenv("SOLODRIVE_RESERVE_BYTES"); v != "" {
		c.ReserveBytes, err = strconv.ParseInt(v, 10, 64)
		if err != nil || c.ReserveBytes < 0 {
			return c, errors.New("SOLODRIVE_RESERVE_BYTES must be a nonnegative integer")
		}
	}
	if v := os.Getenv("SOLODRIVE_COOKIE_SECURE"); v != "" {
		c.CookieSecure, err = strconv.ParseBool(v)
		if err != nil {
			return c, err
		}
	}
	if v := os.Getenv("SOLODRIVE_SESSION_HOURS"); v != "" {
		c.SessionHours, err = strconv.Atoi(v)
		if err != nil || c.SessionHours < 1 || c.SessionHours > 8760 {
			return c, errors.New("SOLODRIVE_SESSION_HOURS must be between 1 and 8760")
		}
	}
	path := os.Getenv("SOLODRIVE_ADMIN_PASSWORD_FILE")
	if path == "" {
		return c, errors.New("set SOLODRIVE_ADMIN_PASSWORD_FILE to a file containing a password (12-72 bytes)")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return c, fmt.Errorf("read password file: %w", err)
	}
	c.AdminPassword = strings.TrimRight(string(b), "\r\n")
	c.TrustedProxyCIDRs = env("SOLODRIVE_TRUSTED_PROXY_CIDRS", "127.0.0.0/8,::1/128")
	return c, validateConfig(c)
}
func validateConfig(c Config) error {
	c = securityDefaults(c)
	if c.RequestsPerMinute < 1 || c.RequestsPerMinute > 60000 || c.RequestBurst < 1 || c.RequestBurst > 10000 || c.MaxDownloadsPerIP < 1 || c.MaxDownloadsPerIP > 256 || c.MaxPublicDownloads < 1 || c.MaxPublicDownloads > 512 || c.MaxDownloadsPerShare < 1 || c.MaxDownloadsPerShare > 256 {
		return errors.New("invalid request or download protection limits")
	}
	for _, cidr := range strings.Split(c.TrustedProxyCIDRs, ",") {
		if strings.TrimSpace(cidr) == "" {
			continue
		}
		if _, _, err := net.ParseCIDR(strings.TrimSpace(cidr)); err != nil {
			return fmt.Errorf("invalid trusted proxy CIDR: %w", err)
		}
	}
	if c.AdminUser == "" || len(c.AdminUser) > 100 {
		return errors.New("admin username is required (maximum 100 bytes)")
	}
	if len(c.AdminPassword) < 12 || len(c.AdminPassword) > 72 {
		return errors.New("admin password must be between 12 and 72 bytes")
	}
	if c.DataDir == "" || c.ReserveBytes < 0 {
		return errors.New("invalid storage configuration")
	}
	if c.SessionHours < 1 || c.SessionHours > 8760 {
		return errors.New("invalid session lifetime")
	}
	if c.PublicURL != "" {
		u, err := url.Parse(c.PublicURL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("SOLODRIVE_PUBLIC_URL must be an absolute HTTP(S) origin without path, credentials or query")
		}
		if c.CookieSecure && u.Scheme != "https" {
			return errors.New("secure cookies require an HTTPS public URL")
		}
	}
	return nil
}
func env(k, def string) string {
	if s := os.Getenv(k); s != "" {
		return s
	}
	return def
}

func securityDefaults(c Config) Config {
	if c.RequestsPerMinute == 0 {
		c.RequestsPerMinute = 600
	}
	if c.RequestBurst == 0 {
		c.RequestBurst = 60
	}
	if c.MaxDownloadsPerIP == 0 {
		c.MaxDownloadsPerIP = 16
	}
	if c.MaxPublicDownloads == 0 {
		c.MaxPublicDownloads = 48
	}
	if c.MaxDownloadsPerShare == 0 {
		c.MaxDownloadsPerShare = 32
	}
	return c
}
