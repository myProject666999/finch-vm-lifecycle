// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// Manager runs lifecycle actions behind the cross-process lock and on top of an
// explicit state machine. One Manager is cheap to create; CLI commands construct
// one per invocation with a production Driver, while tests reuse a single
// Manager against the fake driver.
type Manager struct {
	cfg    Config
	driver Driver
	store  *recordStore
}

// NewManager builds a Manager. Defaults from [DefaultConfig] are filled in for
// any zero-valued timing or dependency field.
func NewManager(cfg Config, driver Driver) *Manager {
	cfg.withDefaults()
	return &Manager{
		cfg:    cfg,
		driver: driver,
		store:  newRecordStore(cfg.Fs, cfg.StateDir),
	}
}

// StatusResult is the lock-free view of the virtual machine.
type StatusResult struct {
	// State is the derived lifecycle state. Transitional states must be presented
	// to users as such rather than as a stable state.
	State State
	// NeedsCheck is true after a forced stop; it asks the caller to verify the VM
	// by starting it. It is only meaningful for Stopped/Stopping.
	NeedsCheck bool
	// Holder describes the in-progress operation when State is transitional. It
	// may describe a dead process (StaleHolder == true), in which case the next
	// mutating command will take over and reconcile.
	Holder *OperationRecord
	// StaleHolder is true when Holder names a process that no longer exists.
	StaleHolder bool
}

// ActionResult reports what a mutating command actually did.
type ActionResult struct {
	Action Action
	// Noop is true when the requested end state already held and no driver call
	// was made. The command still exits 0; scripts distinguish this from real
	// work via the printed no-op message and, when using the Go API, this flag.
	Noop bool
	// NeedsCheck mirrors the record annotation after the action (relevant after
	// a forced stop).
	NeedsCheck bool
	// Message is a stable, human-readable summary suitable for the CLI output.
	Message string
}

// Status reads the current state without taking the lock.
//
// Because it is lock-free it may observe a transitional state written by a
// concurrent mutating process; that condition is returned explicitly instead of
// being flattened into a stable state.
func (m *Manager) Status(ctx context.Context) (StatusResult, error) {
	rec, err := m.store.load()
	switch {
	case errors.Is(err, errNoOperationRecord):
		rec = nil
	case errors.Is(err, errCorruptRecord):
		// rec carries the Corrupted marker and is handled below.
	case err != nil:
		return StatusResult{}, err
	}
	if rec != nil && rec.Corrupted {
		return StatusResult{
			State: Corrupted, NeedsCheck: rec.NeedsCheck, Holder: rec,
			StaleHolder: m.isStale(rec),
		}, nil
	}

	driverState, err := m.queryDriver(ctx)
	if err != nil {
		return StatusResult{}, err
	}
	return m.deriveStatus(driverState, rec), nil
}

// Init runs the init action under the lock and state machine.
func (m *Manager) Init(ctx context.Context) (*ActionResult, error) {
	return m.runMutating(ctx, ActionInit, false)
}

// Start runs the start action under the lock and state machine.
func (m *Manager) Start(ctx context.Context) (*ActionResult, error) {
	return m.runMutating(ctx, ActionStart, false)
}

// Stop runs the stop action. When force is true it maps directly onto the
// driver's ForceStop and sets the needs-check annotation.
func (m *Manager) Stop(ctx context.Context, force bool) (*ActionResult, error) {
	return m.runMutating(ctx, ActionStop, force)
}

// Remove runs the remove action. When force is true a running VM is force
// stopped before removal. Remove is the only action accepted in Corrupted.
func (m *Manager) Remove(ctx context.Context, force bool) (*ActionResult, error) {
	return m.runMutating(ctx, ActionRemove, force)
}

