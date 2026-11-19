// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package lifecycle

import (
	"errors"
	"fmt"
)

// Exit codes returned by the lifecycle manager. They are stable and documented so
// that scripts can distinguish "nothing was done, but the desired state holds"
// from an actual failure.
const (
	// ExitOK means the action either ran to completion or was a successful no-op.
	ExitOK = 0
	// ExitInvalidTransition means the action was rejected by the state machine
	// because it is illegal in the current state. The error message names the
	// current state and the prerequisite command to run instead.
	ExitInvalidTransition = 2
	// ExitConcurrent means a start/stop request collided with an in-flight
	// transitional operation that cannot be queued (e.g. start while Stopping).
	ExitConcurrent = 3
	// ExitLockTimeout means another process held the per-VM lock for longer than
	// the configured wait timeout. The error message identifies the holder.
	ExitLockTimeout = 4
	// ExitDriverFailed means an underlying driver call returned an error.
	ExitDriverFailed = 5
	// ExitInterrupted means the process received a termination signal while a
	// driver operation was in progress. The underlying action was terminated
	// together with the CLI and the VM may need inspection (it is marked
	// Corrupted or needs-check accordingly).
	ExitInterrupted = 6
	// ExitInternal means an invariant inside the lifecycle package was violated.
	// Users should never see this; it exists to avoid silently swallowing bugs.
	ExitInternal = 70
)

// Sentinel errors. Use errors.Is to match; the concrete wrappers carry details.
var (
	// ErrInternal is returned on package-internal invariant violations.
	ErrInternal = errors.New("internal lifecycle error")
	// ErrLockTimeout is wrapped by LockTimeoutError when the lock cannot be
	// acquired within the configured deadline.
	ErrLockTimeout = errors.New("timed out waiting for the virtual machine lock")
	// ErrInvalidTransition is wrapped by InvalidTransitionError.
	ErrInvalidTransition = errors.New("action is not allowed in the current state")
	// ErrConcurrentOperation is wrapped when a request collides with a
	// transitional operation instead of being queued.
	ErrConcurrentOperation = errors.New("another lifecycle operation is in progress")
	// ErrDriverFailed is wrapped when a driver call fails.
	ErrDriverFailed = errors.New("underlying virtual machine driver call failed")
	// ErrInterrupted is wrapped when a signal aborts an in-flight operation.
	ErrInterrupted = errors.New("operation interrupted by signal")
)

// InvalidTransitionError explains why an action was rejected and tells the caller
// what to do instead. Its message is part of the user-facing contract.
type InvalidTransitionError struct {
	State  State
	Action Action
	// Hint is a complete, human-readable sentence naming the prerequisite command,
	// e.g. `run "finch vm init" first`.
	Hint string
}

// Error implements error.
func (e *InvalidTransitionError) Error() string {
	return fmt.Sprintf("%s: %q is not allowed while the virtual machine is %s; %s",
		ErrInvalidTransition, e.Action, e.State, e.Hint)
}

// Is enables errors.Is(err, ErrInvalidTransition).
func (e *InvalidTransitionError) Is(target error) bool { return target == ErrInvalidTransition }

// LockTimeoutError carries the identity of the lock holder so that the user can
// decide whether to wait longer or investigate the holder.
type LockTimeoutError struct {
	// Holder is the content of the stale-or-live operation record, or nil if the
	// lock is held but no record can be read.
	Holder *OperationRecord
	// Timeout is how long the caller waited before giving up.
	Timeout Duration
}

// Error implements error.
func (e *LockTimeoutError) Error() string {
	if e.Holder == nil {
		return fmt.Sprintf("%s after %s (holder information unavailable)", ErrLockTimeout, e.Timeout)
	}
	return fmt.Sprintf("%s after %s: held by %q (pid %d, started %s)",
		ErrLockTimeout, e.Timeout, e.Holder.Action, e.Holder.PID, e.Holder.StartedAt.Format(timeStampLayout))
}

// Is enables errors.Is(err, ErrLockTimeout).
func (e *LockTimeoutError) Is(target error) bool { return target == ErrLockTimeout }

// DriverError wraps a failure returned by the underlying driver together with the
// action that triggered it.
type DriverError struct {
	Action Action
	Err    error
}

// Error implements error.
func (e *DriverError) Error() string {
	return fmt.Sprintf("%s: %s: %v", ErrDriverFailed, e.Action, e.Err)
}

// Unwrap supports errors.Is/errors.As into the underlying driver error.
func (e *DriverError) Unwrap() error { return e.Err }

// Is enables errors.Is(err, ErrDriverFailed).
func (e *DriverError) Is(target error) bool { return target == ErrDriverFailed }

// InterruptedError records which action was aborted by a signal.
type InterruptedError struct {
	Action Action
}

// Error implements error.
func (e *InterruptedError) Error() string {
	return fmt.Sprintf("%s: %s was terminated before completing", ErrInterrupted, e.Action)
}

// Is enables errors.Is(err, ErrInterrupted).
func (e *InterruptedError) Is(target error) bool { return target == ErrInterrupted }

// ExitCode maps a lifecycle error to its documented exit code. Unknown errors
// map to ExitDriverFailed, and nil maps to ExitOK.
func ExitCode(err error) int {
	switch {
	case err == nil:
		return ExitOK
	case errors.Is(err, ErrInvalidTransition):
		return ExitInvalidTransition
	case errors.Is(err, ErrConcurrentOperation):
		return ExitConcurrent
	case errors.Is(err, ErrLockTimeout):
		return ExitLockTimeout
	case errors.Is(err, ErrInterrupted):
		return ExitInterrupted
	case errors.Is(err, ErrInternal):
		return ExitInternal
	default:
		return ExitDriverFailed
	}
}
