# Cross-Pod Freeze State Visibility

## Problem

A frozen pod's state is visible to the Kubernetes control plane (readiness
gates, annotations, webhook interception) but not to three categories of
consumers that need it:

1. **The Epoche controller itself.** Nothing prevents multiple DecisionGates
   from freezing most replicas in a group simultaneously. Two gates targeting
   the same Deployment can freeze 4 of 5 replicas, causing the very
   decompensation cascade the freeze was meant to prevent.

2. **Peer pods.** Sibling pods in the same workload group don't know a peer
   is deliberately frozen. Kafka consumer groups rebalance unnecessarily.
   gRPC clients retry against a frozen backend. Connection pools churn.

3. **Other controllers.** HPA sees an unready pod but can't distinguish
   "frozen for deliberation" from "crashed." It scales up to compensate
   for a non-failure.

## Prior Art: z/OS Sysplex

z/OS provides cross-system wait state awareness through four mechanisms:

- **XCF (Cross-system Coupling Facility):** Programs join named groups.
  When a member enters a wait state, XCF notifies other group members
  across the sysplex.
- **GRS (Global Resource Serialization):** Cross-system resource contention
  is visible. The requesting system knows who holds a resource and where.
- **Sysplex-wide WLM:** Workload Manager operates across systems. If a
  workload is waiting on one system, WLM reroutes work to systems that
  aren't waiting.
- **Coupling Facility structures:** Shared lock and list structures are
  visible across systems. Contention is tracked at the cluster level.

The key property: the wait state is a first-class cross-system concept. The
entire sysplex knows, and other systems adjust behavior accordingly.

The existing freeze protocol design (`docs/design/freeze-protocol.md`)
treats z/OS WTOR as a single-node pattern. In a sysplex, the "system" that
knows about the wait is the cluster, not the individual machine. This
design extends Epoche with the same property.

## Design

### FreezeBudget CRD

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

### DecisionGate Changes

New fields on `DecisionGateSpec`:

```yaml
spec:
  # How long this gate tolerates being queued (budget blocking freeze).
  # Must be <= covering FreezeBudget's queueTimeout.
  # Defaults to the FreezeBudget's queueTimeout if omitted.
  # +optional
  queueTimeout: "2m"

  # What happens when queueTimeout expires.
  # Fail (default): phase -> Failed, gate creator decides next step.
  # Force: override budget, freeze anyway.
  # Escalate: re-notify through escalation channels, remain queued.
  # +optional
  # +kubebuilder:validation:Enum=Fail;Force;Escalate
  # +kubebuilder:default=Fail
  queueTimeoutAction: Fail
```

New phase: `Queued` added to `GatePhase` enum.
New event type: `Queued` added to `GateEventType` enum.

### Phase Lifecycle

```
 (created) ------> Queued ------> Pending ------> Decided ------> Executed
                     |                              ^
                     |                              |
                     v                           TimedOut
                   Failed                           |
                     ^                              v
                     +------------------------- Decided
```

| From | To | Trigger |
|---|---|---|
| (new) | Pending | Budget allows freeze, or no budget covers this pod |
| (new) | Queued | Budget exhausted |
| Queued | Pending | Budget opens up (peer unfroze), this gate is next in priority |
| Queued | Failed | Queue timeout expires and `queueTimeoutAction: Fail` |
| Queued | Pending | Queue timeout expires and `queueTimeoutAction: Force` |
| Queued | (re-notify) | Queue timeout expires and `queueTimeoutAction: Escalate` |
| Pending | Decided | Responder patches `spec.response` |
| Pending | Decided | Decision timeout fires, default action applied |
| Decided | Executed | Action executed, container unfrozen |
| Any | Failed | Unrecoverable error |

**Queue priority:** When budget opens, the highest-priority queued gate
proceeds. Priority order: severity first (Critical > Warning > Info),
then creation timestamp (oldest first).

**Queue timeout vs decision timeout:** These are separate durations with
separate semantics. The queue timeout bounds how long a degraded pod runs
without intervention (the pod is NOT frozen during this phase). The
decision timeout bounds how long a human has to respond (the pod IS
frozen). The queue timeout clock starts at gate creation. The decision
timeout clock starts at freeze.

### Grouping

#### Implicit (default)

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

#### Explicit (FreezeBudget)

A FreezeBudget's label selector defines the group. Its `minAvailable` or
`maxUnavailable` defines the constraint. This overrides the implicit
default for all pods matching the selector.

Explicit grouping handles cases implicit cannot:

| Case | Why implicit fails | Explicit solution |
|---|---|---|
| Pods across Deployments in the same Service | Different owner chains | Selector on shared labels |
| StatefulSet with leader/follower roles | All siblings look equal | Separate FreezeBudgets with different selectors |
| Cross-namespace coordination | Owner refs don't cross namespaces | Out of scope for v1alpha1 |

#### Precedence

If both implicit and explicit apply to the same pod, explicit wins. The
implicit owner-chain default is suppressed for any pod covered by at
least one FreezeBudget.

