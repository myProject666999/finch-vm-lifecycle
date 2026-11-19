// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package lifecycle

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testEnv bundles everything a manager test needs. The lock file lives in a real
// temp directory because flock needs a kernel-backed file; the JSON record lives
// in afero's in-memory filesystem, which is sufficient because the manager only
// accesses it through the afero interface.
type testEnv struct {
	t       *testing.T
	driver  *FakeDriver
	fs      afero.Fs
	proc    *fakeProcChecker
	dir     string
	cfg     Config
	manager *Manager
}

func newTestEnv(t *testing.T, initial DriverState) *testEnv {
	t.Helper()
	dir := t.TempDir()
	fs := afero.NewMemMapFs()
	require.NoError(t, fs.MkdirAll(dir, 0o700))
	proc := newFakeProcChecker()
	driver := NewFakeDriver(initial)
	cfg := DefaultConfig(dir, fs)
	cfg.ProcessChecker = proc
	// Keep the suite fast while leaving generous headroom for CI scheduling.
	cfg.LockTimeout = 2 * time.Second
	cfg.StatusRetry = time.Second
	cfg.StopTimeout = 200 * time.Millisecond
	return &testEnv{
		t: t, driver: driver, fs: fs, proc: proc, dir: dir, cfg: cfg,
		manager: NewManager(cfg, driver),
	}
}

// rebuildManager constructs a new manager over the same driver, state directory
// and files. Tests use it after changing cfg fields, since NewManager copies the
// config at construction time.
func (e *testEnv) rebuildManager() {
	e.manager = NewManager(e.cfg, e.driver)
}

// seedRecord writes an operation record directly, simulating what a previous
// process left behind.
func (e *testEnv) seedRecord(rec *OperationRecord) {
	e.t.Helper()
	require.NoError(e.t, e.manager.store.save(rec))
}

// loadRecord returns the persisted record, or nil when none exists.
func (e *testEnv) loadRecord() *OperationRecord {
	e.t.Helper()
	rec, err := e.manager.store.load()
	if errors.Is(err, errNoOperationRecord) {
		return nil
	}
	require.NoError(e.t, err)
	return rec
}

// TestHappyPaths walks the canonical lifecycle and checks state at every step.
func TestHappyPaths(t *testing.T) {
	t.Parallel()

	t.Run("init then stop then start then remove", func(t *testing.T) {
		t.Parallel()
		env := newTestEnv(t, DriverAbsent)
		ctx := context.Background()

		st, err := env.manager.Status(ctx)
		require.NoError(t, err)
		assert.Equal(t, Uninitialized, st.State)

		res, err := env.manager.Init(ctx)
		require.NoError(t, err)
		assert.False(t, res.Noop)

		st, err = env.manager.Status(ctx)
		require.NoError(t, err)
		assert.Equal(t, Running, st.State)

		res, err = env.manager.Stop(ctx, false)
		require.NoError(t, err)
		assert.False(t, res.Noop)
		st, err = env.manager.Status(ctx)
		require.NoError(t, err)
		assert.Equal(t, Stopped, st.State)

		res, err = env.manager.Start(ctx)
		require.NoError(t, err)
		assert.False(t, res.Noop)

		_, err = env.manager.Stop(ctx, false)
		require.NoError(t, err)

		res, err = env.manager.Remove(ctx, false)
		require.NoError(t, err)
		assert.False(t, res.Noop)
		st, err = env.manager.Status(ctx)
		require.NoError(t, err)
		assert.Equal(t, Uninitialized, st.State)
		assert.Nil(t, env.loadRecord())

		// The fake driver must have observed the exact serialized sequence.
		calls := env.driver.Calls()
		require.Len(t, calls, 5)
		assert.Equal(t, ActionInit, calls[0].Action)
		assert.Equal(t, ActionStop, calls[1].Action)
		assert.Equal(t, ActionStart, calls[2].Action)
		assert.Equal(t, ActionStop, calls[3].Action)
		assert.Equal(t, ActionRemove, calls[4].Action)
	})

	t.Run("initialized-stopped distinction needs no driver annotation", func(t *testing.T) {
		t.Parallel()
		env := newTestEnv(t, DriverStopped)
		// No record at all: driver reports stopped and no boot has ever completed.
		st, err := env.manager.Status(context.Background())
		require.NoError(t, err)
		assert.Equal(t, InitializedStopped, st.State)

		_, err = env.manager.Start(context.Background())
		require.NoError(t, err)
		_, err = env.manager.Stop(context.Background(), false)
		require.NoError(t, err)

		st, err = env.manager.Status(context.Background())
		require.NoError(t, err)
		assert.Equal(t, Stopped, st.State)
	})
}

