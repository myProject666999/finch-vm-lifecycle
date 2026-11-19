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

func TestNewInitVMCommand(t *testing.T) {
	t.Parallel()

	cmd := newInitVMCommand(nil, nil, nil, nil, nil, "", nil, "", nil, nil, nil)
	assert.Equal(t, cmd.Name(), "init")
}

func TestInitVMAction_runAdapter(t *testing.T) {
	t.Parallel()

	t.Run("initializes and starts an absent instance", func(t *testing.T) {
		t.Parallel()
		executor, driver, _ := newTestExecutor(t, lifecycle.DriverAbsent)

		err := newInitVMAction(flog.NewLogrus(), executor).runAdapter(&cobra.Command{Use: "init"}, nil)
		require.NoError(t, err)
		assert.Equal(t, lifecycle.DriverRunning, driver.State())
		calls := driver.Calls()
		require.Len(t, calls, 1)
		assert.Equal(t, lifecycle.ActionInit, calls[0].Action)

		st, err := executor.Status(t.Context())
		require.NoError(t, err)
		assert.Equal(t, lifecycle.Running, st.State)
	})
}

func TestInitVMAction_run(t *testing.T) {
	t.Parallel()

	t.Run("init on an existing stopped instance is rejected with start guidance", func(t *testing.T) {
		t.Parallel()
		executor, driver, _ := newTestExecutor(t, lifecycle.DriverStopped)

		err := newInitVMAction(flog.NewLogrus(), executor).run()
		require.Error(t, err)
		assert.ErrorIs(t, err, lifecycle.ErrInvalidTransition)
		assert.Contains(t, err.Error(), "start")
		assert.Empty(t, driver.Calls())
	})

	t.Run("init on a running instance is rejected", func(t *testing.T) {
		t.Parallel()
		executor, driver, _ := newTestExecutor(t, lifecycle.DriverAbsent)
		executorTestSeedRunning(t, executor)

		err := newInitVMAction(flog.NewLogrus(), executor).run()
		require.Error(t, err)
		assert.ErrorIs(t, err, lifecycle.ErrInvalidTransition)
		// The seeding init is the only driver call; the rejected init adds none.
		assert.Len(t, driver.Calls(), 1)
	})

	t.Run("init on a corrupted instance is rejected in favor of remove", func(t *testing.T) {
		t.Parallel()
		executor, driver, _ := newTestExecutor(t, lifecycle.DriverStopped)
		driver.FailNextLeavingState(lifecycle.ActionInit, 1, lifecycle.DriverStopped, errors.New("half create"))

		firstErr := newInitVMAction(flog.NewLogrus(), executor).run()
		require.Error(t, firstErr)

		err := newInitVMAction(flog.NewLogrus(), executor).run()
		require.Error(t, err)
		assert.ErrorIs(t, err, lifecycle.ErrInvalidTransition)
		assert.Contains(t, err.Error(), "remove")
	})

	t.Run("failed init with no instance left behind rolls back and can be retried", func(t *testing.T) {
		t.Parallel()
		executor, driver, _ := newTestExecutor(t, lifecycle.DriverAbsent)
		driver.FailNext(lifecycle.ActionInit, 1, errors.New("disk setup failed"))

		err := newInitVMAction(flog.NewLogrus(), executor).run()
		require.Error(t, err)

		st, err := executor.Status(t.Context())
		require.NoError(t, err)
		assert.Equal(t, lifecycle.Uninitialized, st.State)

		require.NoError(t, newInitVMAction(flog.NewLogrus(), executor).run())
		assert.Equal(t, lifecycle.DriverRunning, driver.State())
	})
}
