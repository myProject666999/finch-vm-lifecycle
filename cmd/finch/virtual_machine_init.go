// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || windows

package main

import (
	"runtime"

	"github.com/runfinch/finch/pkg/command"
	"github.com/runfinch/finch/pkg/config"
	"github.com/runfinch/finch/pkg/dependency"
	"github.com/runfinch/finch/pkg/disk"
	"github.com/runfinch/finch/pkg/flog"
	"github.com/runfinch/finch/pkg/lifecycle"

	"github.com/spf13/afero"
	"github.com/spf13/cobra"
)

func newInitVMCommand(
	ncc command.NerdctlCmdCreator,
	logger flog.Logger,
	optionalDepGroups []*dependency.Group,
	lca config.LimaConfigApplier,
	nca config.NerdctlConfigApplier,
	baseYamlFilePath string,
	fs afero.Fs,
	privateKeyPath string,
	diskManager disk.UserDataDiskManager,
	finchConfig *config.Finch,
	executor lifecycleExecutor,
) *cobra.Command {
	initVMCommand := &cobra.Command{
		Use:      "init",
		Short:    "Initialize the virtual machine",
		RunE:     newInitVMAction(logger, executor).runAdapter,
		PostRunE: newPostVMStartInitAction(logger, ncc, fs, privateKeyPath, nca).runAdapter,
	}

	return initVMCommand
}

type initVMAction struct {
	logger   flog.Logger
	executor lifecycleExecutor
}

func newInitVMAction(logger flog.Logger, executor lifecycleExecutor) *initVMAction {
	return &initVMAction{logger: logger, executor: executor}
}

func (iva *initVMAction) runAdapter(_ *cobra.Command, _ []string) error {
	return iva.run()
}

func (iva *initVMAction) run() error {
	ctx, cancel := lifecycle.WithSignalHandling(signalBackgroundContext())
	defer cancel()

	res, err := iva.executor.Init(ctx)
	if err != nil {
		return wrapLifecycleError(err)
	}
	iva.logger.Info(res.Message)

	if runtime.GOOS == "windows" {
		iva.logger.Warnln("Finch on Windows uses WSL, which mounts the C Drive in read-write mode by default. " +
			"To run finch with more restricted access, follow " +
			"https://runfinch.com/docs/managing-finch/windows/wsl-configuration/")
	}

	iva.logger.Warnln("Please note that Finch is a developer tool and is not meant for use in production environments, " +
		"especially one running multi-tenant workloads. The virtual machine that Finch manages on macOS and Windows " +
		"does not create a security boundary and exists for the sole purpose of running Linux containers on macOS and Windows.")

	return nil
}