// TestActionMatrix executes every action from every reachable state and asserts
// the expected outcome class (execute / no-op / reject). Together with
// TestTransitionTableCompleteness this covers every cell of the transition table
// at the behavior level, not just at the lookup level.
func TestActionMatrix(t *testing.T) {
	t.Parallel()

	// outcome is what the manager should do for the (state, action) cell.
	type outcome int
	const (
		execute outcome = iota
		noop
		reject
	)
	matrix := map[State]map[Action]outcome{
		Uninitialized: {
			ActionInit: execute, ActionStart: reject, ActionStop: reject, ActionRemove: reject,
		},
		InitializedStopped: {
			ActionInit: reject, ActionStart: execute, ActionStop: noop, ActionRemove: execute,
		},
		Starting: {
			ActionInit: reject, ActionStart: reject, ActionStop: reject, ActionRemove: reject,
		},
		Running: {
			ActionInit: reject, ActionStart: noop, ActionStop: execute, ActionRemove: reject,
		},
		Stopping: {
			ActionInit: reject, ActionStart: reject, ActionStop: noop, ActionRemove: reject,
		},
		Stopped: {
			ActionInit: reject, ActionStart: execute, ActionStop: noop, ActionRemove: execute,
		},
		Corrupted: {
			ActionInit: reject, ActionStart: reject, ActionStop: reject, ActionRemove: execute,
		},
	}

	actions := []struct {
		action Action
		force  bool
		run    func(*Manager, context.Context, bool) (*ActionResult, error)
	}{
		{ActionInit, false, func(m *Manager, ctx context.Context, _ bool) (*ActionResult, error) { return m.Init(ctx) }},
		{ActionStart, false, func(m *Manager, ctx context.Context, _ bool) (*ActionResult, error) { return m.Start(ctx) }},
		{ActionStop, false, func(m *Manager, ctx context.Context, f bool) (*ActionResult, error) { return m.Stop(ctx, f) }},
		{ActionRemove, false, func(m *Manager, ctx context.Context, f bool) (*ActionResult, error) { return m.Remove(ctx, f) }},
	}

	// Number of cells under test: seven states times four mutating actions. A
	// missing row or action in the matrix below would shrink the actual number of
	// subtests and fail this count, so every cell is exercised.
	covered := 0
	for state, row := range matrix {
		require.Len(t, row, 4, "row for %s must cover four actions", state)
		covered += len(row)
		for _, ac := range actions {
			expected := row[ac.action]
			name := state.String() + "/" + string(ac.action)
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				env := newEnvForState(t, state)
				callsBefore := len(env.driver.Calls())

				res, err := ac.run(env.manager, context.Background(), ac.force)

				switch expected {
				case execute:
					require.NoError(t, err, "%s should execute", name)
					require.NotNil(t, res)
					assert.False(t, res.Noop)
					assert.Greater(t, len(env.driver.Calls()), callsBefore, "driver must have been called")
				case noop:
					require.NoError(t, err, "%s should be a successful no-op", name)
					require.NotNil(t, res)
					assert.True(t, res.Noop, "no driver call expected")
					assert.Len(t, env.driver.Calls(), callsBefore, "no driver call expected")
				case reject:
					require.Error(t, err, "%s must be rejected", name)
					assert.Nil(t, res)
					assert.Len(t, env.driver.Calls(), callsBefore, "rejected action must not touch the driver")
				}
			})
		}
	}
	assert.Equal(t, 7*4, covered)
}

