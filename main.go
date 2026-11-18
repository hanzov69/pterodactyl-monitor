package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed web/index.html
var indexHTML []byte

type Config struct {
	PublicURL     string
	LANURL        string
	APIURL        string
	APIKey        string
	ClientKey     string
	ServerChecks  map[string]ProbeSpec
	StartGrace    time.Duration
	LANInsecure   bool
	NodeInsecure  bool
	Interval      time.Duration
	Timeout       time.Duration
	FailThreshold int
	DataDir       string
	Listen        string
	DashboardURL  string
	SMTP          SMTPConfig
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envBool(key string) bool {
	b, _ := strconv.ParseBool(env(key, "false"))
	return b
}

func envDuration(key, def string) time.Duration {
	d, err := time.ParseDuration(env(key, def))
	if err != nil || d <= 0 {
		log.Fatalf("%s: invalid duration %q", key, os.Getenv(key))
	}
	return d
}

func loadConfig() Config {
	c := Config{
		PublicURL:    strings.TrimRight(env("PANEL_PUBLIC_URL", ""), "/"),
		LANURL:       strings.TrimRight(env("PANEL_LAN_URL", ""), "/"),
		APIKey:       env("PANEL_API_KEY", ""),
		ClientKey:    env("PANEL_CLIENT_KEY", ""),
		StartGrace:   envDuration("START_GRACE", "15m"),
		LANInsecure:  envBool("LAN_TLS_SKIP_VERIFY"),
		NodeInsecure: envBool("NODE_TLS_SKIP_VERIFY"),
		Interval:     envDuration("CHECK_INTERVAL", "60s"),
		Timeout:      envDuration("CHECK_TIMEOUT", "10s"),
		DataDir:      env("DATA_DIR", "/data"),
		Listen:       env("LISTEN_ADDR", ":8080"),
		DashboardURL: env("DASHBOARD_URL", ""),
		SMTP: SMTPConfig{
			Host:     env("SMTP_HOST", ""),
			Port:     env("SMTP_PORT", "587"),
			Username: env("SMTP_USERNAME", ""),
			Password: os.Getenv("SMTP_PASSWORD"),
			From:     env("SMTP_FROM", ""),
			TLS:      strings.ToLower(env("SMTP_TLS", "starttls")),
		},
	}
	for _, to := range strings.Split(env("SMTP_TO", ""), ",") {
		if to = strings.TrimSpace(to); to != "" {
			c.SMTP.To = append(c.SMTP.To, to)
		}
	}
	c.APIURL = strings.TrimRight(env("PANEL_API_URL", c.LANURL), "/")
	if c.APIURL == "" {
		c.APIURL = c.PublicURL
	}
	n, err := strconv.Atoi(env("FAIL_THRESHOLD", "3"))
	if err != nil || n < 1 {
		log.Fatalf("FAIL_THRESHOLD: must be a positive integer")
	}
	c.FailThreshold = n
	if c.PublicURL == "" && c.LANURL == "" {
		log.Fatal("set PANEL_PUBLIC_URL and/or PANEL_LAN_URL")
	}
	checks, err := parseServerChecks(os.Getenv("SERVER_CHECKS"))
	if err != nil {
		log.Fatalf("SERVER_CHECKS: %v", err)
	}
	c.ServerChecks = checks
	if c.APIKey == "" {
		log.Print("warning: PANEL_API_KEY not set; nodes will not be monitored")
	}
	if c.ClientKey != "" {
		log.Print("server monitoring enabled (PANEL_CLIENT_KEY is set)")
	}
	if !c.SMTP.Enabled() {
		log.Print("warning: SMTP_HOST/SMTP_FROM/SMTP_TO not all set; email notifications disabled")
	}
	return c
}

func main() {
	testEmail := flag.Bool("test-email", false, "send a test email using the SMTP settings and exit")
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.LUTC)
	cfg := loadConfig()

	if *testEmail {
		if !cfg.SMTP.Enabled() {
			log.Fatal("SMTP is not configured")
		}
		err := cfg.SMTP.Send("[pterodactyl-monitor] Test email",
			"This is a test email from pterodactyl-monitor. Notifications are working.\n")
		if err != nil {
			log.Fatalf("test email failed: %v", err)
		}
		fmt.Println("test email sent to", strings.Join(cfg.SMTP.To, ", "))
		return
	}

	m := NewMonitor(cfg)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go m.Run(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(m.Snapshot(r.URL.Query().Get("range")))
	})
	mux.HandleFunc("POST /api/alerts", func(w http.ResponseWriter, r *http.Request) {
		// Requiring a JSON content type keeps other websites from flipping
		// switches through a visitor's browser (it forces a CORS preflight).
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			http.Error(w, "content type must be application/json", http.StatusUnsupportedMediaType)
			return
		}
		var req struct {
			ID      string `json:"id"`
			Enabled *bool  `json:"enabled"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.ID == "" || req.Enabled == nil {
			http.Error(w, `expected {"id": "...", "enabled": true|false}`, http.StatusBadRequest)
			return
		}
		if !m.SetAlerts(req.ID, *req.Enabled) {
			http.Error(w, "unknown target", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if !m.Healthy() {
			http.Error(w, "check loop stalled", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok\n"))
	})

	srv := &http.Server{Addr: cfg.Listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	log.Printf("listening on %s; checking every %s", cfg.Listen, cfg.Interval)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	m.Save()
}
