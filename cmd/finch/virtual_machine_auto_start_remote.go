// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || windows

package main

import (
	"context"
	"fmt"

	"github.com/spf13/afero"

	"github.com/runfinch/finch/pkg/command"
	"github.com/runfinch/finch/pkg/config"
	"github.com/runfinch/finch/pkg/flog"
	"github.com/runfinch/finch/pkg/lifecycle"
	"github.com/runfinch/finch/pkg/lima"
	"github.com/runfinch/finch/pkg/path"
)

type vmAutoStarter struct {
	ncc           command.NerdctlCmdCreator
	ecc           command.Creator
	logger        flog.Logger
	fp            path.Finch
	fs            afero.Fs
	fc            *config.Finch
	home          string
	finchRootPath string
	// executorOverride, when non-nil, replaces the production lock-protected
	// executor. Tests inject one backed by the in-memory fake driver.
	executorOverride lifecycleExecutor
}

func newVMAutoStarter(
	ncc command.NerdctlCmdCreator,
	ecc command.Creator,
	logger flog.Logger,
	fp path.Finch,
	fs afero.Fs,
	fc *config.Finch,
	home string,
	finchRootPath string,
) VMAutoStarter {
	return &vmAutoStarter{
		ncc:           ncc,
		ecc:           ecc,
		logger:        logger,
		fp:            fp,
		fs:            fs,
		fc:            fc,
		home:          home,
		finchRootPath: finchRootPath,
	}
}

// executor builds the same lock-protected executor the `finch vm` commands use,
// so automatic start/init participates in the identical state machine and
// cross-process mutual exclusion.
func (v *vmAutoStarter) executor() (lifecycleExecutor, *vmDependencies) {
	// An injected executor (tests) needs none of the production dependency wiring;
	// skip it so a test harness does not have to construct a full Lima stack.
	if v.executorOverride != nil {
		return v.executorOverride, nil
	}
	deps := buildVMDependencies(v.logger, v.fp, v.ncc, v.ecc, v.fs, v.fc, v.home, v.finchRootPath)
	exec := newVMExecutor(v.fp, v.finchRootPath, v.fs, v.fc, v.logger, deps.optionalDepGroups, deps.lca,
		deps.diskManager)
	return exec, deps
}

func (v *vmAutoStarter) EnsureVMRunning() error {
	// Use the lock-free status path to choose between init and start without
	// blocking; the actual mutating call then takes the lock like any other.
	deps := buildVMDependencies(v.logger, v.fp, v.ncc, v.ecc, v.fs, v.fc, v.home, v.finchRootPath)
	driverStatus, err := lima.GetVMStatus(v.ncc, v.logger, limaInstanceName)
	if err != nil {
		return err
	}

	switch driverStatus {
	case lima.Running:
		return nil
	case lima.Nonexistent:
		return v.initVM(deps)
	case lima.Stopped:
		return v.startVM(deps)
	default:
		return fmt.Errorf("instance %q has an unknown status", limaInstanceName)
	}
}

func (v *vmAutoStarter) initVM(deps *vmDependencies) error {
	exec, _ := v.executor()
	ctx, cancel := lifecycle.WithSignalHandling(context.Background())
	defer cancel()
	if _, err := exec.Init(ctx); err != nil {
		return err
	}
	return v.postStartConfig(deps)
}

func (v *vmAutoStarter) startVM(deps *vmDependencies) error {
	exec, _ := v.executor()
	ctx, cancel := lifecycle.WithSignalHandling(context.Background())
	defer cancel()
	if _, err := exec.Start(ctx); err != nil {
		return err
	}
	return v.postStartConfig(deps)
}

func (v *vmAutoStarter) postStartConfig(deps *vmDependencies) error {
	postAction := newPostVMStartInitAction(
		v.logger,
		v.ncc,
		v.fs,
		v.fp.LimaSSHPrivateKeyPath(),
		deps.nca,
	)
	return postAction.run()
}
