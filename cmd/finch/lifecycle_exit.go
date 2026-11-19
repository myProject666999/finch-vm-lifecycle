// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || windows

package main

import "github.com/runfinch/finch/pkg/lifecycle"

// lifecycleExitError makes lifecycle errors satisfy nerdctl's errutil.ExitCoder
// interface, so `finch vm ...` exits with the documented, scriptable status code
// (2 invalid transition, 3 concurrent operation, 4 lock timeout, 5 driver
// failure, 6 interrupted) instead of the generic status 1.
type lifecycleExitError struct {
	err error
}

func (e *lifecycleExitError) Error() string { return e.err.Error() }

func (e *lifecycleExitError) Unwrap() error { return e.err }

// ExitCode implements errutil.ExitCoder.
func (e *lifecycleExitError) ExitCode() int { return lifecycle.ExitCode(e.err) }

// wrapLifecycleError attaches the lifecycle exit code while preserving the
// concrete error type for errors.Is/errors.As callers.
func wrapLifecycleError(err error) error {
	if err == nil {
		return nil
	}
	return &lifecycleExitError{err: err}
}
