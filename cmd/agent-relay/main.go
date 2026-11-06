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
	"strings"
	"syscall"
	"time"

	"github.com/AlixWang/agent-relay/internal/auth"
	"github.com/AlixWang/agent-relay/internal/config"
	"github.com/AlixWang/agent-relay/internal/effects"
	"github.com/AlixWang/agent-relay/internal/gateway"
	"github.com/AlixWang/agent-relay/internal/guard"
	"github.com/AlixWang/agent-relay/internal/muse"
	"github.com/AlixWang/agent-relay/internal/presence"
	"github.com/AlixWang/agent-relay/internal/prompts"
	"github.com/AlixWang/agent-relay/internal/queue"
	"github.com/AlixWang/agent-relay/internal/resources"
	"github.com/AlixWang/agent-relay/internal/retention"
	"github.com/AlixWang/agent-relay/internal/store"
	"github.com/AlixWang/agent-relay/internal/stream"
	"github.com/AlixWang/agent-relay/internal/verify"
	"github.com/AlixWang/agent-relay/internal/web"
	"github.com/AlixWang/agent-relay/internal/work"
	"golang.org/x/crypto/bcrypt"
)

var (
	configPath = flag.String("config", "", "path to config.toml")
	hashPass   = flag.String("hash", "", "bcrypt-hash the given admin password and exit")
	showVer    = flag.Bool("version", false, "print version and exit")
	showHelper = flag.Bool("print-update-helper", false,
		"print the web-update helper script (deploy/install.sh extracts it) and exit")
)

const (
	serverVersion   = 2
	protocolVersion = 1
)

