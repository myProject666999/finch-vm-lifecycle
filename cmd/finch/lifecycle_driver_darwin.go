// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package main

import (
	"context"
	"os/exec"
	"syscall"
)

// configureCancelableCommand makes a limactl command terminate its whole process
// group when ctx is cancelled.
//
// exec.CommandContext by default sends SIGKILL only to the direct child; if
// limactl spawned helpers of its own they could be orphaned and keep touching
// the VM after the CLI returned, breaking the "no work outlives the lock"
// invariant. Putting the child in its own process group and killing the group
// on cancellation closes that window.
func configureCancelableCommand(ctx context.Context, cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Negative pid addresses the whole process group led by the child.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// WaitDelay reaps the child even if a helper ignores the signal, rather than
	// letting Wait block indefinitely.
	cmd.WaitDelay = killWaitDelay
}
