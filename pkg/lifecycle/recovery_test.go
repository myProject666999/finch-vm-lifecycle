// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package lifecycle

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const deadPID = 424242

// seedStale writes an in-progress record owned by a process that the fake process
// checker reports as dead, exactly what a crashed previous finch process leaves
// behind in a real state directory.
func (e *testEnv) seedStale(action Action, started time.Time) {
	e.t.Helper()
	e.proc.MarkDead(deadPID)
	e.seedRecord(&OperationRecord{Action: action, PID: deadPID, StartedAt: started, BootCompleted: true})
}

// TestStaleHolderTakeover covers every reconciliation branch: the new process
// must re-query the driver first, then adopt a completed result, roll back to a
// retryable state, or flag Corrupted — never trust the residual record alone.
func TestStaleHolderTakeover(t *testing.T) {
	t.Parallel()

	started := time.Now().UTC().Add(-time.Minute)

	t.Run("start finished before the holder died: adopt Running", func(t *testing.T) {
		t.Parallel()
		env := newTestEnv(t, DriverRunning)
		env.seedStale(ActionStart, started)

		st, err := env.manager.Status(context.Background())
		require.NoError(t, err)
		assert.True(t, st.StaleHolder)
		assert.Equal(t, Starting, st.State, "lock-free status still shows the interrupted operation")

		// Any mutating command takes over and reconciles.
		res, err := env.manager.Stop(context.Background(), false)
		require.NoError(t, err)
		assert.False(t, res.Noop)

		st, err = env.manager.Status(context.Background())
		require.NoError(t, err)
		assert.Equal(t, Stopped, st.State)
		rec := env.loadRecord()
		require.NotNil(t, rec)
		assert.Empty(t, rec.Action)
		assert.True(t, rec.BootCompleted)
	})

	t.Run("start interrupted mid-boot: mark Corrupted, only remove works", func(t *testing.T) {
		t.Parallel()
		env := newTestEnv(t, DriverStopped)
		env.seedStale(ActionStart, started)

		_, err := env.manager.Start(context.Background())
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrInvalidTransition))

		st, err := env.manager.Status(context.Background())
		require.NoError(t, err)
		assert.Equal(t, Corrupted, st.State)

		_, err = env.manager.Remove(context.Background(), false)
		require.NoError(t, err)
		st, err = env.manager.Status(context.Background())
		require.NoError(t, err)
		assert.Equal(t, Uninitialized, st.State)
	})

	t.Run("init died before creating anything: full rollback, init retries", func(t *testing.T) {
		t.Parallel()
		env := newTestEnv(t, DriverAbsent)
		env.proc.MarkDead(deadPID)
		env.seedRecord(&OperationRecord{Action: ActionInit, PID: deadPID, StartedAt: started})

		_, err := env.manager.Init(context.Background())
		require.NoError(t, err)
		assert.Equal(t, DriverRunning, env.driver.State())
	})

	t.Run("stop effectively completed: adopt Stopped", func(t *testing.T) {
		t.Parallel()
		env := newTestEnv(t, DriverStopped)
		env.seedStale(ActionStop, started)

		res, err := env.manager.Stop(context.Background(), false)
		require.NoError(t, err)
		assert.True(t, res.Noop, "second stop is a no-op after takeover")
		st, err := env.manager.Status(context.Background())
		require.NoError(t, err)
		assert.Equal(t, Stopped, st.State)
	})

	t.Run("stop died while still Running: retryable stop", func(t *testing.T) {
		t.Parallel()
		env := newTestEnv(t, DriverRunning)
		env.seedStale(ActionStop, started)

		_, err := env.manager.Stop(context.Background(), false)
		require.NoError(t, err)
		assert.Equal(t, DriverStopped, env.driver.State())
	})

	t.Run("remove completed before holder died: adopt Uninitialized", func(t *testing.T) {
		t.Parallel()
		env := newTestEnv(t, DriverAbsent)
		env.seedStale(ActionRemove, started)

		st, err := env.manager.Status(context.Background())
		require.NoError(t, err)
		assert.Equal(t, Stopping, st.State) // a remove in progress maps to the Stopping transitional state

		_, err = env.manager.Init(context.Background())
		require.NoError(t, err)
		st, err = env.manager.Status(context.Background())
		require.NoError(t, err)
		assert.Equal(t, Running, st.State)
	})

	t.Run("half-removed instance: Corrupted and remove-only", func(t *testing.T) {
		t.Parallel()
		env := newTestEnv(t, DriverStopped)
		env.seedStale(ActionRemove, started)

		_, err := env.manager.Start(context.Background())
		require.Error(t, err)
		_, err = env.manager.Remove(context.Background(), false)
		require.NoError(t, err)
		assert.Equal(t, DriverAbsent, env.driver.State())
	})

	t.Run("takeover always re-queries the driver instead of trusting the record", func(t *testing.T) {
		t.Parallel()
		// Record claims a stop in progress; the driver actually reports Running.
		env := newTestEnv(t, DriverRunning)
		env.seedStale(ActionStop, started)
		env.driver.FailStatus(1, ErrDriverNotReady)

		// The first driver query is transiently unavailable; reconciliation
		// retries and proceeds against the real Running state rather than guessing.
		_, err := env.manager.Stop(context.Background(), false)
		require.NoError(t, err)
		assert.Equal(t, DriverStopped, env.driver.State())
	})
}

