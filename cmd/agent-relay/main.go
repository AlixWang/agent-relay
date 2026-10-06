// Command agent-relay is the single-binary server (DESIGN §10.1).
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/AlixWang/agent-relay/internal/auth"
	"github.com/AlixWang/agent-relay/internal/config"
	"github.com/AlixWang/agent-relay/internal/gateway"
	"github.com/AlixWang/agent-relay/internal/guard"
	"github.com/AlixWang/agent-relay/internal/presence"
	"github.com/AlixWang/agent-relay/internal/queue"
	"github.com/AlixWang/agent-relay/internal/retention"
	"github.com/AlixWang/agent-relay/internal/store"
	"github.com/AlixWang/agent-relay/internal/verify"
	"github.com/AlixWang/agent-relay/internal/web"
	"golang.org/x/crypto/bcrypt"
)

var (
	configPath = flag.String("config", "", "path to config.toml")
	hashPass   = flag.String("hash", "", "bcrypt-hash the given admin password and exit")
	showVer    = flag.Bool("version", false, "print version and exit")
)

const (
	serverVersion   = 2
	protocolVersion = 1
)

func main() {
	flag.Parse()
	if *showVer {
		fmt.Printf("agent-relay v%d (protocol %d)\n", serverVersion, protocolVersion)
		return
	}
	if *hashPass != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(*hashPass), bcrypt.DefaultCost)
		if err != nil {
			log.Fatalf("hash: %v", err)
		}
		fmt.Println(string(hash))
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatalf("data dir: %v", err)
	}

	dbPath := filepath.Join(cfg.DataDir, "agent-relay.db")
	st, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	au := auth.New(st, 3600)
	g := guard.New(st, guard.Limits{
		FuseMaxMessages: cfg.FuseMaxMessages,
		FuseMaxAgeSecs:  int64(cfg.FuseMaxAgeSecs),
		RatePerMinute:   cfg.RatePerMinute,
	})
	q := queue.New(st, g)
	p := presence.New(st, int64(cfg.OnlineTimeoutSecs), cfg.OfflineWebhookURL)
	v := verify.New(st, int64(cfg.VerifyTimeoutSecs))

	bind := cfg.BindAddr()
	scheme := "http"
	if cfg.Public {
		scheme = "https"
	}
	host := bind
	// bind is ip:port already.
	serverAddr := scheme + "://" + host

	gw := gateway.New(cfg, st, au, q, p, v, serverAddr)
	gw.SetGuard(g)
	if cfg.AdminPasswordHash != "" {
		gw.SetAdminHash([]byte(cfg.AdminPasswordHash))
	} else {
		// One-time random password (lost on restart — DESIGN §10.1).
		var b [18]byte
		_, _ = rand.Read(b[:])
		pw := base64.RawURLEncoding.EncodeToString(b[:])
		hash, _ := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
		gw.SetAdminHash(hash)
		log.Printf("WARN: admin_password_hash empty — one-time admin password: %s", pw)
	}

	handler := withLogging(gw.Handler(web.Handler()))

	srv := &http.Server{
		Addr:              bind,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Background loops.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go presenceLoop(ctx, p)
	go verifyLoop(ctx, v)
	go retentionLoop(ctx, st, cfg)

	log.Printf("agent-relay v%d listening on %s (db=%s)", serverVersion, bind, dbPath)
	go func() {
		var err error
		if cfg.Public {
			err = srv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
		} else {
			// Bind explicitly: net/http serves on Addr; tailnet-only by config.
			err = srv.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	log.Printf("shutdown complete")
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		log.Printf("%s %s %s %d %v", r.Method, r.URL.Path, clientAddr(r), sw.status, time.Since(start))
	})
}

func clientAddr(r *http.Request) string {
	if h := r.Header.Get("X-Forwarded-For"); h != "" {
		return h
	}
	return r.RemoteAddr
}

func presenceLoop(ctx context.Context, p *presence.Service) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.Sweep(time.Now().Unix())
		}
	}
}

func verifyLoop(ctx context.Context, v *verify.Service) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			v.Sweep(time.Now().Unix())
		}
	}
}

func retentionLoop(ctx context.Context, st store.Store, cfg *config.Config) {
	r := retention.New(st, retention.Policy{
		MessageTTLDays:     cfg.MessageTTLDays,
		PeerPruneAfterDays: cfg.PeerPruneAfterDays,
		AuditRetentionDays: cfg.AuditRetentionDays,
		DataDir:            cfg.DataDir,
	})
	t := time.NewTicker(6 * time.Hour)
	defer t.Stop()
	// Run once shortly after startup (delayed so boot isn't blocked).
	select {
	case <-ctx.Done():
		return
	case <-time.After(5 * time.Minute):
		if stats, err := r.RunOnce(time.Now().Unix()); err != nil {
			log.Printf("retention: %v", err)
		} else {
			log.Printf("retention: %+v", stats)
		}
		_ = st.Vacuum()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if stats, err := r.RunOnce(time.Now().Unix()); err != nil {
				log.Printf("retention: %v", err)
			} else {
				log.Printf("retention: %+v", stats)
			}
			_ = st.Vacuum()
		}
	}
}
