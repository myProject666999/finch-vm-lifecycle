# Virtual machine lifecycle state machine and cross-process locking

## Background

`finch vm start` and `finch vm stop` can be invoked almost simultaneously by
different processes on the same machine (two terminals, a CI script and an
interactive shell, the nerdctl auto-starter racing a manual command). Before
this change, ordering between them was implicit: each command queried Lima on
its own and then ran, relying on the underlying tool to fail if the sequence
turned out to be invalid. There was no single state model, no cross-process
mutual exclusion, and a process killed mid-operation could leave the VM in a
state the next command had to guess at.

This document specifies the replacement: an explicit seven-state machine, one
cross-process lock per VM instance, a durable record of the in-progress
operation and its holder, and deterministic recovery from failures,
interruptions and dead holders.

## Goals

- Decide who goes first by an explicit state machine and a cross-process mutex,
  never by an accidental error from the underlying tool.
- Use one shared state judgment for `init`, `start`, `stop` and `remove`.
- After a failure or interruption, re-query the real state and recover safely
  without asking the user to guess an intermediate state.
- Keep read-only status queries non-blocking, while making transitional states
  unmistakable.
- Give scripts stable exit codes that distinguish "nothing changed, but the
  desired state already holds" from an actual failure.

## Explicit state model

The VM is always modeled as exactly one of seven states:

| State | Meaning | Stable? |
| --- | --- | --- |
| `Uninitialized` | No instance exists; the driver reports no matching VM. | yes |
| `InitializedStopped` | The instance was created by `init` but has never completed a standalone start; the driver reports it stopped. | yes |
| `Starting` | A start or init is in progress. | no, transitional |
| `Running` | The driver reports the VM running and no operation is in progress. | yes |
| `Stopping` | A stop or remove is in progress. | no, transitional |
| `Stopped` | The VM exists, is stopped per the driver, and a start has completed at least once. | yes |
| `Corrupted` | An operation failed or was interrupted after partial side effects; the VM cannot be trusted. Only `remove` is accepted. | yes (terminal until removed) |

`InitializedStopped` and `Stopped` are semantically identical for every action;
they differ only in how `finch vm status` can present the distinction. Both
transitional states implement an `IsTransitional()` marker and are never
presented to users as a stable state.

## Source of truth

The authoritative state always comes from a fresh query of the underlying
virtual machine management tool (the `Driver.Status` call; in production this
is `limactl ls -f {{.Status}}`). A self-written state file alone is not trusted,
because a process killed halfway through a start would leave it stuck at
`Starting` forever.

The manager persists only one file, `operation.json`, which records:

- the operation currently in progress and its holder (`action`, `pid`,
  `started_at`);
- durable annotations the driver cannot express: `corrupted`, `needs_check`
  (set after a forced stop) and `boot_completed` (distinguishes
  `InitializedStopped` from `Stopped`).

The record is written atomically (temp file + rename) and is advisory: if its
holder process no longer exists, it is treated as residual state to be taken
over, never as the truth.

## Transition table

This table is the single source of truth for which action is legal in which
state. It is mirrored by an executable, table-driven test that fails if any
cell is left uncovered.

| State | `init` | `start` | `stop` | `remove` | `status` |
| --- | --- | --- | --- | --- | --- |
| `Uninitialized` | execute | **reject** | reject | reject | execute |
| `InitializedStopped` | reject | execute | no-op | execute | execute |
| `Starting` | reject | reject | reject | reject | execute |
| `Running` | reject | **no-op** | execute | reject | execute |
| `Stopping` | reject | **reject** | no-op | reject | execute |
| `Stopped` | reject | execute | no-op | execute | execute |
| `Corrupted` | reject | reject | reject | **execute (only)** | execute |

### The three previously undecided policies

1. **`start` on a Running VM: successful no-op, not an error.** Re-running the
   command that converges the system to its desired state is idempotent and
   exits `0`, after printing a distinguishable "already running; nothing to do"
   message. Scripts that simply retry `start` until success now converge
   instead of failing on the second invocation.
2. **`start` on an Uninitialized VM: reject and tell the caller to `init`.**
   The command does not silently create an instance, because creation takes
   configuration (the base YAML) that `start` does not carry. It exits with the
   invalid-transition code and a message naming `finch vm init`.
3. **`start` while the VM is Stopping: reject immediately, do not queue.** The
   request is refused with the concurrent-operation exit code rather than
   blocking behind the in-flight stop and then starting a VM the user may have
   just asked to stop. The caller decides whether to retry after the stop
   completes.

### No-op versus failure for scripts

Every outcome has a distinct, stable exit code so a caller can tell "nothing was
done, but the requested state holds" from a failure:

