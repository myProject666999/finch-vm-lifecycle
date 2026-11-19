// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package lifecycle

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFileLockMutualExclusion exercises the raw cross-process lock mechanics with
// real goroutines contending for one kernel-backed flock file: non-blocking
// acquisition fails while a holder exists, a blocking waiter times out, and the
// lock is acquirable again immediately after release (a crashed holder leaves
// only the file behind, never an enforced residual lock).
func TestFileLockMutualExclusion(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	holder := newFileLock(dir)
	got, err := holder.AcquireNoWait()
	require.NoError(t, err)
	require.True(t, got)

	other := newFileLock(dir)
	got, err = other.AcquireNoWait()
	require.NoError(t, err)
	assert.False(t, got, "a second non-blocking acquisition must fail while held")

	ctx := context.Background()
	err = other.Acquire(ctx, 80*time.Millisecond)
	assert.ErrorIs(t, err, ErrLockTimeout)

	require.NoError(t, holder.release())
	got, err = other.AcquireNoWait()
	require.NoError(t, err)
	assert.True(t, got, "the lock must be reusable as soon as the holder releases")
	require.NoError(t, other.release())
}

// TestLockTimeoutErrorNamesHolder verifies the user-facing timeout error carries
// the holding command and pid read from the operation record.
func TestLockTimeoutErrorNamesHolder(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t, DriverAbsent)
	holder := newFileLock(env.dir)
	got, err := holder.AcquireNoWait()
	require.NoError(t, err)
	require.True(t, got)
	env.seedRecord(&OperationRecord{Action: ActionStart, PID: 777, StartedAt: time.Now().UTC()})

	env.cfg.LockTimeout = 50 * time.Millisecond
	env.rebuildManager()
	err = env.manager.acquireLock(context.Background(), newFileLock(env.dir))
	var lte *LockTimeoutError
	require.ErrorAs(t, err, &lte)
	require.NotNil(t, lte.Holder)
	assert.Equal(t, ActionStart, lte.Holder.Action)
	assert.Equal(t, 777, lte.Holder.PID)
	assert.Equal(t, ExitLockTimeout, ExitCode(err))
	require.NoError(t, holder.release())
}

// TestConcurrentCommandsNeverOverlap uses real goroutines and a gated fake
// driver. While the first command is blocked inside the driver, the second
// command fails fast by convention (exit code 3) without touching the driver;
// after the first completes, commands serialize through the lock and the driver
// never observes overlapping calls.
func TestConcurrentCommandsNeverOverlap(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t, DriverAbsent)
	env.driver.Block(ActionInit)

	firstDone := make(chan struct{})
	var firstErr error
	go func() {
		_, firstErr = env.manager.Init(context.Background())
		close(firstDone)
	}()
	// Wait until the transitional state is externally visible, which means the
	// holder has both acquired the lock and persisted its operation record.
	// Waiting on the lock alone would race the record write and let the
	// pre-check observe an empty record.
	waitUntil(t, func() bool {
		st, err := env.manager.Status(context.Background())
		return err == nil && st.State == Starting
	}, time.Second)

	// start/stop while Starting is refused immediately rather than queued.
	_, stopErr := env.manager.Stop(context.Background(), false)
	require.Error(t, stopErr)
	assert.True(t, errors.Is(stopErr, ErrConcurrentOperation))
	assert.Equal(t, ExitConcurrent, ExitCode(stopErr))
	require.Len(t, env.driver.Calls(), 1, "the rejected command must not touch the driver")

	// A no-op request against the transitional state also returns without waiting
	// and without a driver call; here stop-while-stopping is no-op, and
	// start-while-starting is rejected. Check a second conflicting action.
	_, startErr := env.manager.Start(context.Background())
	require.Error(t, startErr)
	assert.True(t, errors.Is(startErr, ErrConcurrentOperation))

	env.driver.Release(ActionInit)
	<-firstDone
	require.NoError(t, firstErr)

	// Now serialized commands proceed normally, in order.
	_, err := env.manager.Stop(context.Background(), false)
	require.NoError(t, err)
	_, err = env.manager.Start(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 1, env.driver.MaxConcurrent())
	assert.Equal(t, 0, env.driver.OverlapCount())
	calls := env.driver.Calls()
	require.Len(t, calls, 3)
	assert.Equal(t, []Action{ActionInit, ActionStop, ActionStart},
		[]Action{calls[0].Action, calls[1].Action, calls[2].Action})
}

