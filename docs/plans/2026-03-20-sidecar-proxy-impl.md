# Sidecar Probe Proxy Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Implement the sidecar probe proxy, ExecFreezer, and controller integration from the [sidecar design](2026-03-20-sidecar-proxy-design.md).

**Architecture:** Three independent workstreams (ExecFreezer, sidecar binary, controller integration) that converge at the end. The ExecFreezer is a new `Freezer` implementation using `kubectl exec`-style signaling. The sidecar is a standalone binary that proxies liveness/readiness probes. The controller orchestrates both plus pod label management.

**Tech Stack:** Go 1.25, controller-runtime v0.23.1, client-go v0.35.0, Ginkgo/Gomega for controller tests, standard `testing` for sidecar and freezer tests.

---

## Task 1: ExecFreezer — Executor Interface and Implementation

**Files:**
- Create: `internal/controller/freezer_exec.go`
- Create: `internal/controller/freezer_exec_test.go`

**Step 1: Write the failing test**

Create `internal/controller/freezer_exec_test.go`:

```go
package controller

import (
	"context"
	"fmt"
	"testing"
)

type recordingExecutor struct {
	calls []execCall
	err   error
}

type execCall struct {
	Namespace, PodName, ContainerName string
	Command                           []string
}

func (e *recordingExecutor) Exec(_ context.Context, namespace, podName, containerName string, command []string) error {
	e.calls = append(e.calls, execCall{namespace, podName, containerName, command})
	return e.err
}

func TestExecFreezer_Freeze(t *testing.T) {
	executor := &recordingExecutor{}
	freezer := &ExecFreezer{Exec: executor}

	err := freezer.Freeze(context.Background(), "default", "my-pod", "app")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(executor.calls) != 1 {
		t.Fatalf("expected 1 exec call, got %d", len(executor.calls))
	}
	call := executor.calls[0]
	if call.Namespace != "default" || call.PodName != "my-pod" || call.ContainerName != "app" {
		t.Errorf("wrong target: %+v", call)
	}
	// Must send SIGSTOP (signal 19) to PID 1
	expected := []string{"kill", "-STOP", "1"}
	for i, v := range expected {
		if call.Command[i] != v {
			t.Errorf("command[%d] = %q, want %q", i, call.Command[i], v)
		}
	}
}

func TestExecFreezer_Unfreeze(t *testing.T) {
	executor := &recordingExecutor{}
	freezer := &ExecFreezer{Exec: executor}

	err := freezer.Unfreeze(context.Background(), "default", "my-pod", "app")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	call := executor.calls[0]
	expected := []string{"kill", "-CONT", "1"}
	for i, v := range expected {
		if call.Command[i] != v {
			t.Errorf("command[%d] = %q, want %q", i, call.Command[i], v)
		}
	}
}

func TestExecFreezer_PropagatesError(t *testing.T) {
	executor := &recordingExecutor{err: fmt.Errorf("exec failed")}
	freezer := &ExecFreezer{Exec: executor}

	err := freezer.Freeze(context.Background(), "default", "my-pod", "app")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err.Error() != "freeze container app in pod default/my-pod: exec failed" {
		t.Errorf("unexpected error message: %v", err)
	}
}
```

**Step 2: Run test to verify it fails**

Run: `cd /Users/aalpar/projects/epoche && go test ./internal/controller/ -run TestExecFreezer -v -count=1`

Expected: Compilation error — `ExecFreezer` and `Executor` not defined.

**Step 3: Write the implementation**

Create `internal/controller/freezer_exec.go`:

```go
package controller

import (
	"bytes"
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// Executor abstracts running a command inside a container.
type Executor interface {
	Exec(ctx context.Context, namespace, podName, containerName string, command []string) error
}

// ExecFreezer freezes and unfreezes containers by exec'ing kill signals.
type ExecFreezer struct {
	Exec Executor
}

func (f *ExecFreezer) Freeze(ctx context.Context, namespace, podName, containerName string) error {
	if err := f.Exec.Exec(ctx, namespace, podName, containerName, []string{"kill", "-STOP", "1"}); err != nil {
		return fmt.Errorf("freeze container %s in pod %s/%s: %w", containerName, namespace, podName, err)
	}
	return nil
}

func (f *ExecFreezer) Unfreeze(ctx context.Context, namespace, podName, containerName string) error {
	if err := f.Exec.Exec(ctx, namespace, podName, containerName, []string{"kill", "-CONT", "1"}); err != nil {
		return fmt.Errorf("unfreeze container %s in pod %s/%s: %w", containerName, namespace, podName, err)
	}
	return nil
}

// KubeExecutor implements Executor using the Kubernetes exec API.
type KubeExecutor struct {
	Client kubernetes.Interface
	Config *rest.Config
}

func (e *KubeExecutor) Exec(ctx context.Context, namespace, podName, containerName string, command []string) error {
	req := e.Client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: containerName,
			Command:   command,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(e.Config, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("create executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}); err != nil {
		return fmt.Errorf("exec %v: %s: %w", command, stderr.String(), err)
	}
	return nil
}
```

**Step 4: Run test to verify it passes**

Run: `cd /Users/aalpar/projects/epoche && go test ./internal/controller/ -run TestExecFreezer -v -count=1`

Expected: All 3 tests PASS.

**Step 5: Commit**

```bash
git add internal/controller/freezer_exec.go internal/controller/freezer_exec_test.go
git commit -m "Add ExecFreezer implementation using Kubernetes exec API"
```

---

## Task 2: Sidecar Proxy — Core Package

**Files:**
- Create: `internal/proxy/proxy.go`
- Create: `internal/proxy/proxy_test.go`

**Step 1: Write the failing tests**

Create `internal/proxy/proxy_test.go`:

```go
package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthz_Unfrozen_ProxiesToUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	p := New(Config{
		LivenessUpstream:  upstream.URL,
		ReadinessUpstream: upstream.URL,
	})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	p.ProbeHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("healthz status = %d, want 200", rec.Code)
	}
}

func TestHealthz_Unfrozen_ForwardsUpstreamFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	p := New(Config{LivenessUpstream: upstream.URL, ReadinessUpstream: upstream.URL})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	p.ProbeHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("healthz status = %d, want 503", rec.Code)
	}
}

func TestHealthz_Frozen_Returns200(t *testing.T) {
	// Upstream that would fail — but frozen mode should bypass it.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	p := New(Config{LivenessUpstream: upstream.URL, ReadinessUpstream: upstream.URL})
	p.SetFrozen(true)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	p.ProbeHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("healthz status = %d, want 200", rec.Code)
	}
}

func TestReadyz_Unfrozen_ProxiesToUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	p := New(Config{LivenessUpstream: upstream.URL, ReadinessUpstream: upstream.URL})

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	p.ProbeHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("readyz status = %d, want 200", rec.Code)
	}
}

func TestReadyz_Frozen_Returns503(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	p := New(Config{LivenessUpstream: upstream.URL, ReadinessUpstream: upstream.URL})
	p.SetFrozen(true)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	p.ProbeHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("readyz status = %d, want 503", rec.Code)
	}
}

func TestManageFreeze(t *testing.T) {
	p := New(Config{LivenessUpstream: "http://unused", ReadinessUpstream: "http://unused"})

	req := httptest.NewRequest(http.MethodPost, "/manage/freeze", nil)
	rec := httptest.NewRecorder()
	p.ManageHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("freeze status = %d, want 200", rec.Code)
	}
	if !p.Frozen() {
		t.Error("expected frozen=true after POST /manage/freeze")
	}
}

func TestManageUnfreeze(t *testing.T) {
	p := New(Config{LivenessUpstream: "http://unused", ReadinessUpstream: "http://unused"})
	p.SetFrozen(true)

	req := httptest.NewRequest(http.MethodPost, "/manage/unfreeze", nil)
	rec := httptest.NewRecorder()
	p.ManageHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("unfreeze status = %d, want 200", rec.Code)
	}
	if p.Frozen() {
		t.Error("expected frozen=false after POST /manage/unfreeze")
	}
}

func TestStateRecovery_FrozenFile(t *testing.T) {
	dir := t.TempDir()
	frozenFile := dir + "/frozen"
	if err := writeFile(frozenFile, "true"); err != nil {
		t.Fatal(err)
	}

	p := New(Config{
		LivenessUpstream:  "http://unused",
		ReadinessUpstream: "http://unused",
		StatePath:         frozenFile,
	})

	if !p.Frozen() {
		t.Error("expected frozen=true after reading state file containing 'true'")
	}
}

func TestStateRecovery_NoFile(t *testing.T) {
	p := New(Config{
		LivenessUpstream:  "http://unused",
		ReadinessUpstream: "http://unused",
		StatePath:         "/nonexistent/path",
	})

	if p.Frozen() {
		t.Error("expected frozen=false when state file doesn't exist")
	}
}

func writeFile(path, content string) error {
	return writeFileBytes(path, []byte(content))
}
```

