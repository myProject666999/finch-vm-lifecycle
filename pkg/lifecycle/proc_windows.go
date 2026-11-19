// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package lifecycle

import (
	"golang.org/x/sys/windows"
)

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
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	_ = windows.CloseHandle(h)
	return true
}
