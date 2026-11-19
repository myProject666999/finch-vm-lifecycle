// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package lifecycle

import (
	"time"

	"github.com/spf13/afero"
)

const (
	// lockFileName is the per-VM lock file name inside the state directory.
	lockFileName = "vm.lock"

	// DefaultLockTimeout is how long a mutating command waits for the per-VM lock
	// before failing with an exit code 4 error that names the holder. Thirty
	// seconds comfortably exceeds the normal gap between two manual commands
	// while keeping accidental double-clicks responsive.
	DefaultLockTimeout = 30 * time.Second

	// DefaultStatusRetry is how long the manager keeps re-querying a driver that
	// answers ErrDriverNotReady before giving up.
	DefaultStatusRetry = 5 * time.Second

	// DefaultStopTimeout is the graceful-stop deadline. If Stop has not returned
	// by then, the manager escalates to ForceStop.
	DefaultStopTimeout = 60 * time.Second

	// retryBackoff is the pause between transient driver queries.
	retryBackoff = 200 * time.Millisecond
)

// Config configures a [Manager]. The zero value is not usable; use
// [DefaultConfig] and override individual fields.
type Config struct {
	// StateDir is the per-instance directory holding vm.lock and operation.json.
	// It must be on a filesystem that supports the platform file-lock semantics
	// (a local disk; never NFS for flock). In production it lives inside the
	// Finch state/run directory, e.g. <finch-dir>/vm/finch/.
	StateDir string
	// Fs stores the JSON operation record. Tests pass an in-memory afero fs;
	// production passes afero.NewOsFs().
	Fs afero.Fs
	// LockTimeout bounds lock waiting. Zero means DefaultLockTimeout.
	LockTimeout time.Duration
	// StopTimeout bounds a graceful stop before force escalation.
	StopTimeout time.Duration
	// StatusRetry bounds retrying transient driver-status failures.
	StatusRetry time.Duration
	// NonBlocking makes lock acquisition fail immediately instead of waiting.
	// It is intended for scripted callers that prefer to back off themselves.
	NonBlocking bool
	// Clock and processChecker default to the real implementations; tests inject
	// fakes to simulate dead holders deterministically.
	Clock          Clock
	ProcessChecker processChecker
}

// DefaultConfig returns a config with production defaults for the given state
// directory and filesystem.
func DefaultConfig(stateDir string, fs afero.Fs) Config {
	return Config{
		StateDir:       stateDir,
		Fs:             fs,
		LockTimeout:    DefaultLockTimeout,
		StopTimeout:    DefaultStopTimeout,
		StatusRetry:    DefaultStatusRetry,
		Clock:          realClock{},
		ProcessChecker: osProcessChecker{},
	}
}

func (c *Config) withDefaults() {
	if c.LockTimeout == 0 {
		c.LockTimeout = DefaultLockTimeout
	}
	if c.StopTimeout == 0 {
		c.StopTimeout = DefaultStopTimeout
	}
	if c.StatusRetry == 0 {
		c.StatusRetry = DefaultStatusRetry
	}
	if c.Clock == nil {
		c.Clock = realClock{}
	}
	if c.ProcessChecker == nil {
		c.ProcessChecker = osProcessChecker{}
	}
}