Note: `writeFileBytes` is a small helper — use `os.WriteFile` directly in the test helper:

Replace the `writeFile` helper at the bottom with:

```go
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0644)
}
```

And add `"os"` to the imports.

**Step 2: Run test to verify it fails**

Run: `cd /Users/aalpar/projects/epoche && go test ./internal/proxy/ -v -count=1`

Expected: Compilation error — package `proxy` does not exist.

**Step 3: Write the implementation**

Create `internal/proxy/proxy.go`:

```go
package proxy

import (
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// Config holds the sidecar proxy configuration.
type Config struct {
	LivenessUpstream  string
	ReadinessUpstream string
	StatePath         string // path to downward API file for state recovery
}

// Proxy manages probe proxying and freeze state.
type Proxy struct {
	livenessUpstream  string
	readinessUpstream string
	frozen            atomic.Bool
	client            *http.Client
}

// New creates a Proxy and recovers state from the downward API file if present.
func New(cfg Config) *Proxy {
	p := &Proxy{
		livenessUpstream:  cfg.LivenessUpstream,
		readinessUpstream: cfg.ReadinessUpstream,
		client:            &http.Client{Timeout: 2 * time.Second},
	}
	if cfg.StatePath != "" {
		if data, err := os.ReadFile(cfg.StatePath); err == nil {
			if strings.TrimSpace(string(data)) == "true" {
				p.frozen.Store(true)
			}
		}
	}
	return p
}

// Frozen returns the current freeze state.
func (p *Proxy) Frozen() bool {
	return p.frozen.Load()
}

// SetFrozen sets the freeze state.
func (p *Proxy) SetFrozen(v bool) {
	p.frozen.Store(v)
}

// ProbeHandler returns an http.Handler for /healthz and /readyz.
func (p *Proxy) ProbeHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", p.handleHealthz)
	mux.HandleFunc("GET /readyz", p.handleReadyz)
	return mux
}

// ManageHandler returns an http.Handler for /manage/freeze and /manage/unfreeze.
func (p *Proxy) ManageHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /manage/freeze", p.handleFreeze)
	mux.HandleFunc("POST /manage/unfreeze", p.handleUnfreeze)
	return mux
}

func (p *Proxy) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if p.frozen.Load() {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "frozen: alive")
		return
	}
	p.proxyTo(w, r, p.livenessUpstream)
}

func (p *Proxy) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if p.frozen.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, "frozen: not ready")
		return
	}
	p.proxyTo(w, r, p.readinessUpstream)
}

func (p *Proxy) handleFreeze(w http.ResponseWriter, _ *http.Request) {
	p.frozen.Store(true)
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, "frozen")
}

func (p *Proxy) handleUnfreeze(w http.ResponseWriter, _ *http.Request) {
	p.frozen.Store(false)
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, "unfrozen")
}

func (p *Proxy) proxyTo(w http.ResponseWriter, _ *http.Request, upstream string) {
	resp, err := p.client.Get(upstream)
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, err.Error())
		return
	}
	defer resp.Body.Close()
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}
```

**Step 4: Run test to verify it passes**

Run: `cd /Users/aalpar/projects/epoche && go test ./internal/proxy/ -v -count=1`

Expected: All 9 tests PASS.

**Step 5: Commit**

```bash
git add internal/proxy/proxy.go internal/proxy/proxy_test.go
git commit -m "Add sidecar probe proxy package with management API and state recovery"
```

---

## Task 3: Sidecar Binary and Dockerfile

**Files:**
- Create: `cmd/proxy/main.go`
- Create: `Dockerfile.proxy`

**Step 1: Create the binary entry point**

Create `cmd/proxy/main.go`:

```go
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"

	"github.com/aalpar/epoche/internal/proxy"
)

func main() {
	var (
		livenessUpstream  string
		readinessUpstream string
		managePort        int
		probePort         int
		statePath         string
	)

	flag.StringVar(&livenessUpstream, "liveness-upstream", "", "URL of the main container's liveness endpoint")
	flag.StringVar(&readinessUpstream, "readiness-upstream", "", "URL of the main container's readiness endpoint")
	flag.IntVar(&managePort, "manage-port", 9901, "Port for management API")
	flag.IntVar(&probePort, "probe-port", 9902, "Port for probe endpoints")
	flag.StringVar(&statePath, "state-path", "/etc/epoche/frozen", "Path to downward API state file")
	flag.Parse()

	if livenessUpstream == "" || readinessUpstream == "" {
		log.Fatal("--liveness-upstream and --readiness-upstream are required")
	}

	p := proxy.New(proxy.Config{
		LivenessUpstream:  livenessUpstream,
		ReadinessUpstream: readinessUpstream,
		StatePath:         statePath,
	})

	log.Printf("Starting epoche-proxy (frozen=%v)", p.Frozen())
	log.Printf("  manage: :%d  probes: :%d", managePort, probePort)
	log.Printf("  liveness upstream:  %s", livenessUpstream)
	log.Printf("  readiness upstream: %s", readinessUpstream)

	errs := make(chan error, 2)
	go func() { errs <- http.ListenAndServe(fmt.Sprintf(":%d", managePort), p.ManageHandler()) }()
	go func() { errs <- http.ListenAndServe(fmt.Sprintf(":%d", probePort), p.ProbeHandler()) }()

	log.Fatal(<-errs)
}
```

**Step 2: Verify it compiles**

Run: `cd /Users/aalpar/projects/epoche && go build ./cmd/proxy/`

Expected: Binary builds with no errors.

**Step 3: Create the Dockerfile**

Create `Dockerfile.proxy`:

```dockerfile
FROM golang:1.25 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o epoche-proxy cmd/proxy/main.go

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/epoche-proxy .
USER 65532:65532

ENTRYPOINT ["/epoche-proxy"]
```

**Step 4: Commit**

```bash
git add cmd/proxy/main.go Dockerfile.proxy
git commit -m "Add epoche-proxy sidecar binary and Dockerfile"
```

---

## Task 4: Controller — Pod Label Management

**Files:**
- Modify: `internal/controller/decisiongate_controller.go`
- Modify: `internal/controller/decisiongate_controller_test.go`

This task adds setting `epoche.dev/frozen=true` on the target pod during
initialize and removing it during reconcileDecided. The sidecar HTTP
calls come in Task 5.

**Step 1: Write the failing test**

Add to `internal/controller/decisiongate_controller_test.go`, inside the
`Context("initialization", ...)` block, after the existing success test:

```go
It("should set the frozen label on the target pod", func() {
    podName := uniqueName("pod")
    gateName := uniqueName("gate")
    createPod(ctx, podName)
    createGate(ctx, gateName, podName)

    _, err := doReconcile(gateName)
    Expect(err).NotTo(HaveOccurred())

    // Re-fetch the pod and check the label.
    var pod corev1.Pod
    Expect(k8sClient.Get(ctx, types.NamespacedName{
        Name: podName, Namespace: "default",
    }, &pod)).To(Succeed())
    Expect(pod.Labels).To(HaveKeyWithValue("epoche.dev/frozen", "true"))
})
```

Add to the `Context("decided phase", ...)` block, inside the
"should unfreeze and transition to Executed" test, after verifying
`gate.Status.Phase` is Executed:

```go
// Verify frozen label was removed from pod.
var pod corev1.Pod
Expect(k8sClient.Get(ctx, types.NamespacedName{
    Name: podName, Namespace: "default",
}, &pod)).To(Succeed())
Expect(pod.Labels).NotTo(HaveKey("epoche.dev/frozen"))
```

**Step 2: Run test to verify it fails**

Run: `cd /Users/aalpar/projects/epoche && make test`

Expected: The two new assertions fail — label not set / not removed.

**Step 3: Implement pod label management**

Modify `internal/controller/decisiongate_controller.go`:

Update the RBAC marker for pods to include `patch`:

```go
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;patch
```

Add a helper method to set/remove the frozen label:

