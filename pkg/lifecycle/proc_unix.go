// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package lifecycle

import "golang.org/x/sys/unix"

// processChecker reports whether an operating-system process still exists.
type processChecker interface {
	Alive(pid int) bool
}

// osProcessChecker is the production implementation.
type osProcessChecker struct{}

func (osProcessChecker) Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	// Signal 0 performs only existence/permission checks without delivering a
	// signal. ESRCH means the process is gone; EPERM means it exists but belongs
	// to another user, which still counts as alive.
	err := unix.Kill(pid, 0)
	return err == nil || err == unix.EPERM
}
