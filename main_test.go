package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func newTestApp() *app {
	return &app{log: slog.New(slog.NewTextHandler(io.Discard, nil)), self: identity{Pod: "harbour-7d9f-abcde", Namespace: "harbour", Node: "node0", Version: "v1"}}
}

func TestCheckDigit(t *testing.T) {
	// Reference example from ISO 6346: CSQU 305438 3.
	if got := checkDigit("CSQU305438"); got != 3 {
		t.Fatalf("CSQU305438: check digit %d, want 3", got)
	}
	for range 50 {
		c := newContainerCode()
		if len(c) != 11 || !strings.HasPrefix(c, "CKEU") || checkDigit(c[:10]) != int(c[10]-'0') {
			t.Fatalf("invalid container code %q", c)
		}
	}
}

func TestQuantities(t *testing.T) {
	cpu := map[string]int{"12345678n": 12, "2500u": 3, "250m": 250, "1": 1000, "": 0}
	for q, want := range cpu {
		if got := milliCPU(q); got != want {
			t.Errorf("milliCPU(%q) = %d, want %d", q, got, want)
		}
	}
	mem := map[string]int{"20480Ki": 20, "64Mi": 64, "1Gi": 1024, "10485760": 10}
	for q, want := range mem {
		if got := mebibytes(q); got != want {
			t.Errorf("mebibytes(%q) = %d, want %d", q, got, want)
		}
	}
	if v := flagValue([]string{"rush", "-url", "x", "-rps", "30"}, "-rps"); v != "30" {
		t.Errorf("flagValue = %q", v)
	}
}

func TestHealthAndReadiness(t *testing.T) {
	a := newTestApp()
	h := a.routes()
	status := func(path string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code
	}
	if s := status("/health"); s != 200 {
		t.Fatalf("/health = %d", s)
	}
	if s := status("/health/ready"); s != 503 {
		t.Fatalf("/health/ready before the ledger = %d, want 503", s)
	}
	a.ready.Store(true)
	if s := status("/health/ready"); s != 200 {
		t.Fatalf("/health/ready = %d", s)
	}
	a.draining.Store(true)
	if s := status("/health/ready"); s != 503 {
		t.Fatalf("/health/ready while draining = %d, want 503", s)
	}
	if s := status("/health/deep"); s != 503 {
		t.Fatalf("/health/deep without database = %d, want 503", s)
	}
}

func TestHeadersAndPage(t *testing.T) {
	h := newTestApp().routes()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/work", nil))
	if rec.Code != 200 || rec.Header().Get("Connection") != "close" || rec.Header().Get("X-Harbour-Crane") != "harbour-7d9f-abcde" {
		t.Fatalf("/api/work: %d %v", rec.Code, rec.Header())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Harbour") {
		t.Fatalf("/: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/unload", nil))
	if rec.Code != 503 {
		t.Fatalf("/api/unload without database = %d, want 503", rec.Code)
	}
}