// TestCorruptJSONRecordIsResidual verifies that an unreadable operation.json
// cannot wedge the VM: it is treated as a residual Corrupted marker, from which
// remove remains available.
func TestCorruptJSONRecordIsResidual(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t, DriverStopped)
	require.NoError(t, afero.WriteFile(env.fs, env.dir+"/"+recordFileName, []byte("{not json"), 0o600))

	st, err := env.manager.Status(context.Background())
	require.NoError(t, err)
	assert.Equal(t, Corrupted, st.State)

	_, err = env.manager.Remove(context.Background(), false)
	require.NoError(t, err)
	st, err = env.manager.Status(context.Background())
	require.NoError(t, err)
	assert.Equal(t, Uninitialized, st.State)
}

// TestInitFailureHalfwayLeavesCorrupted exercises the explicit rule: an init
// that fails after partial creation leaves a Corrupted instance whose only
// accepted mutating command is remove.
func TestInitFailureHalfwayLeavesCorrupted(t *testing.T) {
	t.Parallel()

	t.Run("driver reports stopped after failed init", func(t *testing.T) {
		t.Parallel()
		env := newTestEnv(t, DriverAbsent)
		// The tool crashes after partially creating the instance.
		env.driver.FailNextLeavingState(ActionInit, 1, DriverStopped, errFakeAction("limactl crashed mid-create"))

		_, err := env.manager.Init(context.Background())
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrDriverFailed))

		st, err := env.manager.Status(context.Background())
		require.NoError(t, err)
		assert.Equal(t, Corrupted, st.State)

		for _, fn := range []func(context.Context) (*ActionResult, error){
			env.manager.Init, env.manager.Start, func(ctx context.Context) (*ActionResult, error) {
				return env.manager.Stop(ctx, false)
			},
		} {
			_, err := fn(context.Background())
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrInvalidTransition))
		}

		_, err = env.manager.Remove(context.Background(), false)
		require.NoError(t, err, "remove must remain the cleanup path from Corrupted")
	})

	t.Run("driver reports nothing after failed init: clean rollback", func(t *testing.T) {
		t.Parallel()
		env := newTestEnv(t, DriverAbsent)
		env.driver.FailNext(ActionInit, 1, errFakeAction("disk setup failed"))

		_, err := env.manager.Init(context.Background())
		require.Error(t, err)

		st, err := env.manager.Status(context.Background())
		require.NoError(t, err)
		assert.Equal(t, Uninitialized, st.State)
		assert.Nil(t, env.loadRecord(), "record must be cleared on full rollback")

		_, err = env.manager.Init(context.Background())
		require.NoError(t, err, "retry must succeed from the rolled-back state")
	})
}

