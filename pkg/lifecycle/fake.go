// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package lifecycle

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"
)

// FakeDriver is an in-memory [Driver] for tests. It models the real driver's
// state transitions and lets every action be blocked, delayed or scripted to
// fail, while recording the exact sequence of calls and asserting that mutating
// calls never overlap.
type FakeDriver struct {
	mu sync.Mutex
	// state is the simulated hypervisor state.
	state DriverState
	// inFlight counts concurrently executing mutating calls; maxInFlight captures
	// its peak so tests can assert serialization.
	inFlight    int
	maxInFlight int
	seq         int
	calls       []DriverCall
	// overlap is incremented whenever a mutating call starts while another is in
	// flight. Correct locking keeps it at zero forever.
	overlap int

	// failFor maps an action to the error its next invocation returns before
	// changing state.
	failFor map[Action]error
	// failRemaining counts how many more times each action should fail.
	failRemaining map[Action]int
	// stateAfterFail, when set, is applied before the failure is returned,
	// modeling a tool that died after partially mutating the VM.
	stateAfterFail map[Action]DriverState
	// delayFor makes each invocation sleep (while honoring ctx) before running.
	delayFor map[Action]time.Duration
	// gates, when present, block the action until the channel closes or ctx ends.
	gates map[Action]chan struct{}

	// statusErr is returned by Status until statusErrRemaining hits zero.
	statusErr          error
	statusErrRemaining int
	// statusGate blocks Status calls like gates do for mutating actions.
	statusGate chan struct{}
}

// NewFakeDriver builds a fake starting in the given driver state.
func NewFakeDriver(initial DriverState) *FakeDriver {
	return &FakeDriver{
		state:          initial,
		failFor:        map[Action]error{},
		failRemaining:  map[Action]int{},
		stateAfterFail: map[Action]DriverState{},
		delayFor:       map[Action]time.Duration{},
		gates:          map[Action]chan struct{}{},
	}
}

// SetState overwrites the simulated state (used to simulate crashes and external
// changes between manager calls).
func (f *FakeDriver) SetState(s DriverState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = s
}

// State returns the simulated state.
func (f *FakeDriver) State() DriverState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

// FailNext makes the next count invocations of action fail with err without
// changing state.
func (f *FakeDriver) FailNext(action Action, count int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failFor[action] = err
	f.failRemaining[action] = count
}

// FailNextLeavingState is like FailNext but also applies leaveState before
// returning the error, modeling an underlying tool that died after partially
// mutating the VM (e.g. an init that created a half-bootable instance).
func (f *FakeDriver) FailNextLeavingState(action Action, count int, leaveState DriverState, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failFor[action] = err
	f.failRemaining[action] = count
	f.stateAfterFail[action] = leaveState
}

// Delay makes every invocation of action pause for d (honoring cancellation).
func (f *FakeDriver) Delay(action Action, d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delayFor[action] = d
}

// Block installs a gate for action. The action blocks until Release is called
// or the context is cancelled.
func (f *FakeDriver) Block(action Action) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gates[action] = make(chan struct{})
}

// Release opens the gate for action, unblocking a waiting invocation.
func (f *FakeDriver) Release(action Action) {
	f.mu.Lock()
	ch, ok := f.gates[action]
	f.mu.Unlock()
	if ok {
		close(ch)
	}
}

// BlockStatus installs a gate for Status.
func (f *FakeDriver) BlockStatus() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusGate = make(chan struct{})
}

// ReleaseStatus opens the Status gate.
func (f *FakeDriver) ReleaseStatus() {
	f.mu.Lock()
	ch := f.statusGate
	f.statusGate = nil
	f.mu.Unlock()
	if ch != nil {
		close(ch)
	}
}

// FailStatus makes the next count Status calls return err.
func (f *FakeDriver) FailStatus(count int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusErr = err
	f.statusErrRemaining = count
}

// Calls returns a copy of the recorded call sequence.
func (f *FakeDriver) Calls() []DriverCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// MaxConcurrent returns the peak number of simultaneously executing mutating
// calls. It must be 1 when the manager's locking works.
func (f *FakeDriver) MaxConcurrent() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxInFlight
}

