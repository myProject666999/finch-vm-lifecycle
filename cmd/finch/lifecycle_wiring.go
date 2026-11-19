// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || windows

package main

import (
	"context"
	"path/filepath"

	"github.com/runfinch/finch/pkg/config"
	"github.com/runfinch/finch/pkg/dependency"
	"github.com/runfinch/finch/pkg/disk"
	"github.com/runfinch/finch/pkg/flog"
	"github.com/runfinch/finch/pkg/lifecycle"
	"github.com/runfinch/finch/pkg/path"

	"github.com/spf13/afero"
)

// lifecycleExecutor is the slice of the Manager API the vm commands depend on.
// Depending on the narrow interface keeps the cobra actions testable with any
// lifecycle.Driver, including the in-memory fake.
type lifecycleExecutor interface {
	Status(ctx context.Context) (lifecycle.StatusResult, error)
	Init(ctx context.Context) (*lifecycle.ActionResult, error)
	Start(ctx context.Context) (*lifecycle.ActionResult, error)
	Stop(ctx context.Context, force bool) (*lifecycle.ActionResult, error)
	Remove(ctx context.Context, force bool) (*lifecycle.ActionResult, error)
}

// Compile-time check that the real manager satisfies the command-facing interface.
var _ lifecycleExecutor = (*lifecycle.Manager)(nil)

// vmStateDir returns the per-instance directory that holds vm.lock and
// operation.json. It lives inside the Finch config directory (~/.finch) on a
// local filesystem so flock/LockFileEx semantics apply; it never goes on a
// network mount.
func vmStateDir(fp path.Finch, finchRootPath string) string {
	return filepath.Join(fp.FinchDir(finchRootPath), "vm", limaInstanceName)
}

// newLimaDriverFor builds the production limactl driver from the Finch
// installation layout.
func newLimaDriverFor(fp path.Finch, finchConfig *config.Finch) *LimaDriver {
	return NewLimaDriver(LimaDriverConfig{
		LimactlPath:  fp.LimactlPath(),
		LimaHomePath: fp.LimaHomePath(),
		QemuBinDir:   fp.QEMUBinDir(),
		InstanceName: limaInstanceName,
		BaseYamlPath: fp.BaseYamlFilePath(),
		VZDriver:     finchConfig == nil || *finchConfig.VMType == "vz",
	})
}

// newVMExecutor builds the executor shared by every vm command: the limactl
// driver wrapped with the historical pre-flight steps, all behind one
// cross-process lock in the same per-instance state directory.
func newVMExecutor(
	fp path.Finch,
	finchRootPath string,
	fs afero.Fs,
	finchConfig *config.Finch,
	logger flog.Logger,
	optionalDepGroups []*dependency.Group,
	lca config.LimaConfigApplier,
	dm disk.UserDataDiskManager,
) *lifecycle.Manager {
	stateDir := vmStateDir(fp, finchRootPath)
	driver := newLimaDriverFor(fp, finchConfig)
	return newPreparedManager(stateDir, fs, driver, logger, optionalDepGroups, lca, dm)
}

// preparedDriver wraps a lifecycle.Driver so the historical pre-flight work
// (optional dependencies, lima yaml configuration, user-data disk) still runs,
// but now executes *inside the cross-process lock* immediately before the
// underlying limactl invocation, instead of unprotected before the state check.
type preparedDriver struct {
	inner               lifecycle.Driver
	logger              flog.Logger
	optionalDepGroups   []*dependency.Group
	limaConfigApplier   config.LimaConfigApplier
	userDataDiskManager disk.UserDataDiskManager
}

func (d *preparedDriver) prepareStart() error {
	d.logger.Debugln("Running start pre-flight steps under the lifecycle lock")
	dependency.InstallOptionalDeps(d.optionalDepGroups, d.logger)
	if err := d.limaConfigApplier.ConfigureOverrideLimaYaml(); err != nil {
		return err
	}
	return d.userDataDiskManager.EnsureUserDataDisk()
}

func (d *preparedDriver) prepareInit() error {
	d.logger.Debugln("Running init pre-flight steps under the lifecycle lock")
	if err := d.limaConfigApplier.ConfigureDefaultLimaYaml(d.logger); err != nil {
		return err
	}
	if err := d.limaConfigApplier.ConfigureOverrideLimaYaml(); err != nil {
		return err
	}
	dependency.InstallOptionalDeps(d.optionalDepGroups, d.logger)
	// Best effort: only detach so the disk is never mounted twice.
	_ = d.userDataDiskManager.DetachUserDataDisk()
	return d.userDataDiskManager.EnsureUserDataDisk()
}

// Status passes through; the pre-flight steps never belong to a read-only query.
func (d *preparedDriver) Status(ctx context.Context) (lifecycle.DriverState, error) {
	return d.inner.Status(ctx)
}

// Init runs the init pre-flight steps, then delegates to the inner driver.
func (d *preparedDriver) Init(ctx context.Context) error {
	if err := d.prepareInit(); err != nil {
		return err
	}
	return d.inner.Init(ctx)
}

// Start runs the start pre-flight steps, then delegates to the inner driver.
func (d *preparedDriver) Start(ctx context.Context) error {
	if err := d.prepareStart(); err != nil {
		return err
	}
	return d.inner.Start(ctx)
}

// Stop delegates directly; disk detach stays in the command as best-effort.
func (d *preparedDriver) Stop(ctx context.Context) error { return d.inner.Stop(ctx) }

// ForceStop delegates directly.
func (d *preparedDriver) ForceStop(ctx context.Context) error { return d.inner.ForceStop(ctx) }

// Remove delegates directly.
func (d *preparedDriver) Remove(ctx context.Context, force bool) error {
	return d.inner.Remove(ctx, force)
}

var _ lifecycle.Driver = (*preparedDriver)(nil)

// newPreparedManager wraps driver with the historical pre-flight work and
// returns a manager over the same state directory, so that pre-flight work is
// covered by the same cross-process lock as the limactl invocation.
func newPreparedManager(
	stateDir string,
	fs afero.Fs,
	driver lifecycle.Driver,
	logger flog.Logger,
	optionalDepGroups []*dependency.Group,
	lca config.LimaConfigApplier,
	dm disk.UserDataDiskManager,
) *lifecycle.Manager {
	prepared := &preparedDriver{
		inner:               driver,
		logger:              logger,
		optionalDepGroups:   optionalDepGroups,
		limaConfigApplier:   lca,
		userDataDiskManager: dm,
	}
	return lifecycle.NewManager(lifecycle.DefaultConfig(stateDir, fs), prepared)
}