```go
func (r *DecisionGateReconciler) setPodFrozenLabel(ctx context.Context, namespace, podName string, frozen bool) error {
	var pod corev1.Pod
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, &pod); err != nil {
		return err
	}
	patch := client.MergeFrom(pod.DeepCopy())
	if pod.Labels == nil {
		pod.Labels = make(map[string]string)
	}
	if frozen {
		pod.Labels["epoche.dev/frozen"] = "true"
	} else {
		delete(pod.Labels, "epoche.dev/frozen")
	}
	return r.Patch(ctx, &pod, patch)
}
```

In `initialize`, after the `Freezer.Freeze` call and before setting
status, add:

```go
// Set frozen label on target pod.
if err := r.setPodFrozenLabel(ctx, gate.Namespace, gate.Spec.TargetRef.Name, true); err != nil {
    log.Error(err, "Failed to set frozen label on pod")
    // Best-effort — continue even if label update fails.
}
```

In `reconcileDecided`, before `Freezer.Unfreeze`, add:

```go
// Remove frozen label from target pod.
if err := r.setPodFrozenLabel(ctx, gate.Namespace, gate.Spec.TargetRef.Name, false); err != nil {
    log.Error(err, "Failed to remove frozen label from pod")
}
```

**Step 4: Run tests to verify they pass**

Run: `cd /Users/aalpar/projects/epoche && make test`

Expected: All tests PASS, including the new label assertions.

**Step 5: Regenerate RBAC manifests**

Run: `cd /Users/aalpar/projects/epoche && make manifests`

This regenerates `config/rbac/role.yaml` to include the new `patch`
verb on pods.

**Step 6: Commit**

```bash
git add internal/controller/decisiongate_controller.go \
        internal/controller/decisiongate_controller_test.go \
        config/rbac/role.yaml
git commit -m "Add pod frozen label management to controller"
```

---

## Task 5: Controller — Sidecar HTTP Client

**Files:**
- Modify: `internal/controller/decisiongate_controller.go`
- Modify: `internal/controller/decisiongate_controller_test.go`

This task adds the best-effort HTTP calls to the sidecar's management
API during initialize (POST /manage/freeze) and reconcileDecided
(POST /manage/unfreeze).

**Step 1: Write the failing test**

Add a `SidecarManagePort` field to the reconciler and a test that
verifies the sidecar was called. In
`internal/controller/decisiongate_controller_test.go`:

Add a mock sidecar server in the test setup:

```go
var (
    reconciler      *DecisionGateReconciler
    freezer         *recordingFreezer
    notifier        *recordingNotifier
    currentTime     time.Time
    sidecarServer   *httptest.Server
    sidecarFreezes  int
    sidecarUnfreezes int
)

BeforeEach(func() {
    currentTime = time.Date(2026, 3, 17, 12, 0, 0, 0, time.UTC)
    freezer = &recordingFreezer{}
    notifier = &recordingNotifier{}
    sidecarFreezes = 0
    sidecarUnfreezes = 0
    sidecarServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        switch r.URL.Path {
        case "/manage/freeze":
            sidecarFreezes++
        case "/manage/unfreeze":
            sidecarUnfreezes++
        }
        w.WriteHeader(http.StatusOK)
    }))
    reconciler = &DecisionGateReconciler{
        Client:           k8sClient,
        Scheme:           k8sClient.Scheme(),
        Freezer:          freezer,
        Notifier:         notifier,
        Now:              func() time.Time { return currentTime },
        SidecarManagePort: sidecarPort(sidecarServer),
    }
})

AfterEach(func() {
    sidecarServer.Close()
})
```

Add imports for `"net/http"`, `"net/http/httptest"`, and a helper:

```go
func sidecarPort(s *httptest.Server) int {
    // Extract port from httptest.Server URL.
    // URL format: http://127.0.0.1:<port>
    addr := s.Listener.Addr().String()
    // Parse host:port
    _, portStr, _ := net.SplitHostPort(addr)
    port, _ := strconv.Atoi(portStr)
    return port
}
```

Add imports for `"net"` and `"strconv"`.

Add test inside `Context("initialization", ...)`:

```go
It("should call the sidecar management API on freeze", func() {
    podName := uniqueName("pod")
    gateName := uniqueName("gate")
    createPod(ctx, podName)
    createGate(ctx, gateName, podName)

    _, err := doReconcile(gateName)
    Expect(err).NotTo(HaveOccurred())

    Expect(sidecarFreezes).To(Equal(1))
})
```