| Exit code | Meaning |
| --- | --- |
| `0` | The action completed, or it was a successful no-op. The printed message and the Go API's `Noop` flag distinguish the two. |
| `2` | Invalid transition; the message names the current state and the prerequisite command. |
| `3` | Concurrent operation; the request collided with an in-flight transitional state and was not queued. |
| `4` | Lock timeout; another process held the per-VM lock past the wait deadline. The error names the holder. |
| `5` | Underlying driver call failed after reconciliation. |
| `6` | Interrupted by a signal; the underlying tool invocation was terminated together with the CLI. |
| `70` | Internal lifecycle invariant violation. |

### Corrupted state and remove

An `init` (or start) that fails after partial creation marks the VM
`Corrupted`. In that state every mutating action is refused except `remove`:
remove is the only cleanup path for a broken half-created instance and must
never itself be rejected for being "illegal in this state". After removal the
VM returns to `Uninitialized` and `init` can be retried. If a failed `init`
left nothing behind at all (the driver reports the instance absent), the record
is instead rolled back fully to `Uninitialized` and a plain retry of `init`
works.

## Cross-process mutual exclusion

### Lock mechanism and lock file location

Mutating commands take one exclusive, advisory lock per VM instance before
reading or changing state:

- macOS/Linux: `flock(2)` on a lock file. `flock` locks are owned by the open
  file description and are released by the kernel when the holder exits or is
  killed, even on `SIGKILL`.
- Windows: `LockFileEx` with `LOCKFILE_EXCLUSIVE_LOCK`, whose lock is likewise
  tied to the handle and released by the OS when the process dies.

The lock file is `<finch config dir>/vm/<instance>/vm.lock`, next to
`operation.json`, inside the Finch config directory (`~/.finch` on macOS and
Windows). It is kept on a local disk so the documented lock semantics apply;
network mounts (notably NFS for `flock`) are out of scope. The lock file is
separate from the JSON record so the record can be rewritten, or taken over,
while the lock stays held on a stable descriptor.

### Residual files and process death

A crashed holder leaves the lock *file* behind but never an enforced lock: the
kernel releases `flock`/`LockFileEx` on process exit. The residual
`operation.json` names the holder pid. Before acting, a new holder checks
whether that pid is still alive (`kill(pid, 0)` on Unix, `OpenProcess` on
Windows). If it is gone, the record is stale and the new process takes over;
if it is alive, the lock genuinely remains contended.

### Takeover rules

Takeover always re-queries the driver first and only then decides:

- the interrupted operation actually finished (driver already shows the target
  state): adopt the result and clear the holder;
- it died before creating anything: roll back to the retryable initial state;
- it died mid-boot or half-removed: mark `Corrupted`, from which only `remove`
  proceeds;
- a stop died while the VM still runs: clear the holder so `stop` can be
  retried; a stop that already took effect is adopted as `Stopped`.

The residual record is never trusted on its own. An unreadable (corrupt JSON)
record is itself treated as a residual `Corrupted` marker rather than wedging
the VM permanently.

### Waiting policy, timeout and holder identity

- Default behavior: a mutating command waits for the lock up to a 30 second
  deadline, polling with capped exponential backoff.
- A non-blocking mode is available for callers that prefer to back off
  themselves; it makes one non-blocking attempt and fails immediately.
- On timeout the error reports which command holds the lock and its pid, read
  from the operation record, e.g. `timed out waiting for the virtual machine
  lock after 30s: held by "init" (pid 4821, ...)`, with exit code `4`.

### Read-only commands do not take the lock

`finch vm status` is intentionally lock-free so it can never be blocked by a
long-running start or stop. Because it reads without the lock it may observe a
transitional state; such output is explicitly marked (for example
`Starting (in progress)` plus an `in progress: init (pid …)` line), and a stale
holder produces a warning that the next mutating command will reconcile the
VM. A forced stop additionally prints a `needs inspection` warning.

## Signals and timeouts

### Signal policy: terminate together

On `SIGINT`/`SIGTERM` (Ctrl-C; Ctrl-Break on Windows) the signal-cancelling
context terminates the underlying tool invocation together with the CLI. The
alternative — letting the underlying action continue in the background while
the command exits — was rejected because it inherently opens a window in which
the command has exited and released the lock while driver work is still
mutating the VM, recreating exactly the "guess the intermediate state" problem
this design removes. Holding the lock after the foreground command exits would
require a detached supervisor process that Finch does not have.

With this policy the invariant is simple: the lock is released only after the
driver method has returned, and on the way out the operation record is
reconciled against a fresh driver query, so the holder record always matches
real progress. The interrupted command exits with code `6`.

### Graceful stop deadline and forced-stop escalation