func main() {
	flag.Parse()
	if *showVer {
		// The release tag is what the update worker compares against before
		// installing anything (cmd/agent-relay/update-helper.sh).
		fmt.Printf("agent-relay v%d (protocol %d, release %s)\n", serverVersion, protocolVersion, web.AssetVersion())
		return
	}
	if *showHelper {
		fmt.Print(updateHelperScript)
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

	onlineSecs := int64(cfg.OnlineTimeoutSecs)
	if onlineSecs <= 0 {
		onlineSecs = 300
	}
	workSvc := work.New(st, onlineSecs)
	au := auth.New(st, 3600)
	permTTL := cfg.PermissionTTLSecs
	if permTTL <= 0 {
		permTTL = queue.DefaultPermissionTTL
	}
	permTTLMax := permTTL
	if permTTLMax > queue.MaxPermissionTTL {
		permTTLMax = queue.MaxPermissionTTL
	}
	queue.SetDefaultPermissionTTL(permTTLMax)
	g := guard.New(st, guard.LimitsFromConfig(cfg))
	_ = st.UpsertPolicyVersion(g.PolicyVersion(), g.PolicyRules(), time.Now().Unix())
	workSvc.SetPolicyVersion(g.PolicyVersion())
	fx := effects.New(st)
	fx.RegisterWebhook()
	if err := prompts.RecordTemplateRevisions(st, time.Now().Unix()); err != nil {
		log.Printf("WARN: prompt revisions: %v", err)
	}
	q := queue.New(st, g)

	// Resource hub (§4.12). A key failure disables only the hub: routes
	// answer 503 and heartbeat stays silent about resources. It also owns
	// secret redaction for message text and agent logs (P4).
	var resSvc *resources.Service
	if svc, err := resources.New(st, cfg.DataDir); err != nil {
		log.Printf("WARN: resource hub disabled: %v", err)
	} else {
		resSvc = svc
		resSvc.SetSubmissionPolicy(resources.SubmissionPolicy{
			Enabled:         cfg.ResourceSelfRegistration,
			PendingMax:      cfg.ResourcePendingMax,
			LiveMax:         cfg.ResourceLiveMax,
			PerHour:         cfg.ResourceSubmitPerHour,
			PendingTTLDays:  cfg.ResourcePendingTTLDays,
			NotifyBatchSecs: cfg.ResourceNotifyBatchSecs,
		})
		redactor := svc.Redactor()
		q.SetRedactor(redactor.Redact, func(sender string, names []string, now int64) {
			_ = st.AppendAudit(sender, "secret.redacted",
				fmt.Sprintf("secret=%s where=message", strings.Join(names, ",")), now)
		})
	}
	p := presence.New(st, int64(cfg.OnlineTimeoutSecs), cfg.OfflineWebhookURL)
	v := verify.New(st, int64(cfg.VerifyTimeoutSecs))

	// Optional Muse account authorization + keepalive (§4.9). A failure here
	// disables only the optional feature: the relay must still start.
	museSvc, museErr := muse.New(st, muse.Options{
		BaseURL:           cfg.MuseBaseURL,
		ProxyURL:          cfg.MuseProxyURL,
		HTTPTimeout:       time.Duration(cfg.MuseHTTPTimeoutSecs) * time.Second,
		KeepaliveInterval: time.Duration(cfg.MuseKeepaliveSecs) * time.Second,
		WakeCooldown:      time.Duration(cfg.MuseWakeCooldownSecs) * time.Second,
		DataDir:           cfg.DataDir,
		AutoWake:          cfg.MuseAutoWake,
	})
	if museErr != nil {
		museSvc = nil
		log.Printf("WARN: muse keepalive disabled: %v", museErr)
	}
	if museSvc != nil {
		p.OnOffline = func(peerID string, offlineSecs int64) {
			museSvc.MaybeWakeOffline(peerID, offlineSecs)
		}
	}

	bind := cfg.BindAddr()
	// serverAddr is what assistants see in onboarding prompts. Behind a
	// reverse proxy it is the public address, not the local bind.
	serverAddr := cfg.PublicAddr
	if serverAddr == "" {
		scheme := "http"
		if cfg.Public {
			scheme = "https"
		}
		// bind is ip:port already.
		serverAddr = scheme + "://" + bind
	}

	gw := gateway.New(cfg, st, au, q, p, v, serverAddr)
	gw.SetGuard(g)
	gw.SetWork(workSvc)
	gw.SetEffects(fx)
	// SSE fan-out (DESIGN §4.4b): in-memory wake-ups, DB stays authoritative.
	gw.SetStream(stream.New(cfg.StreamMaxPerPeer, 500))
	gw.SetMuse(museSvc)
	if resSvc != nil {
		gw.SetResources(resSvc)
		gw.AgentLogs().SetRedactor(resSvc.Redactor().Redact, func(peerID string, names []string) {
			_ = st.AppendAudit(peerID, "secret.redacted",
				fmt.Sprintf("secret=%s where=logs", strings.Join(names, ",")), time.Now().Unix())
		})
	}
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

	// A web update restarts this process in the middle of its own job: book the
	// outcome of whatever the helper finished while we were down (§10.4).
	gw.ReconcileUpdateJobs()

	srv := &http.Server{
		Addr:              bind,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// No WriteTimeout: SSE streams (/messages/stream) are held open
		// for hours; the 30s cap would silently kill them. Slow-client
		// protection comes from the hub's per-subscriber buffer cap.
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}

	// Background loops.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	workSvc.Start(ctx)
	fx.Start(ctx)
	go presenceLoop(ctx, p)
	go verifyLoop(ctx, v)
	go retentionLoop(ctx, st, cfg, resSvc)
	if museSvc != nil && cfg.MuseKeepaliveSecs > 0 {
		go museLoop(ctx, museSvc, cfg)
	}

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

// Flush keeps SSE streaming working through the logging middleware:
// without it the gateway's Flusher assertion fails and streams 500.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
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

// museLoop renews authorized Muse sessions and keeps their VMs warm. It waits
// 15s after boot so the relay is serving before optional outbound traffic, then
// runs on the configured interval. Zero interval never reaches here.
func museLoop(ctx context.Context, m *muse.Service, cfg *config.Config) {
	interval := time.Duration(cfg.MuseKeepaliveSecs) * time.Second
	if interval <= 0 {
		return
	}
	run := func() {
		sum, err := m.KeepaliveAll(ctx, false)
		if err != nil {
			log.Printf("muse: keepalive: %v", err)
			return
		}
		log.Printf("muse: keepalive total=%d ok=%d failed=%d skipped=%d needs_reauth=%d wake_ok=%d",
			sum.Total, sum.OK, sum.Failed, sum.Skipped, sum.NeedsReauth, sum.WakeOK)
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(15 * time.Second):
	}
	run()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

func retentionLoop(ctx context.Context, st store.Store, cfg *config.Config, resSvc *resources.Service) {
	r := retention.New(st, retention.Policy{
		MessageTTLDays:         cfg.MessageTTLDays,
		PeerPruneAfterDays:     cfg.PeerPruneAfterDays,
		AuditRetentionDays:     cfg.AuditRetentionDays,
		AgentLogRetentionDays:  cfg.AgentLogRetentionDays,
		AgentLogMaxRowsPerPeer: cfg.AgentLogMaxRowsPerPeer,
		PermissionTTLSecs:      cfg.PermissionTTLSecs,
		DataDir:                cfg.DataDir,
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
		if resSvc != nil {
			if n, err := resSvc.ExpirePendingSubmissions(); err != nil {
				log.Printf("resources: expire submissions: %v", err)
			} else if n > 0 {
				log.Printf("resources: expired %d pending submissions", n)
			}
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
			if resSvc != nil {
				if n, err := resSvc.ExpirePendingSubmissions(); err != nil {
					log.Printf("resources: expire submissions: %v", err)
				} else if n > 0 {
					log.Printf("resources: expired %d pending submissions", n)
				}
			}
			_ = st.Vacuum()
		}
	}
}
