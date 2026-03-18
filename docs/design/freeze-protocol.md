# Freeze Protocol Design

## Problem Statement

Epoché's core operation is to pause a running container so that a human or
automation system can make a decision before the system acts autonomously.
The naive approach — freeze the container's processes using the cgroup v2
freezer or CRI `PauseContainer` — creates two categories of problems that
are worse than many of the conditions the freeze was meant to address.

### External control loops don't know about the freeze

Kubernetes runs multiple independent control loops that monitor container
health. None of them understand "deliberately paused":

| Control loop | What happens during freeze | Consequence |
|---|---|---|
| Liveness probe | Fails (process can't respond) | kubelet restarts the pod |
| Readiness probe | Fails | Pod removed from Service endpoints |
| Load balancer health check | Fails | Backend marked unhealthy |
| HPA | Sees error rate spike | Scales up (new replicas for a non-failure) |
| PodDisruptionBudget | Sees unavailable pod | May block other operations |

The liveness probe failure is fatal: the kubelet kills the pod, destroying
the container we froze to protect. The readiness probe failure cascades
upstream. The freeze causes the very decompensation it was designed to prevent.

### Internal state becomes inconsistent with reality

Even if all external control loops are coordinated (probes suspended,
readiness gates set), the container's internal state is corrupted by a
time discontinuity. During a cgroup freeze:

- **Wall clock keeps advancing.** `CLOCK_MONOTONIC` and `CLOCK_REALTIME`
  are not paused. When the process unfreezes, `time.Now()` jumps forward
  by the freeze duration.

- **Go `context.Context` deadlines expire.** Every `context.WithTimeout`
  and `context.WithDeadline` in flight becomes `Done()` immediately on
  unfreeze. The Go runtime's timer heap sees all pending timers as expired
  and fires them simultaneously.

- **Outgoing TCP connections die.** TCP keepalive timers (kernel-level)
  keep ticking. The remote end's read timeout expires during the freeze.
  When the process unfreezes, connection pools are full of dead connections
  (`connection reset by peer`).

- **Database connections are closed.** PostgreSQL's `tcp_keepalives_idle`,
  MySQL's `wait_timeout`, etc. all fire on the server side. Every pending
  query fails on resume.

- **gRPC/HTTP2 streams are terminated.** Keepalive pings weren't sent.
  The server closed the connection. Every pending RPC fails.

- **Message queue consumers are evicted.** Kafka's `session.timeout.ms`
  expires. The consumer is kicked from the group. Partition rebalance is
  triggered for other consumers.

The net effect: the instant the process unfreezes, every goroutine with
a pending timed operation fails simultaneously. Error handling paths fire
in parallel across every subsystem. This is a decompensation cascade
*inside the application*.

## Prior Art: z/OS WTOR

z/OS has had operator-intervention-as-control-flow since the 1960s. When a
program issues a WTOR (Write To Operator with Reply), the following happens:

1. **The program chooses when to pause.** The WTOR call is at a point in the
   code where the programmer decided it was safe to wait. The program has
   already drained pending work and reached a defined suspension point.

2. **The system knows the program is waiting.** The Master Scheduler tracks
   the address space state. Workload Manager (WLM) stops measuring
   performance goals. The wait is a recognized system state, not an
   unresponsive process.

3. **There are no independent health checks that would kill the waiting
   program.** The operator console IS the health check system. Waiting for
   a reply is a legitimate state that the entire system respects.

4. **The program resumes from the defined point.** When the operator replies,
   the program continues from exactly where it called WTOR. It can
   re-establish connections and resume work in a controlled sequence.

The key property: **the pause is cooperative and occurs at a defined safe
point.** The program's internal state is consistent because the program
prepared for the wait. There is no time discontinuity from the program's
perspective — it asked to wait, and it was woken up with an answer.

## The Unix Signal Protocol

Unix has had an equivalent mechanism since 4.2BSD job control, built on
three signals:

| Signal | Catchable | Default action | Purpose |
|--------|-----------|----------------|---------|
| `SIGTSTP` | Yes | Stop process | "Prepare to stop" |
| `SIGSTOP` | No | Stop process | "You are stopped now" |
| `SIGCONT` | Yes | Continue | "You have been resumed" |

The shell uses this for job control: `Ctrl+Z` sends `SIGTSTP`, the process
stops, `fg` sends `SIGCONT`. Programs like `vim` catch `SIGTSTP` to restore
terminal state before stopping, and catch `SIGCONT` to redraw on resume.

This is the z/OS WTOR pattern expressed in Unix signals:

```
SIGTSTP received (catchable)
  → handler drains in-flight requests
  → handler cancels outgoing contexts
  → handler closes or parks connections
  → handler pauses context timers (see below)
  → handler calls kill(getpid(), SIGSTOP)   ← self-stop at a safe point

...process is stopped, decision is being made...

SIGCONT received (catchable, delivered automatically on resume)
  → handler resumes context timers
  → handler re-establishes connection pools
  → handler rejoins consumer groups
  → handler signals readiness to accept work
```

The program participates in reaching a safe point (`SIGTSTP` handler),
then stops itself at that point (`self-SIGSTOP`). On resume, it gets
notified (`SIGCONT`) and can re-establish its world before accepting work.

For processes that **don't** catch `SIGTSTP`, the default action is to
stop — same as `SIGSTOP`. The mechanism degrades gracefully: cooperative
applications get clean suspend/resume; uncooperative applications still
stop, they just resume messily.

### Comparison

```
z/OS WTOR                         cgroup freeze
───────────────────────────────   ───────────────────────────────
Program chooses when to pause     External agent pauses at arbitrary point
Program reaches safe state first  No safe state guarantee
Program knows it waited           Program sees time discontinuity
Program re-establishes on resume  Every timer fires simultaneously
Cooperative by design             Supervisory only

SIGTSTP / SIGSTOP / SIGCONT
───────────────────────────────
Cooperative when caught (SIGTSTP)
Supervisory fallback (default SIGTSTP → stop)
Resume notification built in (SIGCONT)
Defined safe point when cooperative
```

## Pausable Contexts

The Unix signal protocol solves the process-level problem: the application
can drain connections and reach a safe point before stopping. But there is
a deeper problem at the language runtime level: Go's `context.Context`
deadlines are based on wall clock time. Even if the application drains
in-flight requests, any context that survives the freeze (background
goroutines, long-running operations) will see its deadline expire
instantly on resume.

### The problem with standard contexts

```go
ctx, cancel := context.WithTimeout(parentCtx, 30*time.Second)
defer cancel()

// If the process is frozen for 5 minutes between here...
resp, err := http.DefaultClient.Do(req.WithContext(ctx))
// ...and here, the context is already expired. err = context.DeadlineExceeded.
```

The 30-second timeout measured 5 minutes of wall clock time during which
the process wasn't running. The timeout is wrong — it should measure
*active time*, not wall clock time.

### Epoché pausable context

A custom `context.Context` implementation that supports pausing the
deadline timer:

```go
package epoche

// WithPausableTimeout returns a context whose deadline timer can be paused
// and resumed. When paused, the remaining duration is saved. When resumed,
// the timer restarts with the saved duration. Wall clock time that elapses
// while paused does not count toward the timeout.
//
// If the parent context is canceled or expires, the pausable context is
// also canceled, regardless of pause state.
func WithPausableTimeout(parent context.Context, timeout time.Duration) (*PausableContext, context.CancelFunc)
```

The implementation tracks remaining time at pause and restarts the timer
on resume:

```go
type PausableContext struct {
	context.Context                  // parent
	mu        sync.Mutex
	done      chan struct{}
	err       error
	timer     *time.Timer
	remaining time.Duration          // saved on pause
	deadline  time.Time              // adjusted on resume
	paused    bool
}

func (c *PausableContext) Pause() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.paused || c.err != nil {
		return
	}
	c.timer.Stop()
	c.remaining = time.Until(c.deadline)
	if c.remaining < 0 {
		c.remaining = 0
	}
	c.paused = true
}

func (c *PausableContext) Resume() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.paused || c.err != nil {
		return
	}
	c.paused = false
	c.deadline = time.Now().Add(c.remaining)
	c.timer.Reset(c.remaining)
}

func (c *PausableContext) Deadline() (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deadline, true
}

func (c *PausableContext) Done() <-chan struct{} {
	return c.done
}

func (c *PausableContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}
```

### Global pause/resume via the SDK

The Epoché client SDK maintains a registry of all active pausable
contexts. The `SIGTSTP` handler pauses them all; the `SIGCONT` handler
resumes them:

```go
package epoche

var (
	registryMu sync.Mutex
	registry   []*PausableContext
)

func init() {
	suspend := make(chan os.Signal, 1)
	resume := make(chan os.Signal, 1)
	signal.Notify(suspend, syscall.SIGTSTP)
	signal.Notify(resume, syscall.SIGCONT)

	go func() {
		for range suspend {
			pauseAllContexts()
			// Application's drain hook runs here (see Freeze Protocol below)
			syscall.Kill(syscall.Getpid(), syscall.SIGSTOP)
		}
	}()

	go func() {
		for range resume {
			resumeAllContexts()
			// Application's resume hook runs here
		}
	}()
}

func pauseAllContexts() {
	registryMu.Lock()
	defer registryMu.Unlock()
	for _, ctx := range registry {
		ctx.Pause()
	}
}

func resumeAllContexts() {
	registryMu.Lock()
	defer registryMu.Unlock()
	for _, ctx := range registry {
		ctx.Resume()
	}
}
```

### What pausable contexts solve and don't solve

**Solved:**
- Go-level timeouts (`context.WithTimeout`, `context.WithDeadline`) for
  operations that use `epoche.WithPausableTimeout` instead of the standard
  library equivalents.
- Any library that accepts a `context.Context` parameter will respect the
  adjusted deadline transparently — the interface is unchanged.

**Not solved:**
- Contexts created internally by libraries using `context.WithTimeout`.
  A library that creates its own timeout context from the parent won't
  create a pausable one. The parent being pausable doesn't make the child
  pausable.
- Kernel-level timers (TCP keepalive, TCP retransmission). These run in
  the kernel and are not affected by userspace pause/resume.
- Remote-end timeouts. The server's read timeout fires regardless of what
  the client does. Long-lived connections (database pools, gRPC streams)
  will be closed by the remote end during any non-trivial freeze duration.

The first limitation can be addressed by having the Epoché SDK provide
pausable wrappers for common operations (`epoche.HTTPClient`,
`epoche.DialContext`, etc.) that use pausable contexts internally.

The second and third limitations are inherent: kernel timers and remote
servers cannot be paused by the client. The `SIGCONT` resume handler must
re-establish these connections. This is the same requirement z/OS programs
have — the program re-establishes its external connections after the
operator replies.

### Future: language-level support

The ideal solution would be for `context.WithTimeout` in the Go standard
library to support a `Pausable` option, or for the runtime to understand
process suspension (e.g., adjusting the monotonic clock on `SIGCONT`).
This would make all contexts freeze-aware without library changes.

This is not currently proposed for Go. The Epoché pausable context serves
as a proof of concept and a practical solution for applications that
integrate the SDK.

## The Epoché Freeze Protocol

Combining the external coordination (Kubernetes control plane), the Unix
signal protocol, and pausable contexts into a complete freeze operation:

### Phase 1: Prepare (operator → Kubernetes → container)

```
Operator (Epoché controller):
  1. Add readiness gate: epoche.dev/frozen=False
     → Pod removed from Service endpoints (no new inbound traffic)
  2. Annotate pod: epoche.dev/gate=<gate-name>
     → Mutating webhook intercepts liveness probe results,
       reports passing while annotation is present
  3. Wait for in-flight request drain (configurable grace period)

Container (Epoché sidecar or SDK):
  4. Send SIGTSTP to PID 1 in the container's PID namespace
```

### Phase 2: Drain (inside the container, SIGTSTP handler)

```
SIGTSTP handler fires:
  5. Pause all registered pausable contexts
  6. Stop accepting new work (close listeners, deregister from service mesh)
  7. Wait for in-flight requests to complete (bounded by drain timeout)
  8. Close or park connection pools (DB, gRPC, message queues)
  9. Call syscall.Kill(syscall.Getpid(), syscall.SIGSTOP)
     → Process stops at a defined safe point
```

### Phase 3: Frozen (process is stopped, decision is pending)

```
  10. Container is in TASK_STOPPED state. No CPU time consumed.
  11. Liveness probes pass (webhook intercepts).
  12. Pod is out of service endpoints (readiness gate holds).
  13. DecisionGate resource is Pending. Notifications sent.
  14. Responder evaluates and provides spec.response.
      (Or timeout fires and default action is applied.)
```

### Phase 4: Resume (decision made, operator unfreezes)

```
Operator (Epoché controller):
  15. Send SIGCONT to PID 1
      → SIGCONT handler fires in the container

Container (SIGCONT handler):
  16. Resume all registered pausable contexts
  17. Re-establish connection pools (DB, gRPC, message queues)
  18. Rejoin consumer groups (Kafka, etc.)
  19. Signal readiness

Operator (Epoché controller):
  20. Clear readiness gate: epoche.dev/frozen=True
      → Pod added back to Service endpoints
  21. Remove pod annotation
      → Webhook stops intercepting liveness probes
  22. Execute the decided action (scale, restart, heap dump, etc.)
  23. Update DecisionGate status to Executed
```

### Sequence diagram

```
  Operator          Sidecar/SDK        Container PID 1      Kubelet
     │                  │                    │                  │
     ├─ set readiness ──┤                    │                  │
     │   gate           │                    │                  │
     ├─ annotate pod ───┤                    │                  │
     │                  │                    │                  │
     │                  ├── SIGTSTP ────────►│                  │
     │                  │                    ├─ pause contexts  │
     │                  │                    ├─ drain requests  │
     │                  │                    ├─ close pools     │
     │                  │                    ├─ SIGSTOP(self) ──┤
     │                  │                    │  (stopped)       │
     │                  │                    │                  ├─ liveness probe
     │                  │                    │                  │  (webhook: pass)
     │                  │                    │                  │
     │  ...decision pending...               │                  │
     │                  │                    │                  │
     ├── SIGCONT ──────►├── SIGCONT ────────►│                  │
     │                  │                    ├─ resume contexts │
     │                  │                    ├─ reconnect pools │
     │                  │                    ├─ signal ready    │
     │                  │                    │                  │
     ├─ clear gate ─────┤                    │                  │
     ├─ remove annot. ──┤                    │                  │
     ├─ execute action  │                    │                  │
     │                  │                    │                  │
```

## Implementation Tiers

The protocol supports three levels of application integration. The
mechanism is the same; the quality of suspend/resume degrades gracefully.

### Tier 1: Full SDK integration

The application uses the Epoché Go SDK:
- `epoche.WithPausableTimeout` for context creation
- Registers drain and resume hooks
- Catches `SIGTSTP` and `SIGCONT`

**Suspend quality:** Clean. All contexts paused, connections drained,
safe stop point reached.

**Resume quality:** Clean. Contexts resume with correct remaining
time, connections re-established, consumer groups rejoined.

### Tier 2: Signal-aware application

The application catches `SIGTSTP` and `SIGCONT` with custom handlers
(without the Epoché SDK):
- Drains connections in `SIGTSTP` handler
- Re-establishes in `SIGCONT` handler
- Does not use pausable contexts

**Suspend quality:** Good. Connections drained, but Go contexts that
survive the freeze will expire on resume.

**Resume quality:** Moderate. Connections re-established, but a burst
of `context.DeadlineExceeded` errors for any contexts that were active
during the freeze.

### Tier 3: Uncooperative application

The application does not handle `SIGTSTP`:
- Default `SIGTSTP` action stops the process (same as `SIGSTOP`)
- No drain, no context pausing
- `SIGCONT` resumes but no handler runs

**Suspend quality:** Equivalent to cgroup freeze. Process stops at an
arbitrary point.

**Resume quality:** Poor. Thundering herd of expired contexts, dead
connections, failed operations. The application must recover through
its own retry/reconnection logic.

This is still better than an OOM kill or uncontrolled restart: the
process state is preserved, no cold start is needed, and the
decided action can be a graceful restart rather than a crash.

### Tier comparison

```
                       Tier 1          Tier 2          Tier 3
                       (Full SDK)      (Signal-aware)  (Uncooperative)
────────────────────   ─────────────   ──────────────  ───────────────
Context deadlines      Paused          Expire          Expire
In-flight requests     Drained         Drained         Interrupted
Connection pools       Parked          Closed          Dead
Resume connections     Automatic       Automatic       App retry logic
Consumer groups        Graceful leave  Graceful leave  Evicted + rebalance
Resume errors          None            Some            Many
Application changes    SDK + hooks     Signal handlers None
```

## Revised Freezer Interface

The Freezer interface reflects the two-phase signal protocol:

```go
// Freezer manages the suspension and resumption of a container's processes.
//
// Suspend sends SIGTSTP and waits up to the grace period for the process
// to self-stop (indicating it has reached a safe point). If the process
// does not stop within the grace period, Suspend sends SIGSTOP to force it.
//
// Resume sends SIGCONT. If the process caught SIGTSTP and self-stopped,
// its SIGCONT handler will fire and re-establish connections. If the
// process was force-stopped, SIGCONT simply resumes it without a handler.
type Freezer interface {
	Suspend(ctx context.Context, ref ContainerRef, grace time.Duration) error
	Resume(ctx context.Context, ref ContainerRef) error
}

// ContainerRef identifies a container to freeze.
type ContainerRef struct {
	Namespace     string
	PodName       string
	ContainerName string
}
```

### Implementation: signal-based freezer

```go
type SignalFreezer struct {
	// CRI client or node agent for accessing the container's PID namespace
	runtime RuntimeClient
}

func (f *SignalFreezer) Suspend(ctx context.Context, ref ContainerRef, grace time.Duration) error {
	pid, err := f.runtime.GetContainerPID(ctx, ref)
	if err != nil {
		return fmt.Errorf("get container PID: %w", err)
	}

	// Phase 1: cooperative stop (SIGTSTP)
	if err := syscall.Kill(pid, syscall.SIGTSTP); err != nil {
		return fmt.Errorf("send SIGTSTP: %w", err)
	}

	// Wait for the process to self-stop (State: T in /proc/<pid>/status)
	deadline := time.After(grace)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			// Grace period expired — force stop
			if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
				return fmt.Errorf("send SIGSTOP: %w", err)
			}
			return nil
		case <-ticker.C:
			if isStopped(pid) {
				return nil // Process self-stopped after handling SIGTSTP
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (f *SignalFreezer) Resume(ctx context.Context, ref ContainerRef) error {
	pid, err := f.runtime.GetContainerPID(ctx, ref)
	if err != nil {
		return fmt.Errorf("get container PID: %w", err)
	}

	return syscall.Kill(pid, syscall.SIGCONT)
}
```

## What Cannot Be Solved

Some problems are inherent to pausing a networked process and have no
solution at any tier:

1. **Remote-end timeouts.** The server's idle timeout fires regardless of
   what the client does. Long-lived connections (database pools, gRPC
   streams, WebSocket connections) will be closed by the remote end during
   any non-trivial freeze. The `SIGCONT` handler must re-establish them.
   This is the same requirement z/OS programs have.

2. **Kernel-level timers.** TCP keepalive and retransmission timers run in
   the kernel. They cannot be paused from userspace. The drain phase should
   close sockets cleanly so there are no kernel timers left to fire.

3. **External state changes.** The world moves on while the process is
   stopped. Database rows may be modified. Message queue offsets may
   advance. Configuration may change. The resume handler must validate
   assumptions, not blindly continue.

4. **Non-Go runtimes.** The pausable context mechanism is Go-specific.
   Other languages would need equivalent implementations. The signal
   protocol (SIGTSTP/SIGSTOP/SIGCONT) is universal across Unix, but
   the context/timer pausing is language-runtime-specific.

These are not failures of the design — they are the irreducible cost of
pausing a running system. z/OS programs face the same costs. The goal is
not to make the freeze invisible, but to make the resume *controlled*.

## References

- z/OS MVS Programming: Authorized Assembler Services Guide — WTOR macro
- IEEE Std 1003.1 (POSIX) — Signal concepts, job control signals
- Linux kernel documentation — cgroup v2 freezer
- Go standard library — context package, os/signal package
