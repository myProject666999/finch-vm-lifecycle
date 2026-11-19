// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || windows

package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/runfinch/finch/pkg/lifecycle"
	"github.com/spf13/afero"
	"github.com/stretchr/testify/require"
)

// testExecutorEnv keeps the pieces command-level tests need to arrange state.
type testExecutorEnv struct {
	manager *lifecycle.Manager
	driver  *lifecycle.FakeDriver
	fs      afero.Fs
	dir     string
}

// newTestExecutor builds a lifecycle manager over the in-memory fake driver in
// a throwaway state directory. It is the command-level equivalent of the fake
// driver harness used inside the lifecycle package tests: no real limactl, no
// real VM, kernel-backed lock file in t.TempDir.
func newTestExecutor(t *testing.T, initial lifecycle.DriverState) (*lifecycle.Manager, *lifecycle.FakeDriver, string) {
	t.Helper()
	env := newTestExecutorEnv(t, initial)
	return env.manager, env.driver, env.dir
}

func newTestExecutorEnv(t *testing.T, initial lifecycle.DriverState) *testExecutorEnv {
	t.Helper()
	dir := t.TempDir()
	fs := afero.NewMemMapFs()
	require.NoError(t, fs.MkdirAll(dir, 0o700))
	driver := lifecycle.NewFakeDriver(initial)
	cfg := lifecycle.DefaultConfig(dir, fs)
	return &testExecutorEnv{manager: lifecycle.NewManager(cfg, driver), driver: driver, fs: fs, dir: dir}
}

// seedStaleRecord writes an operation.json owned by a pid that cannot exist in
// the test process, simulating the residue of a crashed finch invocation.
func (e *testExecutorEnv) seedStaleRecord(t *testing.T, action lifecycle.Action) {
	t.Helper()
	rec := map[string]any{
		"version":        1,
		"action":         string(action),
		"pid":            2_000_000, // far above any live pid in the test sandbox
		"started_at":     time.Now().UTC().Add(-time.Minute).Format(time.RFC3339),
		"updated_at":     time.Now().UTC().Format(time.RFC3339),
		"boot_completed": true,
	}
	raw, err := json.Marshal(rec)
	require.NoError(t, err)
	require.NoError(t, afero.WriteFile(e.fs, filepath.Join(e.dir, "operation.json"), raw, 0o600))
}

// waitForTestLock gives a companion goroutine time to pass the state check, take
// the lock and block inside the fake driver's gate. Instead of a fixed sleep it
// polls the lock-free status until the in-progress operation becomes visible,
// which can only happen after the holder wrote its operation record.
func waitForTestLock(t *testing.T, executor lifecycleExecutor) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st, err := executor.Status(context.Background())
		if err == nil && st.State.IsTransitional() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the companion goroutine to acquire the lock")
}
