# Sidecar Probe Proxy Design

## Problem

A frozen container cannot respond to Kubernetes probes. The kubelet
sees an unresponsive container and restarts it, destroying the freeze.
Readiness probes also fail, but that side effect is desirable (remove
from traffic). The liveness failure is fatal to the design.

Kubernetes probes are per-container. One container's probes cannot
influence another container's restart decision. A sidecar container
cannot "answer liveness on behalf of" a sibling just by existing
alongside it.

## Solution

An Epoche sidecar container (`epoche-proxy`) that serves as the probe
target for the main container. The pod's liveness and readiness probes
point at the sidecar, which proxies to the main container's actual
health endpoints when unfrozen and returns canned responses when frozen.

The freeze mechanism (process suspension via signals) remains separate
and pluggable through the existing `Freezer` interface.

## Components

Three components with clear ownership boundaries.

| Component | Owns | Talks to |
|---|---|---|
| DecisionGateReconciler | Gate lifecycle (phases, timeout, decisions) | Sidecar (HTTP), Kubernetes API (exec, pod labels) |
| ExecFreezer (implements Freezer) | Process suspension/resumption | Kubernetes exec API, target container |
| epoche-proxy (sidecar) | Probe facade | Main container (health proxy), controller (management API) |

The `Freezer` interface stays pluggable. `ExecFreezer` is the default
implementation. Other implementations (CRI-based, cgroup-based,
DaemonSet-based) can replace it without affecting the sidecar or the
controller logic.

## Sidecar Design

### Endpoints

| Endpoint | Purpose | Caller |
|---|---|---|
| `POST /manage/freeze` | Switch to frozen mode | Controller |
| `POST /manage/unfreeze` | Switch to unfrozen mode | Controller |
| `GET /healthz` | Liveness probe target | Kubelet |
| `GET /readyz` | Readiness probe target | Kubelet |

### Behavior by state

| State | `/healthz` (liveness) | `/readyz` (readiness) |
|---|---|---|
| Unfrozen | Proxy to main container's health endpoint | Proxy to main container's readiness endpoint |
| Frozen | Return 200 (keep kubelet from killing) | Return 503 (remove from Service endpoints) |

When unfrozen, the sidecar is a transparent proxy. Normal probe
semantics are preserved. If the main container's health endpoint fails,
the sidecar forwards the failure. The only time the sidecar deviates
from standard behavior is during a freeze.

### Configuration

Command-line flags, since the sidecar is configured in the pod spec
next to the main container:

```
epoche-proxy \
  --liveness-upstream=http://localhost:8080/healthz \
  --readiness-upstream=http://localhost:8080/readyz \
  --manage-port=9901 \
  --probe-port=9902
```

Two ports separate management traffic (controller) from probe traffic
(kubelet), allowing different network policy rules.

### State recovery

On startup, the sidecar reads the pod's `epoche.dev/frozen` label via
the downward API (a file mounted from pod metadata). If the label is
`"true"`, the sidecar starts in frozen mode.

The pod label is the source of truth. The HTTP `POST /freeze` is the
fast path; the downward API file is the recovery path.

### Pod spec example

```yaml
spec:
  containers:
    - name: app
      image: myapp:latest
      # No liveness/readiness probes — they target the sidecar
    - name: epoche-proxy
      image: epoche.dev/proxy:v0.1
      args:
        - --liveness-upstream=http://localhost:8080/healthz
        - --readiness-upstream=http://localhost:8080/readyz
        - --manage-port=9901
        - --probe-port=9902
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
  volumes:
    - name: epoche-state
      downwardAPI:
        items:
          - path: frozen
            fieldRef:
              fieldPath: metadata.labels['epoche.dev/frozen']
```

## ExecFreezer

Default `Freezer` implementation using the Kubernetes exec API.

### Behavior