// runMutating is the single entry point for every state-changing action, so that
// init/start/stop/remove share one lock, one state read and one recovery path.
func (m *Manager) runMutating(ctx context.Context, action Action, force bool) (*ActionResult, error) {
	// Lock-free pre-check. A live holder mid-operation means the VM is in a
	// transitional state; conflicting requests are refused immediately (exit code
	// 3) instead of queueing behind the lock, which is the chosen policy for
	// e.g. start while the VM is Stopping. A no-op request such as stop while
	// Stopping returns success without waiting.
	pre := m.precheckLiveOperation(action)
	if pre.handled {
		return pre.noopResult, pre.err
	}

	// A fresh fileLock per operation: all instances point at the same lock file
	// and the kernel serializes them, while each instance owns its descriptor
	// privately so concurrent goroutines never share mutable state.
	lock := newFileLock(m.cfg.StateDir)
	if err := m.acquireLock(ctx, lock); err != nil {
		return nil, err
	}
	defer func() { _ = lock.release() }()

	rec, err := m.store.load()
	switch {
	case errors.Is(err, errNoOperationRecord):
		rec = nil
	case errors.Is(err, errCorruptRecord):
		// rec carries the Corrupted marker; currentState maps it to Corrupted.
	case err != nil:
		return nil, err
	}
	// The kernel guarantees nobody else holds the lock, so any in-progress record
	// found here is residual state from a previous process. Reconcile it against
	// the real driver before deciding anything.
	if rec != nil && rec.Action != "" {
		rec, err = m.reconcileStale(ctx, rec)
		if err != nil {
			return nil, err
		}
	}

	state, err := m.currentState(ctx, rec)
	if err != nil {
		return nil, err
	}

	decision, err := Transition(state, action)
	if err != nil {
		return nil, err
	}
	switch decision {
	case DecisionNoop:
		return &ActionResult{
			Action:     action,
			Noop:       true,
			NeedsCheck: rec != nil && rec.NeedsCheck,
			Message:    m.noopMessage(action, state),
		}, nil
	case DecisionReject:
		return nil, m.rejectError(state, action)
	}

	return m.execute(ctx, action, force, rec)
}

// precheckResult is the outcome of the lock-free pre-check. When handled is
// true the mutating command must short-circuit with either noopResult (a
// successful no-op) or err (a rejection). When false the caller proceeds to
// acquire the lock.
type precheckResult struct {
	handled    bool
	noopResult *ActionResult
	err        error
}

// precheckLiveOperation inspects the operation record without taking the lock.
// When a live holder is mid-operation it applies the transition table to the
// transitional state directly: rejects become concurrent-operation errors and
// no-ops stay successful no-ops. A stale (dead-holder) record is ignored here;
// the lock-acquiring path takes it over and reconciles it.
func (m *Manager) precheckLiveOperation(action Action) precheckResult {
	rec, err := m.store.load()
	switch {
	case errors.Is(err, errNoOperationRecord):
		return precheckResult{}
	case errors.Is(err, errCorruptRecord):
		// A corrupt record cannot name a live holder; let the locked path
		// reconcile it.
		return precheckResult{}
	case err != nil:
		return precheckResult{handled: true, err: err}
	}
	if rec.Action == "" || m.isStale(rec) {
		return precheckResult{}
	}
	transitional := transitionalFor(rec.Action)
	decision, err := Transition(transitional, action)
	if err != nil {
		return precheckResult{handled: true, err: err}
	}
	switch decision {
	case DecisionNoop:
		return precheckResult{
			handled: true,
			noopResult: &ActionResult{
				Action:     action,
				Noop:       true,
				NeedsCheck: rec.NeedsCheck,
				Message:    m.noopMessage(action, transitional),
			},
		}
	case DecisionReject:
		return precheckResult{handled: true, err: m.rejectError(transitional, action)}
	default:
		// The table contains no execute cell for a transitional state; reaching
		// here means the table and this pre-check disagree.
		return precheckResult{
			handled: true,
			err:     fmt.Errorf("%w: unexpected execute decision for transitional state %s", ErrInternal, transitional),
		}
	}
}

// acquireLock implements the blocking/non-blocking policy and enriches timeout
// errors with the holder identity read from the operation record.
func (m *Manager) acquireLock(ctx context.Context, lock *fileLock) error {
	if err := m.cfg.Fs.MkdirAll(m.cfg.StateDir, 0o700); err != nil {
		return fmt.Errorf("%w: creating state directory: %w", ErrInternal, err)
	}
	var err error
	if m.cfg.NonBlocking {
		var got bool
		got, err = lock.AcquireNoWait()
		if err != nil {
			return err
		}
		if !got {
			err = ErrLockTimeout
		}
	} else {
		err = lock.Acquire(ctx, m.cfg.LockTimeout)
	}
	if errors.Is(err, ErrLockTimeout) {
		return m.lockTimeoutError()
	}
	return err
}

