# FreezeBudget CRD

Part of [Cross-Pod Freeze State Visibility](cross-pod-visibility.md).

## Resource Definition

A new custom resource that defines a freeze group and its constraints.
Named to parallel `PodDisruptionBudget`.

```yaml
apiVersion: decisions.epoche.dev/v1alpha1
kind: FreezeBudget
metadata:
  name: payments-freeze-budget
  namespace: production
spec:
  # Label selector defines the group (same semantics as PDB)
  selector:
    matchLabels:
      app: payments

  # PDB-aligned invariants. Mutually exclusive.
  minAvailable: 2
  # OR
  # maxUnavailable: 1

  # Default queue timeout ceiling for gates queued against this budget.
  # Gate-level queueTimeout can be shorter, not longer.
  queueTimeout: "5m"

status:
  expectedPods: 5
  currentHealthy: 4
  currentFrozen: 1
  freezesAllowed: 1
  observedGeneration: 3

  # The lateral visibility mechanism.
  # Equivalent to XCF group member state.
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

Namespace-scoped, same as PDB. Cross-namespace and cross-cluster
coordination are out of scope for v1alpha1.

## Spec Fields

**`selector`** — A standard Kubernetes label selector. Defines which pods
belong to this freeze group. Same semantics as PDB: the selector matches
pods, and the budget constrains how many of those pods can be frozen
simultaneously.

**`minAvailable`** / **`maxUnavailable`** — Mutually exclusive, same as
PDB. `minAvailable` sets the floor of pods that must remain unfrozen.
`maxUnavailable` sets the ceiling of pods that may be frozen. Can be an
integer or a percentage string (e.g., `"25%"`).

**`queueTimeout`** — The maximum duration a DecisionGate can remain in
the Queued phase when this budget is blocking it. Gate-level
`spec.queueTimeout` can be shorter (more urgent condition) but not longer
(the budget owner sets the ceiling). See [Queued Phase](queued-phase.md).

## Status Fields

**`expectedPods`** — Total pods matching the selector.

**`currentHealthy`** — Pods matching the selector that are not frozen.

**`currentFrozen`** — Pods matching the selector that are frozen.

**`freezesAllowed`** — How many more freezes the budget permits right now.
Computed from the invariant and current counts.

**`observedGeneration`** — Standard Kubernetes field for detecting stale
status.

**`frozenPods`** — List of currently frozen pods in this group. Each entry
includes the pod name, the owning DecisionGate, the freeze timestamp, and
the triggering condition (type and severity). This is the lateral
visibility mechanism — the equivalent of XCF group member state.

**`queuedGates`** — List of DecisionGates in the Queued phase that are
waiting for budget to open. Each entry includes the gate name, severity,
and queue timestamp. Enables operators and dashboards to see pending
demand.

## Grouping

### Implicit (default)

When no FreezeBudget matches a pod, the controller walks the owner chain:

```
Pod -> ReplicaSet (ownerRef) -> Deployment (ownerRef)
```

It finds all sibling pods (same ReplicaSet) and enforces a built-in
invariant: never freeze more than `floor(replicas/2)` siblings.

This is a safety net. No FreezeBudget required. Disabled per-gate with:

```yaml
metadata:
  annotations:
    epoche.dev/skip-implicit-budget: "true"
```

### Explicit (FreezeBudget)

A FreezeBudget's label selector defines the group. Its `minAvailable` or
`maxUnavailable` defines the constraint. This overrides the implicit
default for all pods matching the selector.

Explicit grouping handles cases implicit cannot:

| Case | Why implicit fails | Explicit solution |
|---|---|---|
| Pods across Deployments in the same Service | Different owner chains | Selector on shared labels |
| StatefulSet with leader/follower roles | All siblings look equal | Separate FreezeBudgets with different selectors |
| Cross-namespace coordination | Owner refs don't cross namespaces | Out of scope for v1alpha1 |

### Precedence

If both implicit and explicit apply to the same pod, explicit wins. The
implicit owner-chain default is suppressed for any pod covered by at
least one FreezeBudget.

Multiple FreezeBudgets can cover the same pod. The controller must
satisfy all of them. Most restrictive wins.

## Relationship to DecisionGate

The DecisionGate does not reference a FreezeBudget in its spec. The
binding is discovered at reconcile time via label selectors — same as
how PDB discovers pods. The gate targets a pod; the controller finds
all FreezeBudgets whose selectors match that pod.

This keeps DecisionGate creation simple. The entity creating the gate
(a human, a monitoring system, an admission webhook) doesn't need to
know about freeze budgets. It just says "freeze this pod." The
controller enforces constraints.

The FreezeBudget status references back to DecisionGates through the
`frozenPods[].gate` and `queuedGates[].name` fields.
