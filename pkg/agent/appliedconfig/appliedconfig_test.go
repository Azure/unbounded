// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package appliedconfig

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/migrate"
)

func write(t *testing.T, dir, slot, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, slot+"-applied-config.json"), []byte(body), 0o600))
}

func TestReadDir(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		files    map[string]string
		wantSlot string
		wantMode isolation.Mode
		wantErr  string
	}{
		{name: "missing mode is nspawn", files: map[string]string{"kube1": `{"MachineName":"x"}`}, wantSlot: "kube1", wantMode: isolation.ModeNSpawn},
		{name: "none", files: map[string]string{"kube2": `{"Isolation":"none"}`}, wantSlot: "kube2", wantMode: isolation.ModeNone},
		{name: "container", files: map[string]string{"kube1": `{"Isolation":"container"}`}, wantSlot: "kube1", wantMode: isolation.ModeContainer},
		{name: "kube1 wins", files: map[string]string{"kube1": `{}`, "kube2": `{"Isolation":"none"}`}, wantSlot: "kube1", wantMode: isolation.ModeNSpawn},
		{name: "unknown mode", files: map[string]string{"kube1": `{"Isolation":"microvm"}`}, wantErr: "unsupported isolation mode"},
		{name: "bad json", files: map[string]string{"kube1": `{`}, wantErr: "decode applied config"},
		{name: "none present", files: nil, wantErr: ErrNotFound.Error()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			for slot, body := range tt.files {
				write(t, dir, slot, body)
			}

			got, err := ReadDir(dir)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}

			require.NoError(t, err)
			require.Equal(t, &Applied{Slot: tt.wantSlot, Mode: tt.wantMode}, got)
		})
	}
}

func TestReadDirChecksum(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	body := `{"Isolation":"none"}`
	write(t, dir, "kube1", body)

	// A matching sidecar is accepted.
	sidecar := filepath.Join(dir, "kube1-applied-config.json.sha256")
	require.NoError(t, os.WriteFile(sidecar, []byte(goalstates.ComputeChecksum([]byte(body))+"\n"), 0o600))

	got, err := ReadDir(dir)
	require.NoError(t, err)
	require.Equal(t, isolation.ModeNone, got.Mode)

	// A mismatching sidecar is rejected.
	require.NoError(t, os.WriteFile(sidecar, []byte("deadbeef\n"), 0o600))

	_, err = ReadDir(dir)
	require.ErrorIs(t, err, goalstates.ErrChecksumMismatch)
}

func writeJournal(t *testing.T, dir string, j migrate.Journal) {
	t.Helper()
	require.NoError(t, migrate.JournalStore{Path: filepath.Join(dir, InPlaceJournalName)}.Save(j))
}

func inPlaceJournal(step migrate.Step) migrate.Journal {
	return migrate.Journal{
		Strategy: migrate.InPlace, From: isolation.ModeNSpawn, To: isolation.ModeContainer,
		SourceSlot: "kube1", TargetSlot: "kube2", Step: step,
	}
}

func TestSlotOrderFollowsTheJournal(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, InPlaceJournalName)

	require.Equal(t, []string{"kube1", "kube2"}, SlotOrder(path), "no journal")

	for step, want := range map[migrate.Step][]string{
		migrate.StepPreparing:      {"kube1", "kube2"},
		migrate.StepStartingTarget: {"kube1", "kube2"},
		migrate.StepAwaitingReady:  {"kube2", "kube1"},
		migrate.StepReady:          {"kube2", "kube1"},
		migrate.StepComplete:       {"kube2", "kube1"},
		migrate.StepRolledBack:     {"kube1", "kube2"},
	} {
		writeJournal(t, dir, inPlaceJournal(step))
		require.Equal(t, want, SlotOrder(path), string(step))
	}

	// Slot two as the source.
	j := inPlaceJournal(migrate.StepReady)
	j.SourceSlot, j.TargetSlot = "kube2", "kube1"
	writeJournal(t, dir, j)
	require.Equal(t, []string{"kube1", "kube2"}, SlotOrder(path))

	// A reprovision journal does not reorder anything.
	j = inPlaceJournal(migrate.StepReady)
	j.Strategy = migrate.Reprovision
	writeJournal(t, dir, j)
	require.Equal(t, []string{"kube1", "kube2"}, SlotOrder(path))

	// An unreadable journal leaves the default order.
	require.NoError(t, os.WriteFile(path, []byte("{"), 0o600))
	require.Equal(t, []string{"kube1", "kube2"}, SlotOrder(path))
}

func TestReadDirReadsTheRunningNodesSlotDuringAnInPlaceMigration(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	write(t, dir, "kube1", `{"Isolation":"nspawn"}`)
	write(t, dir, "kube2", `{"Isolation":"container"}`)

	got, err := ReadDir(dir)
	require.NoError(t, err)
	require.Equal(t, &Applied{Slot: "kube1", Mode: isolation.ModeNSpawn}, got, "no journal: kube1 first")

	// Before the target runs, the source is still the node.
	writeJournal(t, dir, inPlaceJournal(migrate.StepStartingTarget))

	got, err = ReadDir(dir)
	require.NoError(t, err)
	require.Equal(t, "kube1", got.Slot)

	// Once the target runs and is recorded, it is the node.
	writeJournal(t, dir, inPlaceJournal(migrate.StepAwaitingReady))

	got, err = ReadDir(dir)
	require.NoError(t, err)
	require.Equal(t, &Applied{Slot: "kube2", Mode: isolation.ModeContainer}, got)

	// After a rollback the source is the node again.
	writeJournal(t, dir, inPlaceJournal(migrate.StepRolledBack))

	got, err = ReadDir(dir)
	require.NoError(t, err)
	require.Equal(t, "kube1", got.Slot)
}

func TestReadVerified(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "x-applied-config.json")

	_, _, err := ReadVerified(path)
	require.ErrorIs(t, err, os.ErrNotExist)

	require.NoError(t, os.WriteFile(path, []byte(`{}`), 0o600))

	data, missing, err := ReadVerified(path)
	require.NoError(t, err)
	require.Equal(t, `{}`, string(data))
	require.True(t, missing, "no sidecar")

	require.NoError(t, os.WriteFile(path+".sha256", []byte(goalstates.ComputeChecksum([]byte(`{}`))), 0o600))

	_, missing, err = ReadVerified(path)
	require.NoError(t, err)
	require.False(t, missing)

	require.NoError(t, os.WriteFile(path+".sha256", []byte("deadbeef"), 0o600))

	_, _, err = ReadVerified(path)
	require.ErrorIs(t, err, goalstates.ErrChecksumMismatch)
}