func (m *Manager) lockTimeoutError() error {
	holder, loadErr := m.store.load()
	if loadErr != nil || holder == nil || holder.Action == "" {
		return &LockTimeoutError{Holder: nil, Timeout: m.cfg.LockTimeout}
	}
	if m.isStale(holder) {
		// flock cannot outlive its holder, so a stale record under a contended
		// lock means pid reuse; report it but keep the timeout semantics.
		holder = cloneRecord(holder)
	}
	return &LockTimeoutError{Holder: holder, Timeout: m.cfg.LockTimeout}
}

func (m *Manager) isStale(rec *OperationRecord) bool {
	return rec != nil && rec.PID != os.Getpid() && !m.cfg.ProcessChecker.Alive(rec.PID)
}

// currentState derives the authoritative state from a fresh driver query plus
// the persisted annotations.
func (m *Manager) currentState(ctx context.Context, rec *OperationRecord) (State, error) {
	if rec != nil && rec.Corrupted {
		return Corrupted, nil
	}
	driverState, err := m.queryDriver(ctx)
	if err != nil {
		return 0, err
	}
	return m.deriveStatus(driverState, rec).State, nil
}

// deriveStatus maps a driver state plus record annotations onto the seven-state
// model. A live in-progress record makes the state transitional; a stale
// in-progress record is surfaced as transitional too, with StaleHolder set, so
// that lock-free status never hides an interrupted operation.
func (m *Manager) deriveStatus(driverState DriverState, rec *OperationRecord) StatusResult {
	inProgress := rec != nil && rec.Action != ""
	stale := m.isStale(rec)
	switch driverState {
	case DriverAbsent:
		if inProgress {
			return StatusResult{
				State:       transitionalFor(rec.Action),
				Holder:      rec,
				StaleHolder: stale,
			}
		}
		return StatusResult{State: Uninitialized}
	case DriverRunning:
		if inProgress {
			return StatusResult{
				State:       transitionalFor(rec.Action),
				Holder:      rec,
				StaleHolder: stale,
				NeedsCheck:  rec.NeedsCheck,
			}
		}
		return StatusResult{State: Running, NeedsCheck: rec != nil && rec.NeedsCheck}
	case DriverStopped:
		if inProgress {
			return StatusResult{
				State:       transitionalFor(rec.Action),
				Holder:      rec,
				StaleHolder: stale,
				NeedsCheck:  rec.NeedsCheck,
			}
		}
		if rec == nil || !rec.BootCompleted {
			return StatusResult{State: InitializedStopped}
		}
		return StatusResult{State: Stopped, NeedsCheck: rec.NeedsCheck}
	default:
		return StatusResult{State: Corrupted}
	}
}

// transitionalFor maps an in-progress action onto its transitional state.
func transitionalFor(a Action) State {
	switch a {
	case ActionStop, ActionRemove:
		return Stopping
	default:
		return Starting
	}
}

// queryDriver asks the driver for the real VM state, retrying once per backoff
// while it returns ErrDriverNotReady, up to the configured status-retry budget.
func (m *Manager) queryDriver(ctx context.Context) (DriverState, error) {
	deadline := m.cfg.Clock.Now().Add(m.cfg.StatusRetry)
	var lastErr error
	for {
		state, err := m.driver.Status(ctx)
		if err == nil {
			return state, nil
		}
		if !errors.Is(err, ErrDriverNotReady) {
			return 0, &DriverError{Action: ActionStatus, Err: err}
		}
		lastErr = err
		if !m.cfg.Clock.Now().Before(deadline) {
			return 0, &DriverError{Action: ActionStatus, Err: lastErr}
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-waitChan(retryBackoff):
		}
	}
}

