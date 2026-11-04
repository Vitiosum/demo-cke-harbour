// Harbour: a team's own Go service on Clever Kubernetes Engine (CKE).
// Every pod is a crane, every request a container, and the ledger lives in a
// Clever Cloud managed PostgreSQL outside the cluster.
package main

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// version is baked into the image at build time: VERSION=v2 ko build (see .ko.yaml).
var version = "dev"

const appName = "harbour"

//go:embed web
var webFS embed.FS

type identity struct {
	Pod       string `json:"pod"`
	Namespace string `json:"namespace"`
	Node      string `json:"node"`
	Version   string `json:"version"`
	Go        string `json:"go"`
}

type app struct {
	self     identity
	ledger   atomic.Pointer[ledger] // nil when no database is configured (local preview)
	kube     *kube                  // nil outside a cluster
	ready    atomic.Bool
	draining atomic.Bool
	log      *slog.Logger
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "rush" {
		os.Exit(rush(os.Args[2:]))
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if version == "" || strings.Contains(version, "<") {
		version = "dev"
	}
	host, _ := os.Hostname()
	a := &app{log: log, self: identity{
		Pod:       getenv("POD_NAME", host),
		Namespace: getenv("POD_NAMESPACE", "local"),
		Node:      getenv("NODE_NAME", "laptop"),
		Version:   version,
		Go:        runtime.Version(),
	}}
	k, err := newKubeInCluster()
	if err != nil {
		log.Warn("kubernetes API unavailable", "err", err)
	}
	a.kube = k

	srv := &http.Server{
		Addr:              ":" + getenv("PORT", "8080"),
		Handler:           a.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Info("crane on duty", "pod", a.self.Pod, "version", version, "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server", "err", err)
			os.Exit(1)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	go a.openLedger(ctx, os.Getenv("POSTGRESQL_ADDON_URI"))

	<-ctx.Done()
	// Kubernetes already removed this pod from the Service endpoints during the
	// preStop sleep (k8s/20-deployment.yaml). Finish the requests in flight, then exit.
	a.draining.Store(true)
	log.Info("SIGTERM received, finishing requests in flight", "pod", a.self.Pod)
	shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		log.Warn("shutdown", "err", err)
	}
	if l := a.ledger.Load(); l != nil {
		l.pool.Close()
	}
	log.Info("crane off duty", "pod", a.self.Pod)
}

// openLedger connects to PostgreSQL and migrates, retrying until it works.
// The pod only becomes Ready (and receives traffic) once this has succeeded.
func (a *app) openLedger(ctx context.Context, uri string) {
	if uri == "" {
		a.log.Warn("POSTGRESQL_ADDON_URI is not set: the ledger is off")
		a.ready.Store(true)
		return
	}
	for attempt := 1; ctx.Err() == nil; attempt++ {
		l, err := openLedger(ctx, uri)
		if err == nil {
			mctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err = l.migrate(mctx)
			cancel()
			if err == nil {
				a.ledger.Store(l)
				a.ready.Store(true)
				a.log.Info("ledger ready", "attempt", attempt)
				return
			}
			l.pool.Close()
		}
		a.log.Warn("ledger not reachable yet", "attempt", attempt, "err", err)
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
		}
	}
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", http.FileServerFS(static))

	mux.HandleFunc("POST /api/unload", a.unload)
	mux.HandleFunc("GET /api/ledger", a.ledgerView)
	mux.HandleFunc("GET /api/fleet", a.fleet)
	mux.HandleFunc("GET /api/whoami", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, a.self) })
	mux.HandleFunc("GET /api/work", a.work)

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /health/ready", a.readiness)
	mux.HandleFunc("GET /health/deep", a.deep)
	return a.headers(mux)
}

// headers tags every response with the crane (pod) that served it.
// API responses close the connection, so each call is balanced again by the
// Service: the page sees every pod, and a pod that leaves keeps no client.
func (a *app) headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Harbour-Crane", a.self.Pod)
		h.Set("X-Harbour-Version", a.self.Version)
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' https://fonts.googleapis.com; font-src https://fonts.gstatic.com; img-src 'self' data:; script-src 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/health") {
			h.Set("Connection", "close")
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// unload: one container off the ship, written to the ledger by this crane.
func (a *app) unload(w http.ResponseWriter, r *http.Request) {
	l := a.ledger.Load()
	if l == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "the ledger is not connected"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	e, err := l.record(ctx, a.self.Pod, a.self.Version, a.self.Node)
	if err != nil {
		a.log.Warn("unload", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "the ledger did not answer"})
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (a *app) ledgerView(w http.ResponseWriter, r *http.Request) {
	l := a.ledger.Load()
	if l == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "the ledger is not connected"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	v, err := l.recent(ctx)
	if err != nil {
		a.log.Warn("ledger", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "the ledger did not answer"})
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// fleet: what Kubernetes says about the cranes, the rollout, the autoscaler and recent events.
func (a *app) fleet(w http.ResponseWriter, r *http.Request) {
	type response struct {
		fleet
		ServedBy identity `json:"servedBy"`
	}
	if a.kube == nil {
		self := crane{Name: a.self.Pod, Node: a.self.Node, Phase: "Running", Ready: a.ready.Load(), Started: time.Now()}
		writeJSON(w, http.StatusOK, response{fleet{Namespace: a.self.Namespace, Cranes: []crane{self}, Events: []event{}, Local: true, At: time.Now()}, a.self})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	f, err := a.kube.fleet(ctx, appName)
	if err != nil {
		a.log.Warn("kubernetes API", "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "the Kubernetes API did not answer"})
		return
	}
	writeJSON(w, http.StatusOK, response{f, a.self})
}

// work burns about 10 ms of CPU: what the rush-hour Job calls to wake up the autoscaler.
func (a *app) work(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	buf := make([]byte, 4096)
	rounds := 0
	for time.Since(start) < 10*time.Millisecond {
		sum := sha256.Sum256(buf)
		copy(buf, sum[:])
		rounds++
	}
	writeJSON(w, http.StatusOK, map[string]any{"crane": a.self.Pod, "version": a.self.Version, "rounds": rounds})
}

// readiness: Kubernetes sends traffic to this pod only when the ledger is migrated,
// and stops as soon as the pod is shutting down.
func (a *app) readiness(w http.ResponseWriter, r *http.Request) {
	switch {
	case a.draining.Load():
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "draining"})
	case !a.ready.Load():
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "starting"})
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	}
}

// deep: checks the managed PostgreSQL right now.
func (a *app) deep(w http.ResponseWriter, r *http.Request) {
	l := a.ledger.Load()
	if l == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"postgresql": "not connected"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := l.ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"postgresql": "unreachable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"postgresql": "ok"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func getenv(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
