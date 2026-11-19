// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package lifecycle

import (
	"context"
	"errors"
)

// DriverState is the coarse state reported by the underlying virtual machine
// management tool. The driver only knows what the hypervisor can answer; the
// richer lifecycle states (transitional, corrupted, initialized-vs-stopped) are
// derived by the manager from driver state plus its own operation record.
type DriverState int

const (
	// DriverAbsent means the tool reports no matching instance.
	DriverAbsent DriverState = iota
	// DriverRunning means the tool reports the instance as running.
	DriverRunning
	// DriverStopped means the tool reports the instance as existing but stopped.
	DriverStopped
)

// String renders the driver state for logs and tests.
func (d DriverState) String() string {
	switch d {
	case DriverAbsent:
		return "absent"
	case DriverRunning:
		return "running"
	case DriverStopped:
		return "stopped"
	default:
		return "unknown"
	}
}

// ErrDriverNotReady is returned by a driver that cannot answer a query for a
// transient reason (tool busy, I/O hiccup). The manager retries the query once
// before surfacing the error, so that one flaky query does not get misclassified
// as Corrupted.
var ErrDriverNotReady = errors.New("driver not ready, retry later")

// Driver abstracts every call into the underlying virtual machine management
// tool (for production Finch: limactl).
//
// Implementations must satisfy:
//
//   - All methods are safe for concurrent use. The manager serializes mutating
//     calls with a cross-process lock, but Status may run lock-free at any time.
//   - Mutating methods honor ctx cancellation: when ctx is done (e.g. the user
//     pressed Ctrl-C), the implementation must terminate the underlying tool
//     invocation rather than letting it run orphaned, and then return
//     context.Canceled (or context.DeadlineExceeded). No goroutine may keep
//     touching the VM after the method returns.
//   - A returned error means the requested end state was not reached; it does not
//     necessarily mean the VM is unchanged. The manager reconciles real state by
//     querying Status after every failure.
type Driver interface {
	// Status returns the current state of the instance as known to the tool.
	Status(ctx context.Context) (DriverState, error)

	// Init creates the instance and starts it for the first time (matching the
	// historical semantics of `finch vm init`, which leaves a running VM).
	Init(ctx context.Context) error
	// Start starts an existing, stopped instance.
	Start(ctx context.Context) error
	// Stop requests a graceful stop. A graceful stop that does not finish by the
	// manager's deadline is escalated to ForceStop.
	Stop(ctx context.Context) error
	// ForceStop forcibly terminates the instance. It is used either because the
	// caller passed --force, or because a graceful Stop exceeded its deadline.
	// A force stop always leaves the needs-check annotation set.
	ForceStop(ctx context.Context) error
	// Remove deletes the instance. force permits removing a running instance by
	// force-stopping it first.
	Remove(ctx context.Context, force bool) error
}

// DriverCall records one invocation of a Driver mutating method. The fake driver
// records a slice of these so tests can assert that calls never overlap and
// happen in the expected order.
type DriverCall struct {
	Action Action
	Force  bool
	// Seq is a monotonically increasing call number, assigned under the fake
	// driver's internal mutex.
	Seq int
}
