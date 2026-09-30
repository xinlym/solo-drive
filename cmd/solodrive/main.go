package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"solodrive/internal/drive"
	"solodrive/web"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		addr := os.Getenv("SOLODRIVE_ADDR")
		if addr == "" {
			addr = "127.0.0.1:8091"
		}
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			os.Exit(1)
		}
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		c := http.Client{Timeout: 3 * time.Second}
		res, err := c.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
		if err != nil {
			os.Exit(1)
		}
		res.Body.Close()
		if res.StatusCode != 200 {
			os.Exit(1)
		}
		return
	}
	cfg, err := drive.ConfigFromEnv()
	if err != nil {
		slog.Error("configuration", "error", err)
		os.Exit(1)
	}
	app, err := drive.New(cfg)
	if err != nil {
		slog.Error("startup", "error", err)
		os.Exit(1)
	}
	defer app.Close()
	app.SetStaticFS(web.Assets())
	server := &http.Server{Addr: cfg.Addr, Handler: app.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 32 << 10}
	// No whole-request ReadTimeout/WriteTimeout: large transfers may last hours.
	// tusd manages upload inactivity deadlines; the proxy manages idle downloads.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	slog.Info("SoloDrive started", "addr", cfg.Addr, "storage", cfg.DataDir, "secure_cookies", cfg.CookieSecure)
	select {
	case <-stop:
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err = server.Shutdown(ctx); err != nil {
			_ = server.Close()
		}
	case err = <-done:
		if err != nil && err != http.ErrServerClosed {
			slog.Error("serve", "error", err)
		}
	}
}
