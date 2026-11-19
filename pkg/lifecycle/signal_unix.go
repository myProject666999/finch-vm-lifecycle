// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package lifecycle

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// WithSignalHandling returns a context that is cancelled when the CLI receives
// an interrupt or termination signal.
//
// Chosen signal policy: terminate the underlying action together with the CLI.
//
// The alternative — letting the underlying tool invocation keep running in the
// background while the command exits — was rejected because it inherently opens
// a window where the command has exited and released the lock while driver work
// is still mutating the VM, which is exactly the "guess the intermediate state"
// situation this package exists to prevent. Keeping the lock held after the
// foreground command exits would in turn require a detached supervisor process
// that Finch does not have.
//
// With this policy the invariant is simple: the lock is released only after the
// driver method has returned, and the operation record is reconciled against a
// fresh driver query on the way out, so the holder record always matches real
// progress. Production drivers translate the cancellation into terminating the
// tool process group (see the limactl driver), which kills the whole child tree
// rather than just the wrapper.
func WithSignalHandling(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
}
