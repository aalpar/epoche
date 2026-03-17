# Epoché

**Deliberate suspension of judgment for Kubernetes workloads.**

*Epoché* (ἐποχή) — the Pyrrhonian Skeptic practice of withholding judgment
when evidence is insufficient. Applied to infrastructure: when a system
reaches its compensation boundary, the correct response is not to act on
incomplete information — it is to *pause*, *escalate*, and *wait* for a
decision from an entity with more context.

## The Problem

Kubernetes control loops are closed: observe → decide → act. The decision
is always made by code — a liveness probe fails, the kubelet restarts the
pod; memory exceeds a limit, the OOM killer fires; a node is under pressure,
pods are evicted. These local decisions often cascade precisely because they
lack global context.

There is no primitive for "I don't know what to do — ask someone who does."

## What Epoché Provides

A `DecisionGate` custom resource and operator that introduces a
**deliberation step** into the Kubernetes control loop:

1. **Freeze** — pause a container's processes using the cgroup v2 freezer
2. **Escalate** — notify a human or automation system with structured context
3. **Decide** — an authorized responder chooses an action
4. **Execute** — the operator carries out the decision and unfreezes
5. **Audit** — every step is recorded as Kubernetes Events with full attribution

```
Container hits condition
        │
        ▼
  DecisionGate CR created
        │
        ▼
  Operator freezes container (cgroup v2)
        │
        ▼
  Notifications sent (PagerDuty, Slack, etc.)
        │
        ├──────────────────────────┐
        ▼                          ▼
  Human responds             Timeout fires
  (kubectl, dashboard,       (executes default
   Slack bot, API)            action)
        │                          │
        └──────────┬───────────────┘
                   ▼
  Decision recorded, action executed, container unfrozen
```

## Design Principles

- **Supervisory, not cooperative.** The cgroup freezer pauses any container
  without the application's cooperation — because a failing component may
  not be in a state to cooperate.
- **Decision, not delay.** The pause exists to gather information, not to
  wait passively. Timeout policies ensure decisions are bounded.
- **Automation-ready.** Responders can be humans or service accounts with
  the same RBAC and audit trail. Start manual, automate the patterns.
- **Auditable by default.** Who decided, when, what they chose, and why —
  recorded in the DecisionGate status and Kubernetes Events.

## Theoretical Basis

Epoché operationalizes concepts from control-theoretic analysis of failure
cascades:

- **Compensation boundary** — the threshold where a system transitions from
  reporting degradation to taking autonomous corrective action. Epoché
  inserts a decision gate at this boundary.
- **Decision authority tracks information asymmetry** — the entity with the
  most relevant context should make the decision. Local controllers often
  lack global context; Epoché escalates to entities that have it.
- **Decompensation cascade** — when local corrective actions (restarts,
  kills, evictions) create load that triggers further failures. Epoché
  breaks the cascade by pausing instead of acting.

See: *Don't Let Your System Decide It's Dead* (forthcoming).

## Status

Early design phase. Contributions and discussion welcome.

## License

Apache License 2.0