```
Freeze(ctx, namespace, pod, container):
  exec into container: kill -STOP 1

Unfreeze(ctx, namespace, pod, container):
  exec into container: kill -CONT 1
```

Uses SIGSTOP/SIGCONT (supervisory, Tier 3). The cooperative
SIGTSTP protocol is a follow-up once the SDK exists.

### RBAC

```go
// +kubebuilder:rbac:groups="",resources=pods/exec,verbs=create
```

### Limitations

The target container must have `kill` available in its filesystem.
Distroless images typically lack it. A future CRI-based freezer
implementation can avoid this constraint.

## Controller Integration

### Initialize flow (freeze)

```
1. Validate target pod exists and has container
2. Freezer.Freeze(ctx, namespace, pod, container)
3. POST http://<pod-ip>:9901/manage/freeze
4. Set pod label epoche.dev/frozen=true
5. Set status.phase = Pending, record FreezeTime
6. Notify escalation channels
```

Order matters. The process must be stopped (step 2) before the
sidecar switches to frozen mode (step 3). Otherwise the sidecar
reports "alive" while the upstream is temporarily unreachable during
the SIGSTOP delivery window. The label (step 4) is the durable record.

### ReconcileDecided flow (unfreeze)

```
1. POST http://<pod-ip>:9901/manage/unfreeze
2. Freezer.Unfreeze(ctx, namespace, pod, container)
3. Remove pod label epoche.dev/frozen
4. Set status.phase = Executed, record events
```

Reverse order. The sidecar resumes proxying (step 1) before the
process is resumed (step 2). When the process starts responding,
the sidecar is already forwarding probes to it.

### Reaching the sidecar

The controller reads `pod.Status.PodIP` and calls
`http://<pod-ip>:9901/manage/freeze`. In-cluster HTTP, no ingress.

### Sidecar is best-effort

If the sidecar management call fails (no sidecar deployed, network
issue, sidecar crashed), the controller logs a warning and continues.
The freeze still happens. The label still gets set. The sidecar
recovers its state from the label on restart.

This matches the existing pattern: `Notifier.Notify` failures do not
block the gate lifecycle.

### Failure modes

| Failure | Consequence | Recovery |
|---|---|---|
| Freeze exec fails | Gate transitions to Failed | None needed |
| Sidecar POST /freeze fails | Process frozen, probes unmanaged | Sidecar reads label on restart |
| Sidecar POST /unfreeze fails | Process resumed, sidecar reports not-ready | Sidecar sees label removed on restart |
| Pod label update fails | Return error, requeue | Controller retries |

## Deliverables

### New code

| File | What |
|---|---|
| `cmd/proxy/main.go` | Sidecar binary |
| `internal/controller/freezer_exec.go` | ExecFreezer implementation |
| `internal/controller/freezer_exec_test.go` | ExecFreezer tests |

### Modified code

| File | Change |
|---|---|
| `internal/controller/decisiongate_controller.go` | Sidecar HTTP call, pod label set/remove |
| `internal/controller/decisiongate_controller_test.go` | Tests for new flows |
| `cmd/main.go` | Wire ExecFreezer, pass clientset |

### New artifacts

| Artifact | What |
|---|---|
| `Dockerfile.proxy` | Multi-stage build for sidecar image |
| `config/samples/pod-with-sidecar.yaml` | Example pod spec |

## Out of Scope

- Mutating admission webhook for automatic sidecar injection (v1alpha1
  uses manual configuration)
- SIGTSTP cooperative protocol (SIGSTOP only for v1alpha1)
- Epoche SDK and pausable contexts
- FreezeBudget CRD
- Readiness gates (the sidecar's readiness probe handles traffic
  removal)

## Testing Strategy

- **ExecFreezer:** Unit tests with a mock exec client
- **Sidecar:** Go HTTP tests for probe behavior, management API,
  state recovery from downward API
- **Controller:** Existing envtest suite extended with mock sidecar
  (httptest.Server) and recording freezer
