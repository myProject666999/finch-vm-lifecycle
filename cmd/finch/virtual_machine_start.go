// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || windows

package main

import (
	"github.com/runfinch/finch/pkg/command"
	"github.com/runfinch/finch/pkg/config"
	"github.com/runfinch/finch/pkg/dependency"
	"github.com/runfinch/finch/pkg/disk"
	"github.com/runfinch/finch/pkg/flog"
	"github.com/runfinch/finch/pkg/lifecycle"

	"github.com/spf13/afero"
	"github.com/spf13/cobra"
)

func newStartVMCommand(
	ncc command.NerdctlCmdCreator,
	logger flog.Logger,
	optionalDepGroups []*dependency.Group,
	lca config.LimaConfigApplier,
	nca config.NerdctlConfigApplier,
	fs afero.Fs,
	privateKeyPath string,
	dm disk.UserDataDiskManager,
	executor lifecycleExecutor,
) *cobra.Command {
	return &cobra.Command{
		Use:      "start",
		Short:    "Start the virtual machine",
		RunE:     newStartVMAction(logger, executor).runAdapter,
		PostRunE: newPostVMStartInitAction(logger, ncc, fs, privateKeyPath, nca).runAdapter,
	}
}

type startVMAction struct {
	logger   flog.Logger
	executor lifecycleExecutor
}

func newStartVMAction(logger flog.Logger, executor lifecycleExecutor) *startVMAction {
	return &startVMAction{logger: logger, executor: executor}
}

func (sva *startVMAction) runAdapter(_ *cobra.Command, _ []string) error {
	return sva.run()
}

func (sva *startVMAction) run() error {
	ctx, cancel := lifecycle.WithSignalHandling(signalBackgroundContext())
	defer cancel()

	res, err := sva.executor.Start(ctx)
	if err != nil {
		return wrapLifecycleError(err)
	}
	if res.Noop {
		sva.logger.Info(res.Message)
		return nil
	}
	sva.logger.Info(res.Message)
	return nil
}
