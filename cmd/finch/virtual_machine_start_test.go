// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || windows

package main

import (
	"errors"
	"testing"

	"github.com/runfinch/finch/pkg/flog"
	"github.com/runfinch/finch/pkg/lifecycle"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewStartVMCommand(t *testing.T) {
	t.Parallel()

	cmd := newStartVMCommand(nil, nil, nil, nil, nil, nil, "", nil, nil)
	assert.Equal(t, cmd.Name(), "start")
}

func TestStartVMAction_runAdapter(t *testing.T) {
	t.Parallel()

	t.Run("starts an initialized, stopped instance", func(t *testing.T) {
		t.Parallel()
		executor, driver, _ := newTestExecutor(t, lifecycle.DriverStopped)

		err := newStartVMAction(flog.NewLogrus(), executor).runAdapter(&cobra.Command{Use: "start"}, nil)
		require.NoError(t, err)
		assert.Equal(t, lifecycle.DriverRunning, driver.State())
		calls := driver.Calls()
		require.Len(t, calls, 1)
		assert.Equal(t, lifecycle.ActionStart, calls[0].Action)
	})
}

func TestStartVMAction_run(t *testing.T) {
	t.Parallel()

	t.Run("start on an uninitialized VM is rejected and tells the caller to init", func(t *testing.T) {
		t.Parallel()
		executor, driver, _ := newTestExecutor(t, lifecycle.DriverAbsent)

		err := newStartVMAction(flog.NewLogrus(), executor).run()
		require.Error(t, err)
		assert.ErrorIs(t, err, lifecycle.ErrInvalidTransition)
		assert.Contains(t, err.Error(), "init")
		var coder interface{ ExitCode() int }
		require.ErrorAs(t, err, &coder)
		assert.Equal(t, lifecycle.ExitInvalidTransition, coder.ExitCode())
		assert.Empty(t, driver.Calls(), "a rejected start must not reach the driver")
	})

	t.Run("start on a running VM is a successful no-op, exit 0, no driver call", func(t *testing.T) {
		t.Parallel()
		executor, driver, _ := newTestExecutor(t, lifecycle.DriverAbsent)
		executorTestSeedRunning(t, executor)
		callsBefore := len(driver.Calls())

		res, err := executor.Start(t.Context())
		require.NoError(t, err)
		assert.True(t, res.Noop)
		// And through the cobra action it remains a clean success.
		require.NoError(t, newStartVMAction(flog.NewLogrus(), executor).run())
		assert.Len(t, driver.Calls(), callsBefore)
	})

	t.Run("start while stopping is refused with the concurrent-operation exit code", func(t *testing.T) {
		t.Parallel()
		executor, driver, _ := newTestExecutor(t, lifecycle.DriverRunning)
		driver.Block(lifecycle.ActionStop)

		stopErr := make(chan error, 1)
		go func() {
			_, err := executor.Stop(t.Context(), false)
			stopErr <- err
		}()
		waitForTestLock(t, executor)

		err := newStartVMAction(flog.NewLogrus(), executor).run()
		require.Error(t, err)
		assert.ErrorIs(t, err, lifecycle.ErrConcurrentOperation)
		var coder interface{ ExitCode() int }
		require.ErrorAs(t, err, &coder)
		assert.Equal(t, lifecycle.ExitConcurrent, coder.ExitCode())

		driver.Release(lifecycle.ActionStop)
		require.NoError(t, <-stopErr)
	})

	t.Run("driver failure surfaces as a driver error and leaves a recoverable record", func(t *testing.T) {
		t.Parallel()
		executor, driver, _ := newTestExecutor(t, lifecycle.DriverStopped)
		driver.FailNext(lifecycle.ActionStart, 1, errors.New("boot failed"))

		err := newStartVMAction(flog.NewLogrus(), executor).run()
		require.Error(t, err)
		assert.ErrorIs(t, err, lifecycle.ErrDriverFailed)
	})
}