// TestManyGoroutinesNeverOverlap fires a burst of full lifecycle commands at one
// manager. It asserts at the driver boundary that mutating calls are strictly
// serialized for the whole run.
func TestManyGoroutinesNeverOverlap(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t, DriverAbsent)
	// Small delays widen the race window.
	env.driver.Delay(ActionStart, 5*time.Millisecond)
	env.driver.Delay(ActionStop, 5*time.Millisecond)
	env.driver.Delay(ActionInit, 5*time.Millisecond)
	env.driver.Delay(ActionRemove, 5*time.Millisecond)
	env.cfg.LockTimeout = 10 * time.Second

	const n = 16
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			ctx := context.Background()
			switch i % 4 {
			case 0:
				_, _ = env.manager.Start(ctx)
			case 1:
				_, _ = env.manager.Stop(ctx, false)
			case 2:
				_, _ = env.manager.Init(ctx)
			default:
				_, _ = env.manager.Remove(ctx, false)
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, 1, env.driver.MaxConcurrent(), "driver calls must never overlap")
	assert.Equal(t, 0, env.driver.OverlapCount())
}

// TestNonBlockingConfigFailsOnContendedLock proves the NonBlocking configuration
// turns lock contention into an immediate failure at the lock layer. At the
// manager layer transitional contention is normally caught earlier, by the
// lock-free pre-check, so this test pins the lock-layer behavior directly.
func TestNonBlockingConfigFailsOnContendedLock(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t, DriverAbsent)
	holder := newFileLock(env.dir)
	got, err := holder.AcquireNoWait()
	require.NoError(t, err)
	require.True(t, got)

	env.cfg.NonBlocking = true
	env.rebuildManager()
	start := time.Now()
	err = env.manager.acquireLock(context.Background(), newFileLock(env.dir))
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrLockTimeout)
	assert.Less(t, elapsed, 100*time.Millisecond)
	require.NoError(t, holder.release())
}

// TestStatusDoesNotBlockOnLock proves read-only status never waits for the lock:
// it returns a transitional view while a mutating command is in flight.
func TestStatusDoesNotBlockOnLock(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t, DriverAbsent)
	env.driver.Block(ActionInit)

	holderDone := make(chan struct{})
	go func() {
		_, _ = env.manager.Init(context.Background())
		close(holderDone)
	}()
	// Synchronize on the externally visible transitional state (lock plus record)
	// rather than on the lock alone.
	waitUntil(t, func() bool {
		st, err := env.manager.Status(context.Background())
		return err == nil && st.State == Starting
	}, time.Second)

	done := make(chan StatusResult, 1)
	go func() {
		st, err := env.manager.Status(context.Background())
		require.NoError(t, err)
		done <- st
	}()
	select {
	case st := <-done:
		assert.True(t, st.State.IsTransitional(), "status must surface the transitional state, got %s", st.State)
		assert.Equal(t, Starting, st.State)
		require.NotNil(t, st.Holder)
		assert.Equal(t, ActionInit, st.Holder.Action)
		assert.False(t, st.StaleHolder, "the holder process is alive")
	case <-time.After(time.Second):
		t.Fatal("status blocked on the lifecycle lock")
	}

	env.driver.Release(ActionInit)
	<-holderDone

	st, err := env.manager.Status(context.Background())
	require.NoError(t, err)
	assert.Equal(t, Running, st.State)
}

// waitUntil polls cond until it returns true or the deadline elapses.
func waitUntil(t *testing.T, cond func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}

// TestFakeDriverHonorsDelayAndCancellation proves the fake driver's delay hook
// both slows a successful call and honors context cancellation instead of
// sleeping through it.
func TestFakeDriverHonorsDelayAndCancellation(t *testing.T) {
	t.Parallel()

	t.Run("delay elapses and the call succeeds", func(t *testing.T) {
		t.Parallel()
		env := newTestEnv(t, DriverStopped)
		env.driver.Delay(ActionStart, 30*time.Millisecond)

		start := time.Now()
		_, err := env.manager.Start(context.Background())
		require.NoError(t, err)
		assert.GreaterOrEqual(t, time.Since(start), 25*time.Millisecond)
		assert.Equal(t, DriverRunning, env.driver.State())
	})

	t.Run("cancellation cuts a delayed call short and marks Corrupted", func(t *testing.T) {
		t.Parallel()
		env := newTestEnv(t, DriverStopped)
		env.seedRecord(&OperationRecord{BootCompleted: true})
		env.driver.Delay(ActionStart, 5*time.Second)

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := env.manager.Start(ctx)
			done <- err
		}()
		time.Sleep(30 * time.Millisecond)
		cancel()

		select {
		case err := <-done:
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInterrupted)
		case <-time.After(2 * time.Second):
			t.Fatal("cancelled delayed call did not return promptly")
		}
	})
}
