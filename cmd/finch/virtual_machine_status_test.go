// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || windows

package main

import (
	"bytes"
	"testing"

	"github.com/runfinch/finch/pkg/flog"
	"github.com/runfinch/finch/pkg/lifecycle"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewStatusVMCommand(t *testing.T) {
	t.Parallel()

	cmd := newStatusVMCommand(flog.NewLogrus(), &bytes.Buffer{}, nil)
	assert.Equal(t, cmd.Name(), "status")
}

func TestStatusVMAction_run(t *testing.T) {
	t.Parallel()

	t.Run("reports Nonexistent for an absent instance", func(t *testing.T) {
		t.Parallel()
		executor, _, _ := newTestExecutor(t, lifecycle.DriverAbsent)
		var out bytes.Buffer
		require.NoError(t, newStatusVMAction(flog.NewLogrus(), &out, executor).run())
		assert.Contains(t, out.String(), "Nonexistent")
	})

	t.Run("reports Running for a booted instance", func(t *testing.T) {
		t.Parallel()
		executor, _, _ := newTestExecutor(t, lifecycle.DriverAbsent)
		executorTestSeedRunning(t, executor)
		var out bytes.Buffer
		require.NoError(t, newStatusVMAction(flog.NewLogrus(), &out, executor).run())
		assert.Contains(t, out.String(), "Running")
	})

	t.Run("reports Stopped for a halted, previously booted instance", func(t *testing.T) {
		t.Parallel()
		executor, _, _ := newTestExecutor(t, lifecycle.DriverStopped)
		var out bytes.Buffer
		require.NoError(t, newStatusVMAction(flog.NewLogrus(), &out, executor).run())
		assert.Contains(t, out.String(), "Stopped")
	})

	t.Run("explicitly marks a transitional in-progress state", func(t *testing.T) {
		t.Parallel()
		executor, driver, _ := newTestExecutor(t, lifecycle.DriverAbsent)
		driver.Block(lifecycle.ActionInit)

		done := make(chan struct{})
		go func() {
			_, _ = executor.Init(t.Context())
			close(done)
		}()
		waitForTestLock(t, executor)

		var out bytes.Buffer
		require.NoError(t, newStatusVMAction(flog.NewLogrus(), &out, executor).run())
		// The transitional marker must be visible instead of a stable word.
		assert.Contains(t, out.String(), "in progress")

		driver.Release(lifecycle.ActionInit)
		<-done
	})

	t.Run("surfaces a stale holder as a recoverable warning", func(t *testing.T) {
		t.Parallel()
		env := newTestExecutorEnv(t, lifecycle.DriverRunning)
		env.seedStaleRecord(t, lifecycle.ActionStop)

		var out bytes.Buffer
		require.NoError(t, newStatusVMAction(flog.NewLogrus(), &out, env.manager).run())
		assert.Contains(t, out.String(), "no longer exists")
	})

	t.Run("prints the needs-inspection warning after a forced stop", func(t *testing.T) {
		t.Parallel()
		executor, _, _ := newTestExecutor(t, lifecycle.DriverAbsent)
		executorTestSeedRunning(t, executor)
		_, err := executor.Stop(t.Context(), true)
		require.NoError(t, err)

		var out bytes.Buffer
		require.NoError(t, newStatusVMAction(flog.NewLogrus(), &out, executor).run())
		assert.Contains(t, out.String(), "needs inspection")
	})
}
