// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || windows

package main

import (
	"os"
	"runtime"
)

// pathListSep is the PATH list separator for the current platform.
var pathListSep = func() string {
	if runtime.GOOS == "windows" {
		return ";"
	}
	return ":"
}()

func environ() []string { return os.Environ() }

func getEnv(name string) string { return os.Getenv(name) }
