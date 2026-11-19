// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || windows

package main

import (
	"errors"
	"testing"

	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/runfinch/finch/pkg/config"
	"github.com/runfinch/finch/pkg/flog"
	"github.com/runfinch/finch/pkg/lifecycle"
	"github.com/runfinch/finch/pkg/mocks"
)

func TestVMAutoStarter(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name    string
		status  string
		statErr error
		wantErr string
	}{
		{
			name:   "running VM is a no-op",
			status: "Running",
		},
		{
			name:    "unknown status returns error",
			status:  "Broken",
			wantErr: "unrecognized system status",
		},
		{
			name:    "status command failure propagates error",
			status:  "something else",
			statErr: errors.New("error"),
			wantErr: "error",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctrl := gomock.NewController(t)
			ncc := mocks.NewNerdctlCmdCreator(ctrl)
			logger := mocks.NewLogger(ctrl)

			getVMStatusC := mocks.NewCommand(ctrl)
			ncc.EXPECT().CreateWithoutStdio("ls", "-f", "{{.Status}}", limaInstanceName).Return(getVMStatusC)
			getVMStatusC.EXPECT().Output().Return([]byte(tc.status), tc.statErr)

			if tc.statErr == nil {
				logger.EXPECT().Debugf("Status of virtual machine: %s", tc.status)
			}

			starter := &vmAutoStarter{
				ncc:    ncc,
				logger: logger,
				fs:     afero.NewMemMapFs(),
				fc:     &config.Finch{},
			}

			err := starter.EnsureVMRunning()
			if tc.wantErr == "" {
				assert.NoError(t, err)
			} else {
				assert.ErrorContains(t, err, tc.wantErr)
			}
		})
	}
}

// TestVMAutoStarterUsesLifecycleExecutor verifies the auto-start path drives the
// same lock-protected lifecycle state machine as the explicit vm commands when
// an executor is injected (production injects the limactl-backed one).
func TestVMAutoStarterUsesLifecycleExecutor(t *testing.T) {
	t.Parallel()

	t.Run("stopped instance gets started through the executor", func(t *testing.T) {
		t.Parallel()
		executor, driver, _ := newTestExecutor(t, lifecycle.DriverStopped)
		starter := &vmAutoStarter{
			logger:           flog.NewLogrus(),
			fs:               afero.NewMemMapFs(),
			fc:               &config.Finch{},
			executorOverride: executor,
		}

		exec, _ := starter.executor()
		_, err := exec.Start(t.Context())
		require.NoError(t, err)
		assert.Equal(t, lifecycle.DriverRunning, driver.State())
	})

	t.Run("absent instance gets initialized through the executor", func(t *testing.T) {
		t.Parallel()
		executor, driver, _ := newTestExecutor(t, lifecycle.DriverAbsent)
		starter := &vmAutoStarter{
			logger:           flog.NewLogrus(),
			fs:               afero.NewMemMapFs(),
			fc:               &config.Finch{},
			executorOverride: executor,
		}

		exec, _ := starter.executor()
		_, err := exec.Init(t.Context())
		require.NoError(t, err)
		assert.Equal(t, lifecycle.DriverRunning, driver.State())
		assert.Equal(t, lifecycle.ActionInit, driver.Calls()[0].Action)
	})
}
