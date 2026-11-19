// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package lifecycle

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/afero"
)

const (
	// recordFileName is the JSON file holding the in-progress operation and the
	// durable annotations. It lives next to the per-VM lock file.
	recordFileName = "operation.json"
	// recordVersion is bumped whenever the on-disk schema changes incompatibly.
	recordVersion = 1
	// recordTmpSuffix is used for the write-then-rename atomic update.
	recordTmpSuffix = ".tmp"
	timeStampLayout = time.RFC3339
)

// Duration re-exports time.Duration so the public configuration surface of this
// package does not force callers to import time for simple construction.
type Duration = time.Duration

// OperationRecord is the only state the manager persists itself.
//
// The authoritative VM state always comes from a driver query; this file only
// records the operation currently in progress (and its holder) plus a few
// annotations the driver cannot express. If the holder process is gone, the
// record is treated as residual state that a new process may take over after
// re-querying the driver — it is never trusted on its own.
type OperationRecord struct {
	// Version is the schema version of the record.
	Version int `json:"version"`
	// Action is the mutating action that was running when the record was written.
	Action Action `json:"action"`
	// PID is the operating-system process id of the holder.
	PID int `json:"pid"`
	// StartedAt is when the holder acquired the lock.
	StartedAt time.Time `json:"started_at"`
	// UpdatedAt is when the record was last refreshed.
	UpdatedAt time.Time `json:"updated_at"`
	// Corrupted is set when an operation failed or was interrupted after partial
	// side effects. While true, the derived state is Corrupted and only Remove is
	// accepted.
	Corrupted bool `json:"corrupted"`
	// NeedsCheck is set after a forced stop. The VM is reported as stopped but
	// annotated as needing inspection; the flag clears on the next successful
	// start or on remove.
	NeedsCheck bool `json:"needs_check"`
	// BootCompleted is true once a start has ever completed successfully. It is
	// what distinguishes InitializedStopped (created by init, never started on its
	// own) from Stopped when the driver reports a halted instance.
	BootCompleted bool `json:"boot_completed"`
	// LastCompletedAction records the most recent action that finished without
	// interruption; it aids recovery decisions and diagnostics.
	LastCompletedAction Action `json:"last_completed_action,omitempty"`
}

// recordStore persists OperationRecord as JSON with atomic rename semantics.
type recordStore struct {
	fs   afero.Fs
	path string
}

func newRecordStore(fs afero.Fs, dir string) *recordStore {
	return &recordStore{fs: fs, path: dir + "/" + recordFileName}
}

// Sentinel errors used to distinguish a missing record and an unreadable
// record from genuine I/O failures. Callers map these onto state semantics
// instead of relying on a nil record with a nil error.
var (
	errNoOperationRecord = errors.New("no operation record exists")
	errCorruptRecord     = errors.New("operation record is corrupt")
)

// load reads the record. A missing file yields errNoOperationRecord; an
// unreadable JSON document is returned together with a Corrupted marker and
// errCorruptRecord, so a broken record can never wedge the VM permanently.
func (s *recordStore) load() (*OperationRecord, error) {
	raw, err := afero.ReadFile(s.fs, s.path)
	if errors.Is(err, afero.ErrFileNotFound) {
		return nil, errNoOperationRecord
	}
	if err != nil {
		return nil, fmt.Errorf("reading operation record: %w", err)
	}
	var rec OperationRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return &OperationRecord{Version: recordVersion, Corrupted: true},
			fmt.Errorf("%w: %w", errCorruptRecord, err)
	}
	return &rec, nil
}

// save writes the record atomically (temp file + rename), so a crash can never
// leave a half-written JSON document behind.
func (s *recordStore) save(rec *OperationRecord) error {
	rec.Version = recordVersion
	rec.UpdatedAt = time.Now().UTC()
	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding operation record: %w", err)
	}
	tmp := s.path + recordTmpSuffix
	if err := afero.WriteFile(s.fs, tmp, raw, 0o600); err != nil {
		return fmt.Errorf("writing operation record: %w", err)
	}
	if err := s.fs.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("committing operation record: %w", err)
	}
	return nil
}

// clear removes the record. A missing file is treated as success.
func (s *recordStore) clear() error {
	if err := s.fs.Remove(s.path); err != nil && !errors.Is(err, afero.ErrFileNotFound) {
		return fmt.Errorf("removing operation record: %w", err)
	}
	return nil
}
