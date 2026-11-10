// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package migrate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Azure/unbounded/internal/fsutil"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// JournalVersion is the schema version of a Journal.
const JournalVersion = 1

// StepReprovisioning is the step of a reprovision mode change whose reset and
// bootstrap are carried out by the handoff command; the daemon that started the
// migration is gone by then.
const StepReprovisioning Step = "reprovisioning"

// StepBootstrapping follows the reset of a reprovision migration: the old node
// is gone and bootstrap in the target mode is running or retrying.
const StepBootstrapping Step = "bootstrapping"

// StepBootstrapped is the last step of a reprovision migration: the node runs
// in the target mode and only the report in the Machine status is left.
const StepBootstrapped Step = "bootstrapped"

// StepRolledBack is terminal: the migration failed and the node runs in its
// original mode again. StepFailed is terminal too: the migration failed and the
// node could not be put back, so it needs attention. In both the journal's
// Message says why, until whoever reports the outcome removes it.
const (
	StepRolledBack Step = "rolled-back"
	StepFailed     Step = "failed"
)

// Journal is the small state file of a migration in progress. It is written
// atomically before each step (see InPlaceSpec.Record), so a daemon that
// restarts mid-migration can tell what may have happened and resume or roll
// back. It holds no credentials.
type Journal struct {
	Version  int            `json:"version"`
	Strategy Strategy       `json:"strategy"`
	From     isolation.Mode `json:"from"`
	To       isolation.Mode `json:"to"`
	// SourceSlot and TargetSlot are the blue/green slots of an in-place
	// migration. Reprovision leaves them empty.
	SourceSlot string    `json:"sourceSlot,omitempty"`
	TargetSlot string    `json:"targetSlot,omitempty"`
	Step       Step      `json:"step"`
	StartedAt  time.Time `json:"startedAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
	// Attempts counts the runs of a reprovision handoff that started, so the
	// handoff can give up after a bounded number of them.
	Attempts int `json:"attempts,omitempty"`
	// Message is the last failure the handoff command saw, for operators.
	Message string `json:"message,omitempty"`
}

// ErrNoJournal reports that no migration is recorded.
var ErrNoJournal = errors.New("no migration journal")

// JournalStore reads and writes a Journal at one path.
type JournalStore struct {
	Path string
	// Now overrides the clock. Nil means time.Now.
	Now func() time.Time
}

func (s JournalStore) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}

	return time.Now()
}

// Save writes j atomically and durably, so a crash leaves the previous journal
// or the new one and never a partial file.
func (s JournalStore) Save(j Journal) error {
	j.Version = JournalVersion
	j.UpdatedAt = s.now().UTC()

	if j.StartedAt.IsZero() {
		j.StartedAt = j.UpdatedAt
	}

	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal migration journal: %w", err)
	}

	// Durable, not just atomic: the step is a write-ahead record, so it must
	// survive a power loss before the step it names starts. The directory
	// entry is synced too.
	if err := fsutil.WriteFileDurable(s.Path, data, 0o600); err != nil {
		return fmt.Errorf("write migration journal %s: %w", s.Path, err)
	}

	return nil
}

// Load reads the journal. A missing file is ErrNoJournal. A file that does not
// parse is an error and not "no journal": the daemon must not carry on as if no
// migration were under way when one may be.
func (s JournalStore) Load() (Journal, error) {
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return Journal{}, ErrNoJournal
	}

	if err != nil {
		return Journal{}, fmt.Errorf("read migration journal %s: %w", s.Path, err)
	}

	var j Journal
	if err := json.Unmarshal(data, &j); err != nil {
		return Journal{}, fmt.Errorf("decode migration journal %s: %w", s.Path, err)
	}

	if j.Version != JournalVersion {
		return Journal{}, fmt.Errorf("migration journal %s has unsupported version %d", s.Path, j.Version)
	}

	return j, nil
}

// Remove deletes the journal and syncs its directory, so a removed journal
// does not come back after a crash. A missing file is not an error.
func (s JournalStore) Remove() error {
	if err := os.Remove(s.Path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return err
	}

	return fsutil.SyncDir(filepath.Dir(s.Path))
}

// Terminal reports whether nothing is left to do for the journal but report it.
func (j Journal) Terminal() bool {
	switch j.Step {
	case StepComplete, StepBootstrapped, StepRolledBack, StepFailed:
		return true
	default:
		return false
	}
}
