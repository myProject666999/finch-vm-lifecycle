// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// waitChan is a tiny indirection over time.After so tests can avoid real timers
// if they wish; production uses the real clock.
func waitChan(d time.Duration) <-chan time.Time { return time.After(d) }

// beginRecord writes the in-progress record for the holder. The lock is already
// held at this point, so the record and the lock can never disagree about who the
// holder is.
func (m *Manager) beginRecord(action Action, rec *OperationRecord) (*OperationRecord, error) {
	if rec == nil {
		rec = &OperationRecord{}
	}
	rec.Action = action
	rec.PID = currentPID()
	now := m.cfg.Clock.Now()
	if rec.StartedAt.IsZero() {
		rec.StartedAt = now
	}
	rec.Corrupted = false
	if err := m.store.save(rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// execute performs the driver calls of an accepted action while the lock is
// held. Every error path re-queries the driver so that the persisted record and
// the derived state match reality instead of guessing.
func (m *Manager) execute(ctx context.Context, action Action, force bool, rec *OperationRecord) (*ActionResult, error) {
	rec, err := m.beginRecord(action, rec)
	if err != nil {
		return nil, err
	}

	switch action {
	case ActionInit:
		return m.finish(ctx, action, rec, func(c context.Context) error { return m.driver.Init(c) },
			func(r *OperationRecord) {
				r.BootCompleted = true
			})
	case ActionStart:
		return m.finish(ctx, action, rec, func(c context.Context) error { return m.driver.Start(c) },
			func(r *OperationRecord) {
				r.BootCompleted = true
				r.NeedsCheck = false
			})
	case ActionStop:
		return m.executeStop(ctx, rec, force)
	case ActionRemove:
		return m.executeRemove(ctx, rec, force)
	default:
		return nil, fmt.Errorf("%w: unknown action %q", ErrInternal, action)
	}
}

// finish runs one driver action, handles interruption/failure reconciliation and
// writes the post-success annotations.
func (m *Manager) finish(
	ctx context.Context,
	action Action,
	rec *OperationRecord,
	driverCall func(context.Context) error,
	onSuccess func(*OperationRecord),
) (*ActionResult, error) {
	err := driverCall(ctx)
	if err != nil {
		return m.handleDriverError(ctx, action, rec, err)
	}
	onSuccess(rec)
	rec.LastCompletedAction = action
	rec.Action = ""
	if err := m.store.save(rec); err != nil {
		return nil, err
	}
	return &ActionResult{Action: action, NeedsCheck: rec.NeedsCheck, Message: successMessage(action)}, nil
}

func (m *Manager) executeStop(ctx context.Context, rec *OperationRecord, force bool) (*ActionResult, error) {
	if force {
		return m.forceStop(ctx, rec, ActionStop)
	}
	stopCtx, cancel := context.WithTimeout(ctx, m.cfg.StopTimeout)
	defer cancel()

	err := m.driver.Stop(stopCtx)
	switch {
	case err == nil:
		rec.LastCompletedAction = ActionStop
		rec.Action = ""
		if err := m.store.save(rec); err != nil {
			return nil, err
		}
		return &ActionResult{Action: ActionStop, Message: successMessage(ActionStop)}, nil
	case errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
		// Graceful stop exceeded its deadline while the caller is still waiting:
		// escalate deterministically rather than leaving the VM hanging.
		return m.forceStop(ctx, rec, ActionStop)
	default:
		return m.handleDriverError(ctx, ActionStop, rec, err)
	}
}

// forceStop calls the driver's ForceStop and marks the VM as needing inspection.
// A forced stop is never presented as a clean, normal stop.
func (m *Manager) forceStop(ctx context.Context, rec *OperationRecord, action Action) (*ActionResult, error) {
	if err := m.driver.ForceStop(ctx); err != nil {
		return m.handleDriverError(ctx, action, rec, err)
	}
	rec.NeedsCheck = true
	rec.LastCompletedAction = ActionStop
	rec.Action = ""
	if err := m.store.save(rec); err != nil {
		return nil, err
	}
	return &ActionResult{
		Action:     action,
		NeedsCheck: true,
		Message:    "virtual machine was forcibly stopped and needs inspection; run `finch vm start` to verify it",
	}, nil
}

func (m *Manager) executeRemove(ctx context.Context, rec *OperationRecord, force bool) (*ActionResult, error) {
	if force {
		// A forced remove force-stops a running VM first; that half of the work
		// also leaves needs-check semantics, although removal immediately
		// follows and wipes the instance anyway.
		state, err := m.queryDriver(ctx)
		if err != nil {
			return nil, err
		}
		if state == DriverRunning {
			if err := m.driver.ForceStop(ctx); err != nil {
				return m.handleDriverError(ctx, ActionRemove, rec, err)
			}
		}
	}
	if err := m.driver.Remove(ctx, force); err != nil {
		return m.handleDriverError(ctx, ActionRemove, rec, err)
	}
	// The instance is gone, so every annotation goes with it.
	if err := m.store.clear(); err != nil {
		return nil, err
	}
	return &ActionResult{Action: ActionRemove, Message: successMessage(ActionRemove)}, nil
}

// handleDriverError is the single recovery path for failed driver calls.
//
// Policy:
//
//   - If the caller's context was cancelled (SIGINT/SIGTERM delivered to the
//     CLI), the underlying driver call was terminated together with the CLI. The
//     record is re-derived from a fresh driver query: a half-created/half-started
//     instance becomes Corrupted, a completed-but-unreported result is adopted.
//   - Any other driver failure triggers the same reconciliation. Init failing
//     after partial creation is the important case: it must leave Corrupted so
//     that the only accepted next action is remove.
func (m *Manager) handleDriverError(ctx context.Context, action Action, rec *OperationRecord, callErr error) (*ActionResult, error) {
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(callErr, context.Canceled) {
		return m.interrupted(ctx, action, rec)
	}

	driverState, qErr := m.queryDriver(context.WithoutCancel(ctx))
	if qErr != nil {
		// Even the reconciliation query failed: the safest assumption is that the
		// VM is not in a trustworthy state.
		rec.Corrupted = true
		rec.Action = ""
		_ = m.store.save(rec)
		return nil, fmt.Errorf("%w: %w (state could not be verified, marked Corrupted)",
			&DriverError{Action: action, Err: callErr}, qErr)
	}

	switch action {
	case ActionInit, ActionStart:
		if driverState == DriverRunning {
			// The action actually succeeded; the failure came from a trailing step.
			rec.BootCompleted = true
			rec.LastCompletedAction = action
			rec.Action = ""
			if err := m.store.save(rec); err != nil {
				return nil, err
			}
			return &ActionResult{Action: action, Message: successMessage(action)}, nil
		}
		if action == ActionInit && driverState == DriverAbsent {
			// Nothing was created: fully roll back so the user can simply retry init.
			if err := m.store.clear(); err != nil {
				return nil, err
			}
		} else {
			// A half-created or half-started instance: Corrupted, remove-only.
			rec.Corrupted = true
			rec.Action = ""
			if err := m.store.save(rec); err != nil {
				return nil, err
			}
		}
	case ActionStop:
		if driverState == DriverStopped {
			rec.LastCompletedAction = ActionStop
			rec.Action = ""
			if err := m.store.save(rec); err != nil {
				return nil, err
			}
			return &ActionResult{Action: ActionStop, Message: successMessage(ActionStop)}, nil
		}
		rec.Action = ""
		_ = m.store.save(rec)
	case ActionRemove:
		if driverState == DriverAbsent {
			_ = m.store.clear()
			return &ActionResult{Action: ActionRemove, Message: successMessage(ActionRemove)}, nil
		}
		rec.Corrupted = true
		rec.Action = ""
		_ = m.store.save(rec)
	}

	return nil, &DriverError{Action: action, Err: callErr}
}

// interrupted reconciles an operation aborted by a signal. The chosen signal
// policy is "terminate together": the CLI never returns while an underlying tool
// invocation keeps running, so there is no window in which the lock has been
// released while driver work is still in flight.
func (m *Manager) interrupted(ctx context.Context, action Action, rec *OperationRecord) (*ActionResult, error) {
	// Reconciliation re-queries the driver first and only then adopts a completed
	// result, rolls back or marks the VM Corrupted.
	reconciled, qErr := m.reconcileStale(context.WithoutCancel(ctx), rec)
	if qErr != nil {
		rec.Corrupted = true
		reconciled = rec
	}
	if reconciled.IsEmpty() {
		if err := m.store.clear(); err != nil {
			return nil, err
		}
		return nil, &InterruptedError{Action: action}
	}
	// The interrupted command itself is the (former) holder. Drop the holder
	// fields regardless of the reconciliation outcome so that this same process
	// is not rejected by the lock-free pre-check on its next command; the
	// Corrupted annotation, when set, still enforces remove-only semantics.
	reconciled.Action = ""
	reconciled.PID = 0
	reconciled.StartedAt = time.Time{}
	if err := m.store.save(reconciled); err != nil {
		return nil, err
	}
	return nil, &InterruptedError{Action: action}
}

func successMessage(action Action) string {
	switch action {
	case ActionInit:
		return "virtual machine initialized and started successfully"
	case ActionStart:
		return "virtual machine started successfully"
	case ActionStop:
		return "virtual machine stopped successfully"
	case ActionRemove:
		return "virtual machine removed successfully"
	default:
		return fmt.Sprintf("%s completed", action)
	}
}
