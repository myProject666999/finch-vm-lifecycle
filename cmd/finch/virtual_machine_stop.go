// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || windows

package main

import (
	"os"
	"path/filepath"

	credserver "github.com/runfinch/finch/pkg/credserver"
	"github.com/runfinch/finch/pkg/disk"
	"github.com/runfinch/finch/pkg/flog"
	"github.com/runfinch/finch/pkg/lifecycle"

	"github.com/spf13/cobra"
)

func newStopVMCommand(diskManager disk.UserDataDiskManager, logger flog.Logger, executor lifecycleExecutor) *cobra.Command {
	stopVMCommand := &cobra.Command{
		Use:   "stop",
		Short: "Stop the virtual machine",
		RunE:  newStopVMAction(diskManager, logger, executor).runAdapter,
	}

	stopVMCommand.Flags().BoolP("force", "f", false, "forcibly stop finch VM")

	return stopVMCommand
}

type stopVMAction struct {
	diskManager disk.UserDataDiskManager
	logger      flog.Logger
	executor    lifecycleExecutor
}

func newStopVMAction(diskManager disk.UserDataDiskManager, logger flog.Logger, executor lifecycleExecutor) *stopVMAction {
	return &stopVMAction{diskManager: diskManager, logger: logger, executor: executor}
}

func (sva *stopVMAction) runAdapter(cmd *cobra.Command, _ []string) error {
	force, err := cmd.Flags().GetBool("force")
	if err != nil {
		return err
	}
	return sva.run(force)
}

func (sva *stopVMAction) run(force bool) error {
	// Best-effort host-side cleanup retained from the legacy implementation. The
	// state-machine decision about whether a stop is legal happens inside the
	// executor, under the lock.
	sva.stopCredentialServer()
	_ = sva.diskManager.DetachUserDataDisk()

	ctx, cancel := lifecycle.WithSignalHandling(signalBackgroundContext())
	defer cancel()

	res, err := sva.executor.Stop(ctx, force)
	if err != nil {
		return wrapLifecycleError(err)
	}
	if res.Noop {
		sva.logger.Info(res.Message)
		return nil
	}
	sva.logger.Info(res.Message)
	if res.NeedsCheck {
		sva.logger.Warnln("The virtual machine was forcibly stopped; run `finch vm start` to verify it is healthy")
	}
	return nil
}

// stopCredentialServer stops the credential server before the VM stops (macOS
// only). On Windows the call is a no-op. Failures are warnings, never fatal.
func (sva *stopVMAction) stopCredentialServer() {
	execPath, err := os.Executable()
	if err != nil {
		return
	}
	realPath, err := filepath.EvalSymlinks(execPath)
	if err != nil {
		realPath = execPath
	}
	finchRootPath := filepath.Dir(filepath.Dir(realPath))
	if err := credserver.StopCredentialServer(finchRootPath); err != nil {
		sva.logger.Warnf("Failed to stop credential server: %v", err)
	}
}
