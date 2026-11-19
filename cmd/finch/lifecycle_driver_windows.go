// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"context"
	"os/exec"
)

// configureCancelableCommand wires context cancellation onto the limactl
// command. On Windows exec.CommandContext terminates the process via
// TerminateProcess when ctx ends; the job-object style process-tree kill is
// handled by the OS process tree semantics for a directly created child.
func configureCancelableCommand(_ context.Context, cmd *exec.Cmd) {
	cmd.WaitDelay = killWaitDelay
}
