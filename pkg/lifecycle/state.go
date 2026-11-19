// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

// Package lifecycle implements the explicit virtual machine lifecycle state machine,
// cross-process mutual exclusion and crash recovery used by the finch vm commands.
//
// The package deliberately does not shell out to any specific hypervisor: all calls
// into the underlying virtual machine management tool go through the [Driver]
// interface, so that an in-memory fake implementation can exercise every state,
// concurrency and recovery scenario in tests without touching a real VM.
package lifecycle

import "fmt"

// State is the explicitly modeled lifecycle state of a virtual machine.
//
// The seven states are the only states any finch vm command is allowed to observe
// or assume. Starting and Stopping are transitional states and are never treated as
// stable states (see [State.IsTransitional]).
type State int

const (
	// Uninitialized means no instance has ever been created (the driver reports no VM).
	Uninitialized State = iota
	// InitializedStopped means the instance was created by init but has never
	// completed a standalone start, and the driver currently reports it as stopped.
	// Semantically equivalent to Stopped for every action; the two states only
	// differ in how status presents them to the user.
	InitializedStopped
	// Starting is the transitional state between a start (or init) request and Running.
	Starting
	// Running means the driver reports the VM as running and no operation is in progress.
	Running
	// Stopping is the transitional state between a stop (or remove) request and Stopped.
	Stopping
	// Stopped means the VM exists, the driver reports it as stopped and at least one
	// start has completed since it was created.
	Stopped
	// Corrupted means an operation was interrupted or failed after partial side
	// effects, so that the real condition of the VM cannot be trusted. The only
	// mutating action permitted in this state is Remove.
	Corrupted
)

// String returns the stable, user-facing name of the state.
//
// The names are part of the command line contract and must not be renamed:
// scripts may match on the output of `finch vm status`.
func (s State) String() string {
	switch s {
	case Uninitialized:
		return "Uninitialized"
	case InitializedStopped:
		return "Initialized, not running"
	case Starting:
		return "Starting"
	case Running:
		return "Running"
	case Stopping:
		return "Stopping"
	case Stopped:
		return "Stopped"
	case Corrupted:
		return "Corrupted"
	default:
		return fmt.Sprintf("Unknown(%d)", int(s))
	}
}

// IsTransitional reports whether the state is an in-progress, non-stable state.
//
// Read-only status commands do not take the lock and may therefore observe
// Starting or Stopping while another process holds the lock. Such output is
// explicitly marked as transitional rather than being presented as a stable state.
func (s State) IsTransitional() bool {
	return s == Starting || s == Stopping
}

// Action identifies a mutating lifecycle command.
type Action string

const (
	// ActionInit creates and first-boots the instance.
	ActionInit Action = "init"
	// ActionStart starts an existing stopped instance.
	ActionStart Action = "start"
	// ActionStop stops a running instance, escalating to force if needed.
	ActionStop Action = "stop"
	// ActionRemove deletes the instance; it is the sole mutating action allowed
	// in Corrupted.
	ActionRemove Action = "remove"
	// ActionStatus is the read-only status query. It never takes the lock.
	ActionStatus Action = "status"
)

// String implements fmt.Stringer.
func (a Action) String() string { return string(a) }

// Decision is the result of evaluating an action against a state in the
// transition table.
type Decision int

const (
	// DecisionExecute means the action's driver calls are permitted in this state.
	DecisionExecute Decision = iota
	// DecisionNoop means the requested end state already holds; the command must
	// return success (exit code 0) without calling the driver, while emitting a
	// distinguishable "no-op" message so callers can tell it apart from real work.
	DecisionNoop
	// DecisionReject means the action is illegal in this state and must be refused
	// with an [InvalidTransitionError] before any driver call.
	DecisionReject
)

// String renders the decision for diagnostics and documentation.
func (d Decision) String() string {
	switch d {
	case DecisionExecute:
		return "execute"
	case DecisionNoop:
		return "no-op"
	case DecisionReject:
		return "reject"
	default:
		return fmt.Sprintf("unknown(%d)", int(d))
	}
}

// transitionTable is the single source of truth for which action is legal in
// which state. Every cell is populated on purpose: adding a state or an action
// without filling its cells makes TestTransitionTableCompleteness fail.
//
// The policies for the three previously undecided combinations are:
//
//   - start on Running      -> no-op (idempotent success, exit code 0)
//   - start on Uninitialized -> reject, tell the caller to run init first (exit code 2)
//   - start on Stopping      -> reject, do not queue behind the in-flight stop (exit code 3)
//
// Remove is deliberately allowed from Corrupted: it is the only cleanup path for
// a broken half-created instance and must never be refused because the state is
// "illegal".
var transitionTable = map[State]map[Action]Decision{
	Uninitialized: {
		ActionInit:   DecisionExecute,
		ActionStart:  DecisionReject,
		ActionStop:   DecisionReject,
		ActionRemove: DecisionReject,
		ActionStatus: DecisionExecute,
	},
	InitializedStopped: {
		ActionInit:   DecisionReject,
		ActionStart:  DecisionExecute,
		ActionStop:   DecisionNoop,
		ActionRemove: DecisionExecute,
		ActionStatus: DecisionExecute,
	},
	Starting: {
		ActionInit:   DecisionReject,
		ActionStart:  DecisionReject,
		ActionStop:   DecisionReject,
		ActionRemove: DecisionReject,
		ActionStatus: DecisionExecute,
	},
	Running: {
		ActionInit:   DecisionReject,
		ActionStart:  DecisionNoop,
		ActionStop:   DecisionExecute,
		ActionRemove: DecisionReject,
		ActionStatus: DecisionExecute,
	},
	Stopping: {
		ActionInit:   DecisionReject,
		ActionStart:  DecisionReject,
		ActionStop:   DecisionNoop,
		ActionRemove: DecisionReject,
		ActionStatus: DecisionExecute,
	},
	Stopped: {
		ActionInit:   DecisionReject,
		ActionStart:  DecisionExecute,
		ActionStop:   DecisionNoop,
		ActionRemove: DecisionExecute,
		ActionStatus: DecisionExecute,
	},
	Corrupted: {
		ActionInit:  DecisionReject,
		ActionStart: DecisionReject,
		ActionStop:  DecisionReject,
		// Remove is the unique recovery action from Corrupted.
		ActionRemove: DecisionExecute,
		ActionStatus: DecisionExecute,
	},
}

// allStates is the canonical enumeration used by the completeness test and
// documentation generation. It must list every State constant exactly once.
var allStates = []State{
	Uninitialized,
	InitializedStopped,
	Starting,
	Running,
	Stopping,
	Stopped,
	Corrupted,
}

// allActions lists every action that has transition-table semantics.
var allActions = []Action{ActionInit, ActionStart, ActionStop, ActionRemove, ActionStatus}

// Transition looks up the policy for action in state s.
//
// It returns an error instead of a zero value if an unknown state or action is
// passed, which can only happen through a programming mistake in this package.
func Transition(s State, a Action) (Decision, error) {
	row, ok := transitionTable[s]
	if !ok {
		return DecisionReject, fmt.Errorf("%w: state %s has no transition-table entry", ErrInternal, s)
	}
	d, ok := row[a]
	if !ok {
		return DecisionReject, fmt.Errorf("%w: action %s has no transition-table entry in state %s", ErrInternal, a, s)
	}
	return d, nil
}