// A fake Kubernetes API with the answers of a cluster in the middle of a rolling update.
func TestFleetFromKubernetesAPI(t *testing.T) {
	answers := map[string]string{
		"/api/v1/namespaces/harbour/pods": `{"items":[
			{"metadata":{"name":"harbour-aaa-1","labels":{"app.kubernetes.io/name":"harbour","pod-template-hash":"aaa"},"creationTimestamp":"2026-11-04T10:00:00Z","deletionTimestamp":"2026-11-04T10:05:00Z"},
			 "spec":{"nodeName":"node0"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"False"}],"containerStatuses":[{"imageID":"ttl.sh/x@sha256:1111111111111111","restartCount":0}]}},
			{"metadata":{"name":"harbour-bbb-2","labels":{"app.kubernetes.io/name":"harbour","pod-template-hash":"bbb"},"creationTimestamp":"2026-11-04T10:04:00Z"},
			 "spec":{"nodeName":"node0","containers":[{"resources":{"requests":{"cpu":"100m"}}}]},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}],"containerStatuses":[{"imageID":"ttl.sh/x@sha256:2222222222222222","restartCount":1}]}},
			{"metadata":{"name":"rush-hour-xyz","labels":{"app.kubernetes.io/name":"rush-hour"},"creationTimestamp":"2026-11-04T10:04:30Z"},
			 "spec":{"containers":[{"args":["rush","-url","http://harbour/api/work","-rps","30"]}]},"status":{"phase":"Running"}}]}`,
		"/apis/apps/v1/namespaces/harbour/deployments/harbour": `{"metadata":{"annotations":{"deployment.kubernetes.io/revision":"2"}},
			"spec":{"replicas":2,"template":{"spec":{"containers":[{"image":"ttl.sh/x@sha256:2222"}]}}},
			"status":{"replicas":3,"updatedReplicas":1,"readyReplicas":2,"availableReplicas":2}}`,
		"/apis/autoscaling/v2/namespaces/harbour/horizontalpodautoscalers/harbour": `{"spec":{"minReplicas":2,"maxReplicas":6,"metrics":[{"resource":{"target":{"averageUtilization":60}}}]},
			"status":{"currentReplicas":2,"desiredReplicas":3,"currentMetrics":[{"resource":{"current":{"averageUtilization":85}}}]}}`,
		"/api/v1/namespaces/harbour/events": `{"items":[
			{"metadata":{"creationTimestamp":"2026-11-04T10:04:00Z"},"type":"Normal","reason":"ScalingReplicaSet","message":"Scaled up replica set harbour-bbb from 0 to 1","lastTimestamp":"2026-11-04T10:04:00Z","involvedObject":{"kind":"Deployment","name":"harbour"}},
			{"metadata":{"creationTimestamp":"2026-11-04T10:05:00Z"},"type":"Normal","reason":"SuccessfulRescale","message":"New size: 3; reason: cpu resource utilization (percentage of request) above target","eventTime":"2026-11-04T10:05:00Z","involvedObject":{"kind":"HorizontalPodAutoscaler","name":"harbour"}}]}`,
		"/version": `{"gitVersion":"v1.36.0"}`,
		"/apis/metrics.k8s.io/v1beta1/namespaces/harbour/pods": `{"items":[{"metadata":{"name":"harbour-bbb-2"},"containers":[{"usage":{"cpu":"85000000n","memory":"20480Ki"}}]}]}`,
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := answers[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, body)
	}))
	defer api.Close()

	a := newTestApp()
	a.kube = &kube{base: api.URL, namespace: "harbour", client: api.Client()}
	rec := httptest.NewRecorder()
	a.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/fleet", nil))
	if rec.Code != 200 {
		t.Fatalf("/api/fleet = %d %s", rec.Code, rec.Body)
	}
	var f struct {
		fleet
		ServedBy identity `json:"servedBy"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Cranes) != 2 || !f.Cranes[0].Terminating || !f.Cranes[1].Ready || f.Cranes[1].Hash != "bbb" {
		t.Fatalf("cranes: %+v", f.Cranes)
	}
	if f.Cranes[1].CPU == nil || *f.Cranes[1].CPU != 85 || *f.Cranes[1].Memory != 20 || f.Cranes[1].Image != "sha256:22222222" {
		t.Fatalf("crane metrics: %+v", f.Cranes[1])
	}
	if f.Deployment == nil || !f.Deployment.Rolling || f.Deployment.Revision != "2" {
		t.Fatalf("deployment: %+v", f.Deployment)
	}
	if f.HPA == nil || f.HPA.Desired != 3 || *f.HPA.CPU != 85 || f.HPA.Target != 60 {
		t.Fatalf("hpa: %+v", f.HPA)
	}
	if f.Rush == nil || f.Rush.RPS != "30" || f.Rush.Running != 1 {
		t.Fatalf("rush: %+v", f.Rush)
	}
	if len(f.Events) != 2 || f.Events[0].Reason != "SuccessfulRescale" || f.Events[1].Object != "deployment/harbour" {
		t.Fatalf("events: %+v", f.Events)
	}
	if f.Cranes[1].Request != 100 || f.Kubernetes != "v1.36.0" {
		t.Fatalf("request=%d kubernetes=%q", f.Cranes[1].Request, f.Kubernetes)
	}
	if !f.Metrics || f.ServedBy.Pod != "harbour-7d9f-abcde" {
		t.Fatalf("metrics=%v servedBy=%+v", f.Metrics, f.ServedBy)
	}
}

// Needs a PostgreSQL: TEST_DATABASE_URL=postgres://… go test ./...
func TestLedgerRoundTrip(t *testing.T) {
	uri := os.Getenv("TEST_DATABASE_URL")
	if uri == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	l, err := openLedger(ctx, uri)
	if err != nil {
		t.Fatal(err)
	}
	defer l.pool.Close()
	for range 2 { // idempotent
		if err := l.migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	before, err := l.recent(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e, err := l.record(ctx, "harbour-test-crane", "v9", "node-test")
	if err != nil {
		t.Fatal(err)
	}
	if e.Goods == "" || e.Origin == "" || e.ID == 0 {
		t.Fatalf("entry: %+v", e)
	}
	after, err := l.recent(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Total != before.Total+1 || after.Recent[0].Container != e.Container || after.Version["v9"] < 1 {
		t.Fatalf("ledger did not record the container: before=%d after=%d", before.Total, after.Total)
	}

	a := newTestApp()
	a.ledger.Store(l)
	a.ready.Store(true)
	h := a.routes()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/unload", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"crane":"harbour-7d9f-abcde"`) {
		t.Fatalf("/api/unload = %d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health/deep", nil))
	if rec.Code != 200 {
		t.Fatalf("/health/deep = %d", rec.Code)
	}
}
