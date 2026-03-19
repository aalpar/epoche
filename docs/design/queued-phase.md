# Queued Phase

Part of [Cross-Pod Freeze State Visibility](cross-pod-visibility.md).

## DecisionGate Changes

### New Spec Fields

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

### New Phase and Event Type

`Queued` is added to the `GatePhase` enum. A DecisionGate in Queued
phase means: the condition is real, the freeze is warranted, but the
FreezeBudget won't allow it right now. The pod is NOT frozen — it
continues running in a degraded state.

`Queued` is added to the `GateEventType` enum for lifecycle tracking.

## Phase Lifecycle

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

## Queue Priority

When budget opens (a peer unfreezes and `freezesAllowed` transitions
from 0 to a positive value), the highest-priority queued gate proceeds.

Priority order:
1. **Severity** — Critical > Warning > Info
2. **Creation timestamp** — oldest first (among same severity)

## Two Timeouts, Two Semantics

The queue timeout and the decision timeout are separate durations that
bound different phases of the gate lifecycle:

| | Queue timeout | Decision timeout |
|---|---|---|
| **What it bounds** | Time in Queued phase | Time in Pending phase |
| **Pod state** | Running (degraded) | Frozen |
| **Clock starts** | Gate creation | Pod freeze |
| **Who is waiting** | The system (for budget) | A human (for a decision) |
| **Configured in** | `spec.queueTimeout` | `spec.timeout.duration` |

A gate created with `queueTimeout: 2m` and `timeout.duration: 5m` can
spend up to 2 minutes waiting for budget, then up to 5 minutes waiting
for a human decision after the pod is frozen. Total worst case: 7
minutes from gate creation to timeout.

## Queue Timeout Ceiling

The FreezeBudget's `spec.queueTimeout` sets a ceiling. A gate's
`spec.queueTimeout` can be shorter (more urgent condition) but not
longer (the budget owner sets the maximum tolerance for degraded-but-
unfrozen operation).

If a gate specifies a longer timeout than its covering budget allows,
the budget's timeout applies. If multiple FreezeBudgets cover the pod,
the shortest `queueTimeout` applies.

## Queue Timeout Actions

When the queue timeout expires, `queueTimeoutAction` determines the
outcome:

| Action | Behavior | Use case |
|---|---|---|
| `Fail` (default) | Phase -> Failed, reason "QueueTimeout" | Safe default. Gate creator decides next step. |
| `Force` | Override budget, freeze anyway | Break-glass for critical conditions. |
| `Escalate` | Re-notify through escalation channels, remain queued | Ask a human whether to force. |

### Fail

The gate transitions to Failed with reason "QueueTimeout". The
condition is still active, but the system chose not to freeze. The
entity that created the gate (monitoring system, admission webhook,
human) receives the Failed status and can decide: create a new gate
with higher severity, take a different action, or accept the situation.

### Force

The gate overrides the budget and freezes the pod anyway. This
transitions to Pending and proceeds normally. The FreezeBudget status
will show the budget is violated (`currentFrozen` exceeds what the
invariant allows). This is visible and auditable — the gate's events
record that it forced through a budget.

### Escalate

The gate re-sends notifications through its escalation channels with
context: "Gate X has been queued for Y minutes, FreezeBudget Z is
blocking it." The gate remains Queued. A human decides whether to force
(by patching `queueTimeoutAction` to `Force`) or accept the situation
(by deleting the gate).
