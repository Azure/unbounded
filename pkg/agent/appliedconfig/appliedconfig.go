// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package appliedconfig reads the small part of an applied agent config that
// read-only tooling needs: which slot is active and which isolation mode it
// runs in. It deliberately avoids the full config decode and validation so it
// keeps working for hosts written by older agents.
package appliedconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/migrate"
)

// ErrNotFound means no slot has an applied config.
var ErrNotFound = errors.New("no applied config found")

// Applied is the active slot and its recorded isolation mode.
type Applied struct {
	// Slot is the blue/green slot, for example kube1.
	Slot string
	// Mode is the recorded isolation mode. A config with no recorded mode is
	// nspawn.
	Mode isolation.Mode
}

// InPlaceJournalName is the file name of the in-place migration journal, which
// lives next to the applied configs. It tells which slot the node runs on while
// a migration has two recorded.
const InPlaceJournalName = "isolation-migration.json"

// SlotOrder is the order in which slots are searched for the applied config of
// the running node. It is kube1 then kube2, except while an in-place migration
// has both recorded: the journal at journalPath says which one the node runs
// on. A missing, unreadable or unrelated journal leaves the default order.
func SlotOrder(journalPath string) []string {
	order := []string{goalstates.NSpawnMachineKube1, goalstates.NSpawnMachineKube2}

	j, err := migrate.JournalStore{Path: journalPath}.Load()
	if err != nil || j.Strategy != migrate.InPlace || j.SourceSlot == "" || j.TargetSlot == "" {
		return order
	}

	switch j.Step {
	case migrate.StepAwaitingReady, migrate.StepReady, migrate.StepComplete:
		return []string{j.TargetSlot, j.SourceSlot}
	default:
		return []string{j.SourceSlot, j.TargetSlot}
	}
}

// ReadVerified reads the applied config at path and verifies its sha256
// sidecar (path plus ".sha256") when one exists. A missing file is returned as
// an error that matches os.ErrNotExist. checksumMissing reports that there was
// no sidecar, so the caller can say the integrity check was skipped.
func ReadVerified(path string) (data []byte, checksumMissing bool, err error) {
	data, err = os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}

	if err != nil {
		return nil, false, fmt.Errorf("read applied config %s: %w", path, err)
	}

	checksumPath := path + ".sha256"
	if err := goalstates.VerifyChecksum(data, checksumPath); err != nil {
		return nil, false, fmt.Errorf("verify applied config checksum for %s: %w", path, err)
	}

	_, statErr := os.Stat(checksumPath)

	return data, errors.Is(statErr, os.ErrNotExist), nil
}

// Read returns the first slot, in SlotOrder, that has an applied config in
// goalstates.AgentConfigDir.
func Read() (*Applied, error) {
	return ReadDir(goalstates.AgentConfigDir)
}

// ReadDir is Read for an explicit config directory. A present sha256 sidecar
// is verified; an absent one is accepted. An unrecognized mode is an error so
// an agent never treats a newer mode as nspawn.
func ReadDir(dir string) (*Applied, error) {
	for _, slot := range SlotOrder(filepath.Join(dir, InPlaceJournalName)) {
		path := filepath.Join(dir, slot+"-applied-config.json")

		data, _, err := ReadVerified(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}

		if err != nil {
			return nil, err
		}

		var raw struct {
			Isolation string `json:"Isolation"`
		}

		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("decode applied config %s: %w", path, err)
		}

		mode, err := isolation.ParseMode(raw.Isolation)
		if err != nil {
			return nil, fmt.Errorf("applied config %s: %w", path, err)
		}

		return &Applied{Slot: slot, Mode: mode}, nil
	}

	return nil, fmt.Errorf("%w in %s", ErrNotFound, dir)
}