Add test inside `Context("decided phase", ...)`:

```go
It("should call the sidecar management API on unfreeze", func() {
    podName := uniqueName("pod")
    gateName := uniqueName("gate")
    createPod(ctx, podName)
    createGate(ctx, gateName, podName)

    // Initialize → Pending
    _, err := doReconcile(gateName)
    Expect(err).NotTo(HaveOccurred())

    // Provide response → Decided
    gate := getGate(ctx, gateName)
    gate.Spec.Response = &decisionsv1alpha1.Response{
        Action: "Continue", RespondedBy: "user:test",
    }
    Expect(k8sClient.Update(ctx, gate)).To(Succeed())
    _, err = doReconcile(gateName)
    Expect(err).NotTo(HaveOccurred())

    // Execute → Executed
    _, err = doReconcile(gateName)
    Expect(err).NotTo(HaveOccurred())

    Expect(sidecarUnfreezes).To(Equal(1))
})
```

**Step 2: Run test to verify it fails**

Run: `cd /Users/aalpar/projects/epoche && make test`

Expected: Compilation error — `SidecarManagePort` field does not exist.

**Step 3: Implement sidecar HTTP calls**

In `internal/controller/decisiongate_controller.go`, add a field to
the reconciler:

```go
type DecisionGateReconciler struct {
	client.Client
	Scheme            *runtime.Scheme
	Freezer           Freezer
	Notifier          Notifier
	Now               func() time.Time
	SidecarManagePort int // port for sidecar management API (0 = disabled)
}
```

Add the sidecar call method:

```go
func (r *DecisionGateReconciler) notifySidecar(ctx context.Context, pod *corev1.Pod, action string) {
	if r.SidecarManagePort == 0 || pod.Status.PodIP == "" {
		return
	}
	log := logf.FromContext(ctx)
	url := fmt.Sprintf("http://%s:%d/manage/%s", pod.Status.PodIP, r.SidecarManagePort, action)

	httpClient := &http.Client{Timeout: 3 * time.Second}
	resp, err := httpClient.Post(url, "", nil)
	if err != nil {
		log.Error(err, "Failed to notify sidecar", "action", action, "pod", pod.Name)
		return
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Info("Sidecar returned non-OK status", "action", action, "status", resp.StatusCode)
	}
}
```

Add `"net/http"` to imports.

In `initialize`, after `Freezer.Freeze` succeeds and before setting
status, add:

```go
r.notifySidecar(ctx, &pod, "freeze")
```

In `reconcileDecided`, before `Freezer.Unfreeze`, add:

```go
// Re-fetch the pod for its IP.
var pod corev1.Pod
if err := r.Get(ctx, types.NamespacedName{
    Namespace: gate.Namespace, Name: gate.Spec.TargetRef.Name,
}, &pod); err == nil {
    r.notifySidecar(ctx, &pod, "unfreeze")
}
```

**Step 4: Run tests to verify they pass**

Run: `cd /Users/aalpar/projects/epoche && make test`

Expected: All tests PASS.

Note: In envtest, pods don't have PodIP set (no kubelet). The sidecar
tests work because the test uses `httptest.Server` bound to 127.0.0.1,
and the `createPod` helper needs to be updated to set `pod.Status.PodIP`
to `"127.0.0.1"` after creation:

```go
func createPod(ctx context.Context, name string) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "busybox"},
			},
		},
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, pod)).To(Succeed())
	// Set PodIP for sidecar HTTP tests (envtest has no kubelet).
	pod.Status.PodIP = "127.0.0.1"
	ExpectWithOffset(1, k8sClient.Status().Update(ctx, pod)).To(Succeed())
}
```

**Step 5: Commit**

```bash
git add internal/controller/decisiongate_controller.go \
        internal/controller/decisiongate_controller_test.go
git commit -m "Add best-effort sidecar HTTP calls to controller"
```

---

## Task 6: Wire ExecFreezer and Update RBAC

**Files:**
- Modify: `cmd/main.go`
- Modify: `internal/controller/decisiongate_controller.go` (RBAC marker)

**Step 1: Add pods/exec RBAC marker**

In `internal/controller/decisiongate_controller.go`, add below the
existing RBAC markers:

```go
// +kubebuilder:rbac:groups="",resources=pods/exec,verbs=create
```

