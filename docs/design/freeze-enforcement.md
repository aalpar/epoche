# Freeze Enforcement

Part of [Cross-Pod Freeze State Visibility](cross-pod-visibility.md).

## Controller Responsibilities

Two controllers, clear ownership boundaries.

### DecisionGateReconciler

Owns the gate lifecycle: `Queued -> Pending -> Decided -> Executed`.

Responsibilities:
- Validates the target pod exists
- Reads FreezeBudget status to check budget before freezing
- Freezes/unfreezes the container
- Sets the pod label `epoche.dev/frozen: "true"` when freezing, removes
  it when unfreezing
- Transitions gate phases
- Sends notifications
- Does **not** write FreezeBudget status

### FreezeBudgetReconciler

Owns the FreezeBudget status. Sole writer.

Responsibilities:
- Watches pods matching its selector (for count accuracy)
- Watches DecisionGates (for phase changes affecting its group)
- Computes and writes all status fields: `expectedPods`,
  `currentHealthy`, `currentFrozen`, `freezesAllowed`, `frozenPods`,
  `queuedGates`
- When `freezesAllowed` transitions from 0 to a positive value,
  triggers re-reconciliation of the highest-priority Queued gate

## Reconcile Flow

The `DecisionGateReconciler.initialize` method changes:

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

### Finding Covering FreezeBudgets

The controller lists all FreezeBudgets in the gate's namespace and
evaluates each selector against the target pod's labels. A FreezeBudget
"covers" a pod if its selector matches the pod's labels.

If no FreezeBudget covers the pod, the controller checks the implicit
budget (owner-chain walk). See [FreezeBudget CRD](freeze-budget-crd.md)
for grouping details.

### Checking Budget

For each covering FreezeBudget, the controller reads
`status.freezesAllowed`. If any budget has `freezesAllowed == 0`, the
gate transitions to Queued.

The controller does not decrement `freezesAllowed` itself — it freezes
the pod and sets the label. The FreezeBudgetReconciler observes the
label change (or the gate's phase change), recomputes counts, and
updates status.

## Sole-Writer Pattern

Only the FreezeBudgetReconciler writes FreezeBudget status. This
eliminates contention between controllers.

The DecisionGateReconciler communicates intent through observable side
effects:
- Pod label `epoche.dev/frozen: "true"` (set/removed)
- DecisionGate phase changes (Queued, Pending, Executed, Failed)

The FreezeBudgetReconciler watches these signals and computes the
authoritative status. This is analogous to z/OS's separation of XCF
signaling (async notifications between address spaces) from Coupling
Facility structures (shared state with single-owner semantics).

## Concurrency

Budget evaluation is point-in-time. No distributed locking.

The Kubernetes API server's optimistic concurrency (resource version on
status updates) is the serialization point. The sequence:

1. DecisionGateReconciler reads FreezeBudget status: `freezesAllowed: 1`
2. DecisionGateReconciler freezes the pod, sets label, transitions gate
   to Pending
3. FreezeBudgetReconciler observes the change, recomputes:
   `currentFrozen: 2`, `freezesAllowed: 0`
4. FreezeBudgetReconciler writes updated status

If two DecisionGateReconcilers race to freeze two pods and only one
freeze is allowed:
- Both read `freezesAllowed: 1`
- Both freeze their respective pods
- FreezeBudgetReconciler recomputes and sees `currentFrozen: 2` with
  `maxUnavailable: 1` — budget is violated
- The budget violation is visible in status but not automatically
  rolled back

This is a known race window. The window is small (between reading
`freezesAllowed` and the FreezeBudgetReconciler updating status) and
the consequence is bounded (one extra freeze, visible in status). For
v1alpha1, this is acceptable. A future admission webhook could close
this gap by rejecting the freeze at the API level.

### Re-reconciliation Trigger

When the FreezeBudgetReconciler updates `freezesAllowed` from 0 to a
positive value (because a pod was unfrozen), it must trigger
re-reconciliation of queued gates. It does this by listing DecisionGates
in the Queued phase that target pods covered by this budget, selecting
the highest-priority gate (severity first, then creation time), and
enqueueing it for reconciliation.