Multiple FreezeBudgets can cover the same pod. The controller must
satisfy all of them. Most restrictive wins.

### Enforcement

#### DecisionGate -> FreezeBudget (check before freeze)

The reconcile flow in `DecisionGateReconciler.initialize` changes:

```
Current:
  validate target -> freeze -> set Pending -> notify

Proposed:
  validate target -> find covering FreezeBudgets -> check budget
    |                                                  |
    | (no budget, or budget allows)                    | (budget exhausted)
    v                                                  v
  freeze -> set Pending -> notify                    set Queued, requeue
```

#### FreezeBudget -> DecisionGate (sole status writer)

The FreezeBudgetReconciler is the sole writer of FreezeBudget status.
The DecisionGateReconciler does not write FreezeBudget status.

The DecisionGateReconciler signals intent by:
- Setting the pod label `epoche.dev/frozen: "true"` when freezing
- Updating the gate's phase

The FreezeBudgetReconciler watches:
- Pods matching its selector (for count accuracy)
- DecisionGates (for phase changes affecting its group)

It computes and writes: `expectedPods`, `currentHealthy`, `currentFrozen`,
`freezesAllowed`, `frozenPods`, `queuedGates`.

When `freezesAllowed` transitions from 0 to a positive value, the
FreezeBudgetReconciler triggers re-reconciliation of the highest-priority
Queued gate.

#### Concurrency

Budget evaluation is point-in-time. No distributed locking. The
Kubernetes API server's optimistic concurrency on the FreezeBudget
status update is the serialization point.

If two DecisionGateReconcilers race to freeze two pods and only one
freeze is allowed: one gate proceeds, the other sees budget exhausted
on its next reconcile and transitions to Queued.

### Visibility Layers

Three levels of granularity, one source of truth.

| Layer | Mechanism | Consumer |
|---|---|---|
| Group-level | FreezeBudget `.status` (watch/list) | Other controllers, dashboards, Epoche controller |
| Pod-level | Pod label `epoche.dev/frozen: "true"` | Any controller, kubectl, service mesh |
| Application-level | Epoche SDK `OnPeerFrozen` callback | Peer pods using the SDK |

#### FreezeBudget status (group-level)

The `frozenPods` list provides the z/OS XCF equivalent: which group
members are frozen, since when, for what condition, and which
DecisionGate owns the freeze. The `queuedGates` list shows pending
demand.

Any controller watching FreezeBudget resources can distinguish
"1 of 5 pods is deliberately frozen for deliberation" from
"1 of 5 pods crashed."

#### Pod label (pod-level)

```
epoche.dev/frozen: "true"
```

Set by the DecisionGateReconciler when freezing, removed when unfreezing.
Queryable by any controller, visible in `kubectl get pods -l epoche.dev/frozen=true`.
Usable in service mesh routing rules (e.g., Istio DestinationRule).

#### SDK callback (application-level)

The Epoche SDK provides a watcher over FreezeBudget status:

```go
epoche.OnPeerFrozen(func(event epoche.PeerFreezeEvent) {
    // A peer pod was frozen. Adjust behavior:
    // stop sending work, absorb its partition, etc.
})
```

The SDK watches FreezeBudget resources whose selectors match the
current pod's labels. When `frozenPods` changes, callbacks fire.

### Queue Timeout Behavior

When a queued gate's queue timeout expires, `queueTimeoutAction`
determines the outcome:

| Action | Behavior | Use case |
|---|---|---|
| `Fail` (default) | Phase -> Failed, reason "QueueTimeout" | Safe default. Creator decides next step. |
| `Force` | Override budget, freeze anyway | Break-glass for critical conditions. |
| `Escalate` | Re-notify through escalation channels, remain queued | Ask a human whether to force. |

The queue timeout is bounded by the covering FreezeBudget's
`spec.queueTimeout`. A gate's `spec.queueTimeout` can be shorter
(more urgent condition) but not longer (the budget owner sets the
ceiling). If a gate specifies a longer timeout than its budget allows,
the budget's timeout applies.

## Out of Scope (v1alpha1)

- **Cross-namespace FreezeBudgets.** Requires a cluster-scoped variant.
  The design accommodates this later without breaking changes.
- **Cross-cluster freeze awareness.** Would require a federation layer.
- **HPA/VPA/cluster-autoscaler integration.** These controllers can
  read FreezeBudget status and pod labels. Epoche exposes the state;
  building adapters for every controller is out of scope.
- **SDK `OnPeerFrozen` implementation.** The CRD and status shape
  enable it. The SDK work is a separate effort.
- **Admission webhook for budget validation.** A validating webhook
  that rejects DecisionGate creation when budget is exhausted (rather
  than queuing). Useful but not required for v1alpha1 — the controller
  handles it via the Queued phase.

## References

- z/OS MVS Programming: Authorized Assembler Services Guide — XCF services
- z/OS MVS Planning: Workload Management — sysplex-wide goal management
- Kubernetes PodDisruptionBudget — API conventions and semantics
- `docs/design/freeze-protocol.md` — single-pod freeze protocol
