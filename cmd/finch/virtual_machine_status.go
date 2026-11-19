// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || windows

package main

import (
	"fmt"
	"io"

	"github.com/runfinch/finch/pkg/flog"
	"github.com/runfinch/finch/pkg/lifecycle"

	"github.com/spf13/cobra"
)

func newStatusVMCommand(logger flog.Logger, stdout io.Writer, executor lifecycleExecutor) *cobra.Command {
	statusVMCommand := &cobra.Command{
		Use:   "status",
		Short: "Status of the virtual machine",
		RunE:  newStatusVMAction(logger, stdout, executor).runAdapter,
	}

	return statusVMCommand
}

type statusVMAction struct {
	logger   flog.Logger
	stdout   io.Writer
	executor lifecycleExecutor
}

func newStatusVMAction(logger flog.Logger, stdout io.Writer, executor lifecycleExecutor) *statusVMAction {
	return &statusVMAction{logger: logger, stdout: stdout, executor: executor}
}

func (sva *statusVMAction) runAdapter(_ *cobra.Command, _ []string) error {
	return sva.run()
}

// statusDisplayName maps the seven-state model onto the historical status
// vocabulary while preserving an explicit marker for every non-stable state.
func statusDisplayName(s lifecycle.State) string {
	switch s {
	case lifecycle.Uninitialized:
		return "Nonexistent"
	case lifecycle.InitializedStopped:
		// Historical `finch vm status` only had a single Stopped word; keep it for
		// stable-state compatibility, the distinction is available through the
		// detailed --verbose output below.
		return "Stopped"
	case lifecycle.Stopped:
		return "Stopped"
	case lifecycle.Running:
		return "Running"
	case lifecycle.Starting:
		return "Starting (in progress)"
	case lifecycle.Stopping:
		return "Stopping (in progress)"
	case lifecycle.Corrupted:
		return "Corrupted"
	default:
		return "Unknown"
	}
}

func (sva *statusVMAction) run() error {
	// Status is intentionally lock-free: a read-only command must never block on
	// a mutating command. Transitional states are marked explicitly instead of
	// being presented as stable.
	st, err := sva.executor.Status(signalBackgroundContext())
	if err != nil {
		return wrapLifecycleError(err)
	}
	if _, err := fmt.Fprintln(sva.stdout, statusDisplayName(st.State)); err != nil {
		return err
	}
	if st.State.IsTransitional() {
		if st.StaleHolder && st.Holder != nil {
			fmt.Fprintf(sva.stdout, "warning: interrupted %s operation from a process (pid %d) that no longer exists; "+
				"the next vm command will reconcile it\n", st.Holder.Action, st.Holder.PID)
		} else if st.Holder != nil {
			fmt.Fprintf(sva.stdout, "in progress: %s (pid %d)\n", st.Holder.Action, st.Holder.PID)
		}
	}
	if st.NeedsCheck {
		fmt.Fprintln(sva.stdout, "warning: the virtual machine needs inspection after a forced stop")
	}
	return nil
}
