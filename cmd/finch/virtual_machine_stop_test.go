// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || windows

package main

import (
	"errors"
	"testing"

	"github.com/runfinch/finch/pkg/flog"
	"github.com/runfinch/finch/pkg/lifecycle"
	"github.com/runfinch/finch/pkg/mocks"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func TestNewStopVMCommand(t *testing.T) {
	t.Parallel()

	cmd := newStopVMCommand(nil, flog.NewLogrus(), nil)
	assert.Equal(t, cmd.Name(), "stop")
}

func TestStopVMAction_runAdapter(t *testing.T) {
	t.Parallel()

	t.Run("stops a running instance through the lifecycle manager", func(t *testing.T) {
		t.Parallel()
		ctrl := gomock.NewController(t)
		dm := mocks.NewUserDataDiskManager(ctrl)
		dm.EXPECT().DetachUserDataDisk().Return(nil)

		executor, driver, _ := newTestExecutor(t, lifecycle.DriverAbsent)
		executorTestSeedRunning(t, executor)

		cmd := &cobra.Command{Use: "stop"}
		cmd.Flags().BoolP("force", "f", false, "")
		err := newStopVMAction(dm, flog.NewLogrus(), executor).runAdapter(cmd, nil)
		require.NoError(t, err)
		assert.Equal(t, lifecycle.DriverStopped, driver.State())
	})

	t.Run("--force maps to the driver's ForceStop and needs-check is surfaced", func(t *testing.T) {
		t.Parallel()
		ctrl := gomock.NewController(t)
		dm := mocks.NewUserDataDiskManager(ctrl)
		dm.EXPECT().DetachUserDataDisk().Return(nil)

		executor, driver, _ := newTestExecutor(t, lifecycle.DriverAbsent)
		executorTestSeedRunning(t, executor)

		cmd := &cobra.Command{Use: "stop"}
		cmd.Flags().BoolP("force", "f", false, "")
		require.NoError(t, cmd.Flags().Set("force", "true"))
		err := newStopVMAction(dm, flog.NewLogrus(), executor).runAdapter(cmd, nil)
		require.NoError(t, err)

		calls := driver.Calls()
		require.Len(t, calls, 1)
		assert.True(t, calls[0].Force)
		st, err := executor.Status(t.Context())
		require.NoError(t, err)
		assert.True(t, st.NeedsCheck)
	})
}

func TestStopVMAction_run(t *testing.T) {
	t.Parallel()

	t.Run("graceful stop succeeds", func(t *testing.T) {
		t.Parallel()
		ctrl := gomock.NewController(t)
		dm := mocks.NewUserDataDiskManager(ctrl)
		dm.EXPECT().DetachUserDataDisk().Return(nil)

		executor, driver, _ := newTestExecutor(t, lifecycle.DriverAbsent)
		executorTestSeedRunning(t, executor)

		err := newStopVMAction(dm, flog.NewLogrus(), executor).run(false)
		require.NoError(t, err)
		assert.Equal(t, lifecycle.DriverStopped, driver.State())
	})

	t.Run("stopped VM is a successful no-op with no driver call", func(t *testing.T) {
		t.Parallel()
		ctrl := gomock.NewController(t)
		dm := mocks.NewUserDataDiskManager(ctrl)
		dm.EXPECT().DetachUserDataDisk().Return(nil)

		executor, driver, _ := newTestExecutor(t, lifecycle.DriverStopped)
		err := newStopVMAction(dm, flog.NewLogrus(), executor).run(false)
		require.NoError(t, err, "idempotent stop exits 0")
		assert.Empty(t, driver.Calls())
	})

	t.Run("nonexistent VM is rejected with the documented exit code", func(t *testing.T) {
		t.Parallel()
		ctrl := gomock.NewController(t)
		dm := mocks.NewUserDataDiskManager(ctrl)
		dm.EXPECT().DetachUserDataDisk().Return(nil)

		executor, _, _ := newTestExecutor(t, lifecycle.DriverAbsent)
		err := newStopVMAction(dm, flog.NewLogrus(), executor).run(false)
		require.Error(t, err)
		var coder interface{ ExitCode() int }
		require.ErrorAs(t, err, &coder)
		assert.Equal(t, lifecycle.ExitInvalidTransition, coder.ExitCode())
	})

	t.Run("driver failure is reported with the driver-failure exit code", func(t *testing.T) {
		t.Parallel()
		ctrl := gomock.NewController(t)
		dm := mocks.NewUserDataDiskManager(ctrl)
		dm.EXPECT().DetachUserDataDisk().Return(nil)

		executor, driver, _ := newTestExecutor(t, lifecycle.DriverAbsent)
		executorTestSeedRunning(t, executor)
		driver.FailNext(lifecycle.ActionStop, 1, errors.New("limactl exploded"))

		err := newStopVMAction(dm, flog.NewLogrus(), executor).run(false)
		require.Error(t, err)
		assert.ErrorIs(t, err, lifecycle.ErrDriverFailed)
	})
}

// executorTestSeedRunning drives a fresh executor through init so its operation
// record contains a completed boot and its fake driver reaches Running.
func executorTestSeedRunning(t *testing.T, executor lifecycleExecutor) {
	t.Helper()
	_, err := executor.Init(t.Context())
	require.NoError(t, err)
}