// newEnvForState constructs an environment whose derived status is the requested
// state, seeding records where the driver alone cannot express the state.
func newEnvForState(t *testing.T, state State) *testEnv {
	t.Helper()
	switch state {
	case Uninitialized:
		return newTestEnv(t, DriverAbsent)
	case InitializedStopped:
		return newTestEnv(t, DriverStopped)
	case Starting:
		env := newTestEnv(t, DriverRunning)
		env.seedRecord(&OperationRecord{Action: ActionStart, PID: 1_000_000, StartedAt: time.Now().UTC()})
		return env
	case Running:
		env := newTestEnv(t, DriverRunning)
		env.seedRecord(&OperationRecord{BootCompleted: true})
		return env
	case Stopping:
		env := newTestEnv(t, DriverRunning)
		env.seedRecord(&OperationRecord{
			Action: ActionStop, PID: 1_000_001, BootCompleted: true,
			StartedAt: time.Now().UTC(),
		})
		return env
	case Stopped:
		env := newTestEnv(t, DriverStopped)
		env.seedRecord(&OperationRecord{BootCompleted: true})
		return env
	case Corrupted:
		env := newTestEnv(t, DriverStopped)
		env.seedRecord(&OperationRecord{Corrupted: true, BootCompleted: true})
		return env
	default:
		t.Fatalf("unsupported state %s", state)
		return nil
	}
}

func TestRejectErrorsCarryGuidance(t *testing.T) {
	t.Parallel()

	t.Run("start on uninitialized tells user to init", func(t *testing.T) {
		t.Parallel()
		env := newTestEnv(t, DriverAbsent)
		_, err := env.manager.Start(context.Background())
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrInvalidTransition))
		assert.Contains(t, err.Error(), "init")
		assert.Equal(t, ExitInvalidTransition, ExitCode(err))
	})

	t.Run("remove on running tells user to stop", func(t *testing.T) {
		t.Parallel()
		env := newTestEnv(t, DriverRunning)
		env.seedRecord(&OperationRecord{BootCompleted: true})
		_, err := env.manager.Remove(context.Background(), false)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrInvalidTransition))
		assert.Contains(t, err.Error(), "stop")
	})

	t.Run("start while stopping is a concurrent rejection", func(t *testing.T) {
		t.Parallel()
		env := newEnvForState(t, Stopping)
		_, err := env.manager.Start(context.Background())
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrConcurrentOperation))
		assert.Equal(t, ExitConcurrent, ExitCode(err))
	})

	t.Run("corrupted only accepts remove", func(t *testing.T) {
		t.Parallel()
		env := newEnvForState(t, Corrupted)
		for _, act := range []struct {
			name string
			run  func() (*ActionResult, error)
		}{
			{"init", func() (*ActionResult, error) { return env.manager.Init(context.Background()) }},
			{"start", func() (*ActionResult, error) { return env.manager.Start(context.Background()) }},
			{"stop", func() (*ActionResult, error) { return env.manager.Stop(context.Background(), false) }},
		} {
			_, err := act.run()
			require.Error(t, err, act.name)
			assert.True(t, errors.Is(err, ErrInvalidTransition), act.name)
		}
		res, err := env.manager.Remove(context.Background(), false)
		require.NoError(t, err)
		assert.False(t, res.Noop)
	})
}

func TestNoopSuccessIsDistinguishable(t *testing.T) {
	t.Parallel()

	// start on Running: exit code 0, no driver call, explicit no-op marker.
	env := newTestEnv(t, DriverRunning)
	env.seedRecord(&OperationRecord{BootCompleted: true})
	res, err := env.manager.Start(context.Background())
	require.NoError(t, err)
	assert.True(t, res.Noop)
	assert.Equal(t, ExitOK, ExitCode(nil))
	assert.Contains(t, res.Message, "already")
	assert.Empty(t, env.driver.Calls())

	// stop on Stopped: symmetric idempotent success.
	env2 := newTestEnv(t, DriverStopped)
	env2.seedRecord(&OperationRecord{BootCompleted: true})
	res, err = env2.manager.Stop(context.Background(), false)
	require.NoError(t, err)
	assert.True(t, res.Noop)
	assert.Empty(t, env2.driver.Calls())
}