// reconcileStale takes over an operation record whose holder no longer owns the
// lock. It always re-queries the driver first and only then decides whether to
// complete the interrupted work, roll back to a retryable state, or flag the VM
// as Corrupted. The rewritten record is persisted before returning.
func (m *Manager) reconcileStale(ctx context.Context, rec *OperationRecord) (*OperationRecord, error) {
	rec = cloneRecord(rec)
	driverState, err := m.queryDriver(ctx)
	if err != nil {
		return nil, err
	}

	switch rec.Action {
	case ActionInit, ActionStart:
		switch driverState {
		case DriverRunning:
			// The start actually finished before the holder died. Adopt the result.
			rec.BootCompleted = true
			rec.LastCompletedAction = rec.Action
			rec.Action = ""
		case DriverAbsent:
			if rec.Action == ActionInit {
				// Nothing was ever created: roll back to the retryable initial state.
				rec = &OperationRecord{}
			} else {
				rec.Corrupted = true
			}
		case DriverStopped:
			// Interrupted mid-boot: the half-started instance cannot be trusted.
			rec.Corrupted = true
		}
	case ActionStop:
		switch driverState {
		case DriverStopped:
			// The stop effectively completed; keep needs-check if it was forced.
			rec.LastCompletedAction = ActionStop
			rec.Action = ""
		case DriverRunning:
			// Still running: roll the record back to a clean retryable state.
			rec.Action = ""
		case DriverAbsent:
			rec = &OperationRecord{}
		}
	case ActionRemove:
		switch driverState {
		case DriverAbsent:
			// Removal completed before the holder died.
			rec = &OperationRecord{}
		default:
			// Half-removed instance: only another remove may touch it.
			rec.Corrupted = true
		}
	}

	if rec.IsEmpty() {
		if err := m.store.clear(); err != nil {
			return nil, err
		}
		return rec, nil
	}
	if err := m.store.save(rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// IsEmpty reports whether the record carries no action or annotation.
func (r *OperationRecord) IsEmpty() bool {
	return r == nil || (r.Action == "" && !r.Corrupted && !r.NeedsCheck && !r.BootCompleted &&
		r.LastCompletedAction == "")
}

func cloneRecord(r *OperationRecord) *OperationRecord {
	if r == nil {
		return nil
	}
	cp := *r
	return &cp
}

func (m *Manager) rejectError(state State, action Action) error {
	if state.IsTransitional() {
		return fmt.Errorf("%w: cannot %s while the virtual machine is %s; wait for the in-progress operation to finish and retry",
			ErrConcurrentOperation, action, state)
	}
	return &InvalidTransitionError{State: state, Action: action, Hint: hintFor(state, action)}
}

func hintFor(state State, action Action) string {
	switch {
	case state == Uninitialized && action == ActionStart:
		return fmt.Sprintf(`run "finch vm %s" to create the instance first`, ActionInit)
	case state == Uninitialized && (action == ActionStop || action == ActionRemove):
		return `nothing to do: the virtual machine does not exist`
	case state == Corrupted:
		return fmt.Sprintf(`run "finch vm %s --force" to remove the broken instance, then run "finch vm %s" again`,
			ActionRemove, ActionInit)
	case state == Running && action == ActionRemove:
		return fmt.Sprintf(`run "finch vm %s" first, or pass --force`, ActionStop)
	case state == Running && action == ActionInit:
		return fmt.Sprintf(`the instance is already running; run "finch vm %s" and "finch vm %s" to recreate it`,
			ActionStop, ActionRemove)
	case (state == Stopped || state == InitializedStopped) && action == ActionInit:
		return fmt.Sprintf(`the instance already exists; run "finch vm %s" to start it`, ActionStart)
	default:
		return "check `finch vm status` and run the prerequisite command"
	}
}

func (m *Manager) noopMessage(action Action, state State) string {
	switch action {
	case ActionStart:
		return fmt.Sprintf("virtual machine is already %s; nothing to do", state)
	case ActionStop:
		return fmt.Sprintf("virtual machine is already %s; nothing to do", state)
	default:
		return fmt.Sprintf("virtual machine is %s; %s did nothing", state, action)
	}
}
