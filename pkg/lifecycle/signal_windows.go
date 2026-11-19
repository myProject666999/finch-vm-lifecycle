// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package lifecycle

import (
	"context"
	"os"
	"os/signal"
)

// WithSignalHandling returns a context cancelled on interrupt. Windows has no
// SIGTERM; Ctrl-C and Ctrl-Break both arrive as os.Interrupt when a console
// control handler is installed by the Go runtime.
//
// The signal policy is identical to Unix: the driver invocation is terminated
// together with the CLI and the operation record is reconciled before exit, so
// no driver work can outlive the released lock.
func WithSignalHandling(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, os.Interrupt)
}
