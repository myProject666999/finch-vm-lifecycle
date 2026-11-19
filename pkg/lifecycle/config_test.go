// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package lifecycle

import (
	"testing"
	"time"

	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigWithDefaults(t *testing.T) {
	t.Parallel()

	cfg := Config{StateDir: t.TempDir(), Fs: afero.NewMemMapFs()}
	cfg.withDefaults()

	assert.Equal(t, DefaultLockTimeout, cfg.LockTimeout)
	assert.Equal(t, DefaultStopTimeout, cfg.StopTimeout)
	assert.Equal(t, DefaultStatusRetry, cfg.StatusRetry)
	assert.NotNil(t, cfg.Clock)
	assert.NotNil(t, cfg.ProcessChecker)
}

func TestConfigWithDefaultsPreservesOverrides(t *testing.T) {
	t.Parallel()

	cfg := Config{
		StateDir:    t.TempDir(),
		Fs:          afero.NewMemMapFs(),
		LockTimeout: 7 * time.Second,
		StopTimeout: 9 * time.Second,
		StatusRetry: 3 * time.Second,
	}
	cfg.withDefaults()

	assert.Equal(t, 7*time.Second, cfg.LockTimeout)
	assert.Equal(t, 9*time.Second, cfg.StopTimeout)
	assert.Equal(t, 3*time.Second, cfg.StatusRetry)
}

func TestRealClockAdvances(t *testing.T) {
	t.Parallel()

	before := realClock{}.Now()
	time.Sleep(2 * time.Millisecond)
	after := realClock{}.Now()
	assert.True(t, after.After(before))
}

func TestNewManagerIsUsable(t *testing.T) {
	t.Parallel()

	mgr := NewManager(DefaultConfig(t.TempDir(), afero.NewMemMapFs()), NewFakeDriver(DriverAbsent))
	require.NotNil(t, mgr)

	st, err := mgr.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, Uninitialized, st.State)
}
