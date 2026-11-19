// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package lifecycle

import (
	"context"
	"errors"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// fileLock is an advisory, cross-process exclusive lock backed by flock(2).
//
// Why flock and not fcntl:
//
//   - flock locks are owned by the open file description and released by the
//     kernel when the process exits or is killed, even on SIGKILL, so a crashed
//     holder never leaves an enforced residual lock. The lock *file* may remain
//     on disk, but reacquiring it succeeds immediately.
//   - flock is advisory and works on the local filesystems Finch uses for its
//     state directory. fcntl POSIX locks, by contrast, are associated with the
//     (pid, inode) pair and are silently released whenever any thread in the
//     process closes any descriptor referring to the file, which is unsafe for
//     a long-lived CLI with helper goroutines.
//
// The lock file is deliberately separate from the JSON operation record: the
// record is rewritten (and may be rewritten by a taking-over process) while the
// lock stays held on a stable descriptor.
type fileLock struct {
	path string
	f    *os.File
}

func newFileLock(dir string) *fileLock {
	return &fileLock{path: dir + "/" + lockFileName}
}

// tryAcquire opens (creating if needed) the lock file and takes a non-blocking
// exclusive lock. It returns true on success and false if another live holder
// owns the lock.
func (l *fileLock) tryAcquire() (bool, error) {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, err
	}
	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		_ = f.Close()
		return false, nil
	}
	if err != nil {
		_ = f.Close()
		return false, err
	}
	l.f = f
	return true, nil
}

// release drops the lock and closes the descriptor. It is idempotent.
func (l *fileLock) release() error {
	if l.f == nil {
		return nil
	}
	err := unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
	if closeErr := l.f.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	l.f = nil
	return err
}

// Acquire blocks until the lock is held, ctx is cancelled, or timeout elapses.
//
// Stale handling: flock itself cannot be held by a dead process, so a contended
// lock always means a live process. That process's identity is read from the
// operation record by the caller-facing manager and surfaced in the timeout
// error; this method only reports that a holder exists.
func (l *fileLock) Acquire(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	backoff := 20 * time.Millisecond
	const maxBackoff = 250 * time.Millisecond
	for {
		got, err := l.tryAcquire()
		if err != nil {
			return err
		}
		if got {
			return nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return ErrLockTimeout
		}
		if backoff > remaining {
			backoff = remaining
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// AcquireNoWait makes one non-blocking attempt.
func (l *fileLock) AcquireNoWait() (bool, error) {
	return l.tryAcquire()
}
