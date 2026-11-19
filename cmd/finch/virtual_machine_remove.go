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

func newRemoveVMCommand(diskManager disk.UserDataDiskManager, logger flog.Logger, executor lifecycleExecutor) *cobra.Command {
	removeVMCommand := &cobra.Command{
		Use:   "remove",
		Short: "Remove the virtual machine instance",
		RunE:  newRemoveVMAction(diskManager, logger, executor).runAdapter,
	}

	removeVMCommand.Flags().BoolP("force", "f", false, "forcibly remove finch VM")

	return removeVMCommand
}

type removeVMAction struct {
	diskManager disk.UserDataDiskManager
	logger      flog.Logger
	executor    lifecycleExecutor
}

func newRemoveVMAction(diskManager disk.UserDataDiskManager, logger flog.Logger, executor lifecycleExecutor) *removeVMAction {
	return &removeVMAction{diskManager: diskManager, logger: logger, executor: executor}
}

func (rva *removeVMAction) runAdapter(cmd *cobra.Command, _ []string) error {
	force, err := cmd.Flags().GetBool("force")
	if err != nil {
		return err
	}
	return rva.run(force)
}

func (rva *removeVMAction) run(force bool) error {
	rva.stopCredentialServer()
	_ = rva.diskManager.DetachUserDataDisk()

	ctx, cancel := lifecycle.WithSignalHandling(signalBackgroundContext())
	defer cancel()

	res, err := rva.executor.Remove(ctx, force)
	if err != nil {
		return wrapLifecycleError(err)
	}
	rva.logger.Info(res.Message)
	return nil
}

func (rva *removeVMAction) stopCredentialServer() {
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
		rva.logger.Warnf("Failed to stop credential server: %v", err)
	}
}
