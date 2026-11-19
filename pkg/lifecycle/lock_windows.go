// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package lifecycle

import (
	"context"
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// fileLock is a cross-process exclusive lock backed by LockFileEx.
//
// As with flock on Unix, the lock is owned by the operating-system handle and is
// released by the kernel when the holder process exits or is terminated, even
// forcibly: there is no residual enforced lock after a crash, only the lock file
// itself, which a new holder may open and lock again.
type fileLock struct {
	path string
	f    *os.File
}

func newFileLock(dir string) *fileLock {
	return &fileLock{path: dir + "\\" + lockFileName}
}

func (l *fileLock) tryAcquire() (bool, error) {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, err
	}
	var ol windows.Overlapped
	// LOCKFILE_FAIL_IMMEDIATELY gives non-blocking semantics; LOCKFILE_EXCLUSIVE_LOCK
	// makes the lock exclusive. Lock one byte region; the region itself is
	// irrelevant because the handle is never read or written by anyone.
	err = windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_FAIL_IMMEDIATELY|windows.LOCKFILE_EXCLUSIVE_LOCK,
		0, 1, 0, &ol)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
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

func (l *fileLock) release() error {
	if l.f == nil {
		return nil
	}
	var ol windows.Overlapped
	err := windows.UnlockFileEx(windows.Handle(l.f.Fd()), 0, 1, 0, &ol)
	if closeErr := l.f.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	l.f = nil
	return err
}

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

func (l *fileLock) AcquireNoWait() (bool, error) {
	return l.tryAcquire()
}
