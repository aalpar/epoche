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

The existing freeze protocol design (`freeze-protocol.md`) treats z/OS WTOR
as a single-node pattern. In a sysplex, the "system" that knows about the
wait is the cluster, not the individual machine. This design extends Epoche
with the same property.

## Design Documents

The design is split into four documents, each covering one concern:

- **[FreezeBudget CRD](freeze-budget-crd.md)** — the new resource that
  defines freeze groups and constraints, including implicit and explicit
  grouping semantics.

- **[Queued Phase](queued-phase.md)** — changes to the DecisionGate
  lifecycle: the new Queued phase, queue timeout, and queue timeout
  actions.

- **[Freeze Enforcement](freeze-enforcement.md)** — how the
  DecisionGateReconciler and FreezeBudgetReconciler coordinate,
  including the sole-writer pattern and concurrency model.

- **[Freeze Visibility Layers](freeze-visibility-layers.md)** — three
  visibility mechanisms for three consumer categories: FreezeBudget
  status, pod labels, and SDK callbacks.

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
- `freeze-protocol.md` — single-pod freeze protocol