A graceful `stop` runs under a 60 second deadline. If it does not complete in
time, the manager deterministically escalates to the driver's forced stop. A
forced stop — whether requested via `--force` or triggered by the deadline —
sets the `needs_check` annotation: the VM is reported as stopped but is never
presented as a clean, normal stop. The annotation clears on the next
successful start (or on removal).

## Driver abstraction and testing strategy

No test depends on a real virtual machine manager. All calls into the
underlying tool go through a small `Driver` interface (`Status`, `Init`,
`Start`, `Stop`, `ForceStop`, `Remove`).

- Production provides a Lima driver that shells out to `limactl` through
  `exec.CommandContext`, so context cancellation kills the child process and no
  invocation can outlive the command.
- Tests use an in-memory `FakeDriver` that holds its state in memory and lets
  every action be blocked on a channel gate, delayed, or scripted to fail
  (including a failure that leaves partial state behind). It records the full
  call sequence and counts overlapping calls.

The test suite covers:

- **Full state/action matrix, table-driven.** Every cell of the transition
  table is asserted twice: at the table-lookup level and at the behavior level
  (execute / no-op / reject). Adding a state or action without filling its
  cells fails the completeness test.
- **Real concurrency.** Multiple goroutines contend for one real kernel-backed
  lock file; the first command blocks inside a gated fake-driver call while the
  others either wait at the lock layer or fail fast by convention. The fake
  driver asserts that its mutating calls never overlap (`MaxConcurrent == 1`,
  zero overlap count) and observes the expected serialized call sequence.
- **Dead-holder takeover.** A lock file and operation record are left in a temp
  directory with a pid that does not exist; the next command recognizes the
  residual record, re-queries the driver and reconciles (adopt, roll back, or
  mark `Corrupted`).
- **Failures and interruptions.** Half-created init leaves `Corrupted` from
  which only `remove` works; a failed init with no instance left behind rolls
  back to `Uninitialized`; a context cancellation (and a real `SIGINT`) leaves
  a recoverable record with exit code `6`; a graceful stop past its deadline
  escalates to a forced stop flagged `needs_check`.
- **Race detector.** The package tests run clean under `go test -race`.

## Code layout

- `pkg/lifecycle/` — platform-independent state machine, driver interface,
  in-memory fake driver, operation record, signal handling, and the
  platform-specific lock and process-liveness implementations
  (`lock_unix.go`/`lock_windows.go`, `proc_unix.go`/`proc_windows.go`,
  `signal_unix.go`/`signal_windows.go`).
- `cmd/finch/lifecycle_driver.go` — the production `limactl` driver.
- `cmd/finch/lifecycle_wiring.go` — manager construction and the wrapper that
  runs the historical pre-flight steps (optional dependencies, Lima YAML, user
  data disk) inside the lock.
- `cmd/finch/virtual_machine_{init,start,stop,remove,status}.go` — the commands
  reworked to call the manager; `cmd/finch/lifecycle_exit.go` maps lifecycle
  errors to the documented exit codes.

## Behavior changes to existing commands

The following changes are deliberate and should be called out in release
notes. Several calls that previously failed now succeed as idempotent no-ops,
and generic failures are replaced by specific, stable exit codes.

| Call | Previous behavior | New behavior |
| --- | --- | --- |
| `finch vm start` while Running | Generic error `instance is already running`, non-zero exit. | Successful no-op, exit `0`, with an explicit no-op message. |
| `finch vm start` when no instance exists | Generic error telling the user to run `init`. | Rejected with exit code `2` and the same guidance; no driver call is made. |
| `finch vm start` while a stop is in progress | Could race the stop and rely on a tool-level error. | Refused immediately with exit code `3` (concurrent operation); retry after the stop finishes. |
| `finch vm stop` while already Stopped | Generic error `instance is already stopped`, non-zero exit. | Successful no-op, exit `0`. |
| `finch vm stop` on a nonexistent instance | Generic error. | Rejected with exit code `2` and a "nothing to do" message. |
| Graceful `finch vm stop` exceeding the deadline | Could hang until the underlying tool returned. | Escalates deterministically to a forced stop, marked `needs_check`; status warns that inspection is required. |
| `finch vm init` / `start` / `stop` / `remove` invoked concurrently | Ordering decided by accidental tool errors. | Serialized by a per-VM cross-process lock; conflicting transitional requests fail fast with code `3`; lock waits time out at 30s with code `4` naming the holder. |
| A process killed mid-operation | The next command had to guess the intermediate state. | The residual record is detected and reconciled from the real driver state; half-created VMs are marked `Corrupted` and are removable. |
| `finch vm status` during an operation | Could report a misleading stable word or fail. | Lock-free; reports the transitional state explicitly with the holder pid, or a stale-holder reconciliation warning. |
| `finch vm remove` on a broken half-created VM | Could be refused by the old status checks. | Always accepted in `Corrupted`; it is the unique cleanup path. |
