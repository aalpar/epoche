package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// helper: create an upstream server that returns the given status and body.
func newUpstream(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// helper: perform a GET against the handler and return the recorder.
func doGet(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// helper: perform a POST against the handler and return the recorder.
func doPost(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestHealthz_Unfrozen_ProxiesToUpstream(t *testing.T) {
	up := newUpstream(t, http.StatusOK, "ok from upstream")
	p := New(Config{LivenessUpstream: up.URL, ReadinessUpstream: up.URL})

	rec := doGet(t, p.ProbeHandler(), "/healthz")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if got := rec.Body.String(); got != "ok from upstream" {
		t.Fatalf("expected body %q, got %q", "ok from upstream", got)
	}
}

func TestHealthz_Unfrozen_ForwardsUpstreamFailure(t *testing.T) {
	up := newUpstream(t, http.StatusServiceUnavailable, "upstream down")
	p := New(Config{LivenessUpstream: up.URL, ReadinessUpstream: up.URL})

	rec := doGet(t, p.ProbeHandler(), "/healthz")

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	if got := rec.Body.String(); got != "upstream down" {
		t.Fatalf("expected body %q, got %q", "upstream down", got)
	}
}

func TestHealthz_Frozen_Returns200(t *testing.T) {
	// Upstream would fail, but frozen mode overrides.
	up := newUpstream(t, http.StatusServiceUnavailable, "upstream down")
	p := New(Config{LivenessUpstream: up.URL, ReadinessUpstream: up.URL})
	p.SetFrozen(true)

	rec := doGet(t, p.ProbeHandler(), "/healthz")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if got := rec.Body.String(); got != "frozen: alive" {
		t.Fatalf("expected body %q, got %q", "frozen: alive", got)
	}
}

func TestReadyz_Unfrozen_ProxiesToUpstream(t *testing.T) {
	up := newUpstream(t, http.StatusOK, "ready from upstream")
	p := New(Config{LivenessUpstream: up.URL, ReadinessUpstream: up.URL})

	rec := doGet(t, p.ProbeHandler(), "/readyz")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if got := rec.Body.String(); got != "ready from upstream" {
		t.Fatalf("expected body %q, got %q", "ready from upstream", got)
	}
}

func TestReadyz_Frozen_Returns503(t *testing.T) {
	// Upstream would pass, but frozen mode overrides.
	up := newUpstream(t, http.StatusOK, "ready from upstream")
	p := New(Config{LivenessUpstream: up.URL, ReadinessUpstream: up.URL})
	p.SetFrozen(true)

	rec := doGet(t, p.ProbeHandler(), "/readyz")

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	if got := rec.Body.String(); got != "frozen: not ready" {
		t.Fatalf("expected body %q, got %q", "frozen: not ready", got)
	}
}

func TestManageFreeze(t *testing.T) {
	p := New(Config{})

	rec := doPost(t, p.ManageHandler(), "/manage/freeze")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if got := rec.Body.String(); got != "frozen" {
		t.Fatalf("expected body %q, got %q", "frozen", got)
	}
	if !p.Frozen() {
		t.Fatal("expected Frozen() to be true after POST /manage/freeze")
	}
}

func TestManageUnfreeze(t *testing.T) {
	p := New(Config{})
	p.SetFrozen(true)

	rec := doPost(t, p.ManageHandler(), "/manage/unfreeze")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if got := rec.Body.String(); got != "unfrozen" {
		t.Fatalf("expected body %q, got %q", "unfrozen", got)
	}
	if p.Frozen() {
		t.Fatal("expected Frozen() to be false after POST /manage/unfreeze")
	}
}

func TestStateRecovery_FrozenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "frozen-state")
	if err := os.WriteFile(path, []byte("true\n"), 0644); err != nil {
		t.Fatal(err)
	}

	p := New(Config{StatePath: path})

	if !p.Frozen() {
		t.Fatal("expected Frozen() to be true when state file contains 'true'")
	}
}

func TestStateRecovery_NoFile(t *testing.T) {
	p := New(Config{StatePath: "/nonexistent/path/that/does/not/exist"})

	if p.Frozen() {
		t.Fatal("expected Frozen() to be false when state file does not exist")
	}
}
