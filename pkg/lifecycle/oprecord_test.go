// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package lifecycle

import (
	"errors"
	"testing"
	"time"

	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecordStoreMissingCorruptAndRoundTrip(t *testing.T) {
	t.Parallel()

	fs := afero.NewMemMapFs()
	store := newRecordStore(fs, t.TempDir())

	// A missing record is reported via the not-found sentinel.
	rec, err := store.load()
	require.ErrorIs(t, err, errNoOperationRecord)
	assert.Nil(t, rec)

	// A normal record round-trips.
	original := &OperationRecord{
		Action:              ActionStart,
		PID:                 1234,
		StartedAt:           time.Now().UTC().Truncate(time.Second),
		Corrupted:           false,
		NeedsCheck:          true,
		BootCompleted:       true,
		LastCompletedAction: ActionStop,
	}
	require.NoError(t, store.save(original))
	loaded, err := store.load()
	require.NoError(t, err)
	assert.Equal(t, original.Action, loaded.Action)
	assert.Equal(t, original.PID, loaded.PID)
	assert.True(t, loaded.NeedsCheck)
	assert.True(t, loaded.BootCompleted)
	assert.Equal(t, recordVersion, loaded.Version)

	// Clear returns to the not-found state.
	require.NoError(t, store.clear())
	_, err = store.load()
	assert.ErrorIs(t, err, errNoOperationRecord)

	// Clearing an already-missing file is not an error.
	require.NoError(t, store.clear())
}

func TestRecordStoreCorruptJSONIsResidual(t *testing.T) {
	t.Parallel()

	fs := afero.NewMemMapFs()
	dir := t.TempDir()
	store := newRecordStore(fs, dir)
	require.NoError(t, afero.WriteFile(fs, dir+"/"+recordFileName, []byte("{not valid json"), 0o600))

	rec, err := store.load()
	require.ErrorIs(t, err, errCorruptRecord)
	require.NotNil(t, rec)
	assert.True(t, rec.Corrupted, "a corrupt record is surfaced as a Corrupted marker")
}

func TestOperationRecordIsEmpty(t *testing.T) {
	t.Parallel()

	assert.True(t, (*OperationRecord)(nil).IsEmpty())
	assert.True(t, (&OperationRecord{}).IsEmpty())
	assert.False(t, (&OperationRecord{Action: ActionStart}).IsEmpty())
	assert.False(t, (&OperationRecord{Corrupted: true}).IsEmpty())
	assert.False(t, (&OperationRecord{NeedsCheck: true}).IsEmpty())
	assert.False(t, (&OperationRecord{BootCompleted: true}).IsEmpty())
	assert.False(t, (&OperationRecord{LastCompletedAction: ActionStop}).IsEmpty())
}

func TestTransitionalFor(t *testing.T) {
	t.Parallel()

	assert.Equal(t, Starting, transitionalFor(ActionInit))
	assert.Equal(t, Starting, transitionalFor(ActionStart))
	assert.Equal(t, Stopping, transitionalFor(ActionStop))
	assert.Equal(t, Stopping, transitionalFor(ActionRemove))
}

func TestSentinelErrorIdentity(t *testing.T) {
	t.Parallel()

	// The record sentinels must remain distinguishable from a generic failure.
	assert.True(t, errors.Is(errNoOperationRecord, errNoOperationRecord))
	assert.True(t, errors.Is(errCorruptRecord, errCorruptRecord))
	assert.False(t, errors.Is(errNoOperationRecord, errCorruptRecord))
}