// OverlapCount returns how many times a call started while another was running.
func (f *FakeDriver) OverlapCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.overlap
}

// ResetHistory clears the call history (state is untouched).
func (f *FakeDriver) ResetHistory() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
	f.inFlight = 0
	f.maxInFlight = 0
	f.overlap = 0
}

// Status implements Driver.
func (f *FakeDriver) Status(ctx context.Context) (DriverState, error) {
	f.mu.Lock()
	gate := f.statusGate
	if f.statusErrRemaining > 0 {
		f.statusErrRemaining--
		err := f.statusErr
		f.mu.Unlock()
		return 0, err
	}
	state := f.state
	f.mu.Unlock()

	if gate != nil {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-gate:
		}
	}
	return state, nil
}

// enter records the start of a mutating call and detects overlap.
func (f *FakeDriver) enter(action Action, force bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	f.calls = append(f.calls, DriverCall{Action: action, Force: force, Seq: f.seq})
	if f.inFlight > 0 {
		f.overlap++
	}
	f.inFlight++
	if f.inFlight > f.maxInFlight {
		f.maxInFlight = f.inFlight
	}
}

func (f *FakeDriver) exit() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inFlight--
}

// wait honors the configured delay and gate for action.
func (f *FakeDriver) wait(ctx context.Context, action Action) error {
	f.mu.Lock()
	delay := f.delayFor[action]
	gate := f.gates[action]
	f.mu.Unlock()

	if delay > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	if gate != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-gate:
		}
	}
	return nil
}

// scriptedFailure returns the configured error for action, if any, consuming one
// failure credit and applying any configured partial-state change.
func (f *FakeDriver) scriptedFailure(action Action) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failRemaining[action] > 0 {
		f.failRemaining[action]--
		err := f.failFor[action]
		if leave, ok := f.stateAfterFail[action]; ok {
			f.state = leave
		}
		if f.failRemaining[action] == 0 {
			delete(f.failFor, action)
			delete(f.stateAfterFail, action)
		}
		return err
	}
	return nil
}

// Init implements Driver.
func (f *FakeDriver) Init(ctx context.Context) error {
	return f.mutate(ctx, ActionInit, false, func() { f.state = DriverRunning })
}

// Start implements Driver.
func (f *FakeDriver) Start(ctx context.Context) error {
	return f.mutate(ctx, ActionStart, false, func() { f.state = DriverRunning })
}

// Stop implements Driver.
func (f *FakeDriver) Stop(ctx context.Context) error {
	return f.mutate(ctx, ActionStop, false, func() { f.state = DriverStopped })
}

// ForceStop implements Driver.
func (f *FakeDriver) ForceStop(ctx context.Context) error {
	return f.mutate(ctx, ActionStop, true, func() { f.state = DriverStopped })
}

// Remove implements Driver.
func (f *FakeDriver) Remove(ctx context.Context, force bool) error {
	return f.mutate(ctx, ActionRemove, force, func() { f.state = DriverAbsent })
}

func (f *FakeDriver) mutate(ctx context.Context, action Action, force bool, apply func()) error {
	f.enter(action, force)
	defer f.exit()

	if err := f.wait(ctx, action); err != nil {
		return err
	}
	if err := f.scriptedFailure(action); err != nil {
		return err
	}
	f.mu.Lock()
	apply()
	f.mu.Unlock()
	return nil
}

// Ensure FakeDriver satisfies the Driver interface.
var _ Driver = (*FakeDriver)(nil)

// fakeProcChecker is a ProcessChecker whose answer set is configured by tests,
// so stale-holder takeover can be exercised without killing real processes.
type fakeProcChecker struct {
	mu       sync.Mutex
	deadPIDs map[int]bool
}

func newFakeProcChecker() *fakeProcChecker { return &fakeProcChecker{deadPIDs: map[int]bool{}} }

func (c *fakeProcChecker) MarkDead(pid int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadPIDs[pid] = true
}

func (c *fakeProcChecker) Alive(pid int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.deadPIDs[pid]
}

// errFakeAction is a convenience constructor for scripted failures.
func errFakeAction(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}
