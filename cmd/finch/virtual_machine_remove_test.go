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

func TestNewRemoveVMCommand(t *testing.T) {
	t.Parallel()

	cmd := newRemoveVMCommand(nil, flog.NewLogrus(), nil)
	assert.Equal(t, cmd.Name(), "remove")
}

func TestRemoveVMAction_runAdapter(t *testing.T) {
	t.Parallel()

	t.Run("removes a stopped instance through the lifecycle manager", func(t *testing.T) {
		t.Parallel()
		ctrl := gomock.NewController(t)
		dm := mocks.NewUserDataDiskManager(ctrl)
		dm.EXPECT().DetachUserDataDisk().Return(nil)

		executor, driver, _ := newTestExecutor(t, lifecycle.DriverAbsent)
		executorTestSeedRunning(t, executor)
		_, err := executor.Stop(t.Context(), false)
		require.NoError(t, err)

		cmd := &cobra.Command{Use: "remove"}
		cmd.Flags().BoolP("force", "f", false, "")
		err = newRemoveVMAction(dm, flog.NewLogrus(), executor).runAdapter(cmd, nil)
		require.NoError(t, err)
		assert.Equal(t, lifecycle.DriverAbsent, driver.State())

		st, err := executor.Status(t.Context())
		require.NoError(t, err)
		assert.Equal(t, lifecycle.Uninitialized, st.State)
	})

	t.Run("--force on a running instance force-stops then removes", func(t *testing.T) {
		t.Parallel()
		ctrl := gomock.NewController(t)
		dm := mocks.NewUserDataDiskManager(ctrl)
		dm.EXPECT().DetachUserDataDisk().Return(nil)

		executor, driver, _ := newTestExecutor(t, lifecycle.DriverAbsent)
		executorTestSeedRunning(t, executor)

		cmd := &cobra.Command{Use: "remove"}
		cmd.Flags().BoolP("force", "f", false, "")
		require.NoError(t, cmd.Flags().Set("force", "true"))
		err := newRemoveVMAction(dm, flog.NewLogrus(), executor).runAdapter(cmd, nil)
		require.NoError(t, err)
		assert.Equal(t, lifecycle.DriverAbsent, driver.State())

		// The remove path contains a forced stop before the removal.
		var sawForceStop, sawRemove bool
		for _, c := range driver.Calls() {
			if c.Action == lifecycle.ActionStop && c.Force {
				sawForceStop = true
			}
			if c.Action == lifecycle.ActionRemove {
				sawRemove = true
			}
		}
		assert.True(t, sawForceStop)
		assert.True(t, sawRemove)
	})
}

func TestRemoveVMAction_run(t *testing.T) {
	t.Parallel()

	t.Run("remove on a running instance without --force is rejected with stop guidance", func(t *testing.T) {
		t.Parallel()
		ctrl := gomock.NewController(t)
		dm := mocks.NewUserDataDiskManager(ctrl)
		dm.EXPECT().DetachUserDataDisk().Return(nil)

		executor, driver, _ := newTestExecutor(t, lifecycle.DriverAbsent)
		executorTestSeedRunning(t, executor)

		err := newRemoveVMAction(dm, flog.NewLogrus(), executor).run(false)
		require.Error(t, err)
		assert.ErrorIs(t, err, lifecycle.ErrInvalidTransition)
		assert.Contains(t, err.Error(), "stop")
		assert.NotContains(t, driver.Calls(), lifecycle.ActionRemove)
	})

	t.Run("remove on an absent instance is rejected", func(t *testing.T) {
		t.Parallel()
		ctrl := gomock.NewController(t)
		dm := mocks.NewUserDataDiskManager(ctrl)
		dm.EXPECT().DetachUserDataDisk().Return(nil)

		executor, driver, _ := newTestExecutor(t, lifecycle.DriverAbsent)
		err := newRemoveVMAction(dm, flog.NewLogrus(), executor).run(false)
		require.Error(t, err)
		assert.ErrorIs(t, err, lifecycle.ErrInvalidTransition)
		assert.Empty(t, driver.Calls())
	})

	t.Run("remove is the only mutating command accepted on a corrupted instance", func(t *testing.T) {
		t.Parallel()
		ctrl := gomock.NewController(t)
		dm := mocks.NewUserDataDiskManager(ctrl)
		dm.EXPECT().DetachUserDataDisk().Return(nil).Times(2)

		executor, driver, _ := newTestExecutor(t, lifecycle.DriverStopped)
		driver.FailNextLeavingState(lifecycle.ActionInit, 1, lifecycle.DriverStopped, errors.New("half create"))
		_ = newInitVMAction(flog.NewLogrus(), executor).run() // leaves Corrupted

		st, err := executor.Status(t.Context())
		require.NoError(t, err)
		assert.Equal(t, lifecycle.Corrupted, st.State)

		// Remove is accepted and performs the cleanup.
		require.NoError(t, newRemoveVMAction(dm, flog.NewLogrus(), executor).run(false))
		assert.Equal(t, lifecycle.DriverAbsent, driver.State())
	})
}