**Step 2: Update cmd/main.go to wire ExecFreezer**

In `cmd/main.go`, add a `--freezer` flag that selects the implementation.
Default to `"log"` (preserving current behavior) so the switch to exec
is explicit:

```go
var freezerType string
flag.StringVar(&freezerType, "freezer", "log",
    "Freezer implementation: 'log' (development) or 'exec' (production, requires pods/exec RBAC)")
```

After `flag.Parse()` and manager creation, build the freezer:

```go
var freezerImpl controller.Freezer
switch freezerType {
case "exec":
    clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
    if err != nil {
        setupLog.Error(err, "Failed to create clientset for ExecFreezer")
        os.Exit(1)
    }
    freezerImpl = &controller.ExecFreezer{
        Exec: &controller.KubeExecutor{
            Client: clientset,
            Config: mgr.GetConfig(),
        },
    }
    setupLog.Info("Using exec freezer")
default:
    freezerImpl = &controller.LogFreezer{}
    setupLog.Info("Using log freezer (development mode)")
}
```

Add import: `"k8s.io/client-go/kubernetes"`

Update the reconciler wiring:

```go
if err := (&controller.DecisionGateReconciler{
    Client:            mgr.GetClient(),
    Scheme:            mgr.GetScheme(),
    Freezer:           freezerImpl,
    Notifier:          &controller.LogNotifier{},
    SidecarManagePort: 9901,
}).SetupWithManager(mgr); err != nil {
```

**Step 3: Regenerate manifests and verify compilation**

Run: `cd /Users/aalpar/projects/epoche && make manifests && go build ./cmd/main.go`

Expected: Compiles successfully. `config/rbac/role.yaml` includes
`pods/exec` create permission.

**Step 4: Commit**

```bash
git add cmd/main.go \
        internal/controller/decisiongate_controller.go \
        config/rbac/role.yaml
git commit -m "Wire ExecFreezer with --freezer flag and add pods/exec RBAC"
```

---

## Task 7: Sample Pod Spec and Final Verification

**Files:**
- Create: `config/samples/pod-with-sidecar.yaml`

**Step 1: Create the sample**

Create `config/samples/pod-with-sidecar.yaml`:

```yaml
# Example: a pod configured with the epoche-proxy sidecar.
#
# The main container's probes are served by the sidecar. When frozen,
# the sidecar returns 200 for liveness (keep alive) and 503 for
# readiness (remove from traffic). When unfrozen, it proxies to the
# main container's actual health endpoints.
apiVersion: v1
kind: Pod
metadata:
  name: example-with-epoche-sidecar
  namespace: default
  labels:
    app: example
spec:
  containers:
    - name: app
      image: busybox
      command: ["sh", "-c", "while true; do echo ok; sleep 10; done"]
      # Health endpoint for the sidecar to proxy to.
      # In a real app, this would be your /healthz or /readyz endpoint.
      ports:
        - containerPort: 8080
    - name: epoche-proxy
      image: epoche.dev/proxy:latest
      args:
        - --liveness-upstream=http://localhost:8080/healthz
        - --readiness-upstream=http://localhost:8080/readyz
        - --manage-port=9901
        - --probe-port=9902
        - --state-path=/etc/epoche/frozen
      ports:
        - containerPort: 9901
          name: manage
        - containerPort: 9902
          name: probes
      livenessProbe:
        httpGet:
          path: /healthz
          port: probes
        periodSeconds: 10
        failureThreshold: 3
      readinessProbe:
        httpGet:
          path: /readyz
          port: probes
        periodSeconds: 5
        failureThreshold: 1
      volumeMounts:
        - name: epoche-state
          mountPath: /etc/epoche
          readOnly: true
  volumes:
    - name: epoche-state
      downwardAPI:
        items:
          - path: frozen
            fieldRef:
              fieldPath: metadata.labels['epoche.dev/frozen']
```

**Step 2: Run full test suite**

Run: `cd /Users/aalpar/projects/epoche && make test`

Expected: All tests PASS.

**Step 3: Run linter**

Run: `cd /Users/aalpar/projects/epoche && make lint-fix`

Expected: No errors (or auto-fixed).

**Step 4: Commit**

```bash
git add config/samples/pod-with-sidecar.yaml
git commit -m "Add sample pod spec with epoche-proxy sidecar"
```
