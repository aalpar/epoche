# Freeze Visibility Layers

Part of [Cross-Pod Freeze State Visibility](cross-pod-visibility.md).

## Three Layers, One Source of Truth

Freeze state is observable at three levels of granularity. Each layer
serves a different consumer category.

| Layer | Mechanism | Consumer |
|---|---|---|
| Group-level | FreezeBudget `.status` (watch/list) | Other controllers, dashboards, Epoche controller |
| Pod-level | Pod label `epoche.dev/frozen: "true"` | Any controller, kubectl, service mesh |
| Application-level | Epoche SDK `OnPeerFrozen` callback | Peer pods using the SDK |

## Group-Level: FreezeBudget Status

The FreezeBudget status provides the z/OS XCF equivalent: which group
members are frozen, since when, for what condition, and which
DecisionGate owns the freeze.

```yaml
status:
  expectedPods: 5
  currentHealthy: 4
  currentFrozen: 1
  freezesAllowed: 1

  frozenPods:
    - name: payments-7b4f9-xk2q9
      gate: payments-oom-gate-001
      frozenSince: "2026-03-18T14:22:00Z"
      condition:
        type: ResourcePressure
        severity: Critical

  queuedGates:
    - name: payments-leak-gate-002
      severity: Warning
      queuedSince: "2026-03-18T14:25:00Z"
```

Any controller watching FreezeBudget resources can distinguish "1 of 5
pods is deliberately frozen for deliberation" from "1 of 5 pods
crashed." The `queuedGates` list shows pending demand — freezes that
are warranted but blocked by budget constraints.

### Consumers

- **Epoche controller:** Reads `freezesAllowed` before freezing. See
  [Freeze Enforcement](freeze-enforcement.md).
- **Dashboards/monitoring:** Watch FreezeBudget status for operational
  awareness. "The payments group has 1 active freeze and 1 queued."
- **Custom operators:** Any operator can watch FreezeBudgets and react
  to group-level freeze state. For example, an HPA-aware controller
  could suppress scale-up when freezes are active.

## Pod-Level: Labels

```
epoche.dev/frozen: "true"
```

Set by the DecisionGateReconciler when freezing a pod, removed when
unfreezing. This is the simplest, most broadly compatible signal.

### Consumers

- **kubectl:** `kubectl get pods -l epoche.dev/frozen=true` shows all
  frozen pods in a namespace.
- **Service mesh:** Istio DestinationRule or similar can use label
  selectors to exclude frozen pods from traffic routing, independent
  of the readiness gate.
- **Any controller:** Controllers that watch pods (node-level agents,
  log collectors, metric scrapers) can check for this label without
  knowing about Epoche CRDs.
- **PodDisruptionBudget interaction:** A PDB could use a selector
  that excludes `epoche.dev/frozen=true` pods, though the interaction
  semantics need careful consideration (out of scope for v1alpha1).

## Application-Level: SDK Callbacks

The Epoche SDK provides a watcher over FreezeBudget status:

```go
epoche.OnPeerFrozen(func(event epoche.PeerFreezeEvent) {
    // A peer pod was frozen. Adjust behavior:
    // stop sending work, absorb its partition, etc.
})
```

### How It Works

The SDK watches FreezeBudget resources whose selectors match the
current pod's labels. When `status.frozenPods` changes, callbacks fire
with a `PeerFreezeEvent` containing:

- Which peer was frozen/unfrozen
- The triggering condition (type, severity)
- The owning DecisionGate name
- Whether this is a freeze or unfreeze event

### Use Cases

- **Kafka consumer groups:** When a peer is frozen, proactively absorb
  its partitions rather than waiting for the consumer group protocol's
  `session.timeout.ms` to fire and trigger a rebalance.
- **Work distribution:** Stop sending work to a frozen peer's queue.
  Redistribute in-flight work to healthy peers.
- **Connection management:** Close connections to the frozen peer's
  sidecar rather than accumulating timeouts.
- **Coordinated behavior:** If the application has a leader-follower
  pattern, a follower freeze might trigger leader awareness without
  requiring a full election cycle.

### Scope

The SDK callback implementation is out of scope for v1alpha1. The CRD
and status shape enable it. This section documents the intended
interface so the CRD design supports it without changes.
