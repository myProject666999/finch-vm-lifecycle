// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package lifecycle

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTransitionTableCompleteness fails unless every state/action combination has
// an explicit expectation. The want matrix below is the documented transition
// table in executable form: adding a state or an action without filling its cells
// makes this test fail, so no cell can silently fall through to a default.
func TestTransitionTableCompleteness(t *testing.T) {
	t.Parallel()

	want := map[State]map[Action]Decision{
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
			ActionInit:   DecisionReject,
			ActionStart:  DecisionReject,
			ActionStop:   DecisionReject,
			ActionRemove: DecisionExecute,
			ActionStatus: DecisionExecute,
		},
	}

	covered := 0
	for _, s := range allStates {
		row, ok := want[s]
		require.True(t, ok, "test matrix is missing an entire row for state %s", s)
		for _, a := range allActions {
			expected, ok := row[a]
			require.True(t, ok, "test matrix is missing cell %s/%s", s, a)

			got, err := Transition(s, a)
			require.NoError(t, err, "transition table has no entry for %s/%s", s, a)
			assert.Equal(t, expected, got, "cell %s/%s", s, a)
			covered++
		}
	}

	// Guard against someone shrinking the enumeration: all seven states and five
	// actions must be present.
	require.Len(t, allStates, 7)
	require.Len(t, allActions, 5)
	require.Equal(t, 7*5, covered)
}

func TestStateProperties(t *testing.T) {
	t.Parallel()

	assert.True(t, Starting.IsTransitional())
	assert.True(t, Stopping.IsTransitional())
	for _, s := range []State{Uninitialized, InitializedStopped, Running, Stopped, Corrupted} {
		assert.False(t, s.IsTransitional(), "%s must not be reported as transitional", s)
	}

	// Stable names are part of the CLI contract.
	assert.Equal(t, "Uninitialized", Uninitialized.String())
	assert.Equal(t, "Initialized, not running", InitializedStopped.String())
	assert.Equal(t, "Starting", Starting.String())
	assert.Equal(t, "Running", Running.String())
	assert.Equal(t, "Stopping", Stopping.String())
	assert.Equal(t, "Stopped", Stopped.String())
	assert.Equal(t, "Corrupted", Corrupted.String())
}

func TestExitCodeMapping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, ExitOK},
		{"invalid transition", &InvalidTransitionError{State: Running, Action: ActionRemove, Hint: "x"}, ExitInvalidTransition},
		{"unclassified error defaults to driver failure", errors.New("wrap"), ExitDriverFailed},
		{"lock timeout sentinel", ErrLockTimeout, ExitLockTimeout},
		{"lock timeout typed", &LockTimeoutError{Timeout: DefaultLockTimeout}, ExitLockTimeout},
		{"concurrent sentinel", ErrConcurrentOperation, ExitConcurrent},
		{"driver", &DriverError{Action: ActionStart, Err: errors.New("boom")}, ExitDriverFailed},
		{"interrupted", &InterruptedError{Action: ActionStart}, ExitInterrupted},
		{"internal", ErrInternal, ExitInternal},
		{"plain error", errors.New("unclassified"), ExitDriverFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, ExitCode(tc.err))
		})
	}

	// errors.Is must see through the typed wrappers.
	assert.True(t, errors.Is(&InvalidTransitionError{State: Stopped, Action: ActionInit, Hint: "x"}, ErrInvalidTransition))
	assert.True(t, errors.Is(&LockTimeoutError{Timeout: 1}, ErrLockTimeout))
	assert.True(t, errors.Is(&DriverError{Action: ActionStop, Err: errors.New("x")}, ErrDriverFailed))
	assert.True(t, errors.Is(&InterruptedError{Action: ActionStop}, ErrInterrupted))
}

func TestDriverStateString(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "absent", DriverAbsent.String())
	assert.Equal(t, "running", DriverRunning.String())
	assert.Equal(t, "stopped", DriverStopped.String())
	assert.Equal(t, "unknown", DriverState(99).String())
}

func TestActionString(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "init", ActionInit.String())
	assert.Equal(t, "start", ActionStart.String())
	assert.Equal(t, "stop", ActionStop.String())
	assert.Equal(t, "remove", ActionRemove.String())
	assert.Equal(t, "status", ActionStatus.String())
}

func TestDecisionString(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "execute", DecisionExecute.String())
	assert.Equal(t, "no-op", DecisionNoop.String())
	assert.Equal(t, "reject", DecisionReject.String())
	assert.Contains(t, Decision(99).String(), "unknown")
}