// TestStopTimeoutEscalatesToForce verifies the deterministic escalation rule:
// a graceful stop past the deadline triggers ForceStop, and the result is
// explicitly flagged as needing inspection rather than reported as a clean stop.
func TestStopTimeoutEscalatesToForce(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t, DriverRunning)
	env.seedRecord(&OperationRecord{BootCompleted: true})
	env.cfg.StopTimeout = 100 * time.Millisecond
	env.rebuildManager()
	env.driver.Delay(ActionStop, 300*time.Millisecond)

	res, err := env.manager.Stop(context.Background(), false)
	require.NoError(t, err)
	assert.True(t, res.NeedsCheck)
	assert.Contains(t, res.Message, "forcibly stopped")

	calls := env.driver.Calls()
	require.Len(t, calls, 2)
	assert.False(t, calls[0].Force, "first attempt is the graceful stop")
	assert.True(t, calls[1].Force, "second attempt escalates to force")

	st, err := env.manager.Status(context.Background())
	require.NoError(t, err)
	assert.Equal(t, Stopped, st.State)
	assert.True(t, st.NeedsCheck, "forced stop is never presented as a normal clean stop")

	// A later successful start clears the needs-check annotation.
	env.driver.ResetHistory()
	_, err = env.manager.Start(context.Background())
	require.NoError(t, err)
	st, err = env.manager.Status(context.Background())
	require.NoError(t, err)
	assert.False(t, st.NeedsCheck)
}

// TestExplicitForceStopIsMarkedNeedsCheck covers the --force flag path.
func TestExplicitForceStopIsMarkedNeedsCheck(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t, DriverRunning)
	env.seedRecord(&OperationRecord{BootCompleted: true})

	res, err := env.manager.Stop(context.Background(), true)
	require.NoError(t, err)
	assert.True(t, res.NeedsCheck)
	calls := env.driver.Calls()
	require.Len(t, calls, 1)
	assert.True(t, calls[0].Force)
}

// TestInterruptedStartLeavesRecoverableRecord exercises the "terminate together"
// signal policy through context cancellation: the driver call is aborted, the
// command exits with code 6, and the record is reconciled against real state.
func TestInterruptedStartLeavesRecoverableRecord(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t, DriverStopped)
	env.seedRecord(&OperationRecord{BootCompleted: true})
	env.driver.Block(ActionStart)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := env.manager.Start(ctx)
		done <- err
	}()
	// Wait until the start is externally observable as in progress before
	// interrupting it.
	waitUntil(t, func() bool {
		st, err := env.manager.Status(context.Background())
		return err == nil && st.State == Starting
	}, time.Second)

	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrInterrupted))
		assert.Equal(t, ExitInterrupted, ExitCode(err))
	case <-time.After(2 * time.Second):
		t.Fatal("interrupted command did not return")
	}

	// The half-booted instance is Corrupted and remove is the recovery path.
	st, err := env.manager.Status(context.Background())
	require.NoError(t, err)
	assert.Equal(t, Corrupted, st.State)

	_, err = env.manager.Remove(context.Background(), false)
	require.NoError(t, err)
}

// TestWithSignalHandlingDeliversSIGINT proves the real OS signal path cancels
// the context that the manager passes into driver calls.
func TestWithSignalHandlingDeliversSIGINT(t *testing.T) {
	t.Parallel()

	ctx, cancel := WithSignalHandling(context.Background())
	defer cancel()

	require.NoError(t, syscall.Kill(syscall.Getpid(), syscall.SIGINT))
	select {
	case <-ctx.Done():
		assert.ErrorIs(t, ctx.Err(), context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("SIGINT did not cancel the context")
	}
}

// TestDriverFailureAfterActuallySucceeded adopts the real result: when the
// driver returns an error but a fresh status query shows Running, the start is
// treated as successful rather than Corrupted.
func TestDriverFailureAfterActuallySucceeded(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t, DriverStopped)
	env.seedRecord(&OperationRecord{BootCompleted: true})
	// The start call returns an error (trailing SSH probe) after the VM itself
	// came up, so the fake leaves itself in Running while reporting failure.
	env.driver.FailNextLeavingState(ActionStart, 1, DriverRunning, errFakeAction("trailing SSH probe failed"))

	res, err := env.manager.Start(context.Background())
	require.NoError(t, err)
	assert.False(t, res.Noop)
	st, err := env.manager.Status(context.Background())
	require.NoError(t, err)
	assert.Equal(t, Running, st.State)
}

// TestTransientDriverQueryRetries verifies one flaky status query does not get
// misclassified.
func TestTransientDriverQueryRetries(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t, DriverRunning)
	env.seedRecord(&OperationRecord{BootCompleted: true})
	env.driver.FailStatus(2, ErrDriverNotReady)

	st, err := env.manager.Status(context.Background())
	require.NoError(t, err)
	assert.Equal(t, Running, st.State)
}
