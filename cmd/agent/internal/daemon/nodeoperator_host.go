// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	"github.com/Azure/unbounded/internal/provision"
	daemon "github.com/Azure/unbounded/pkg/agent/daemon"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation/migrate"
)

// hostOperator implements hostOperations on the real host.
type hostOperator struct {
	// slotRunning reports whether the node stack an applied config records is
	// running. It is only asked when two applied configs exist and no
	// migration journal says which one is active. Nil means it cannot be
	// asked, and two records then fail closed.
	slotRunning func(ctx context.Context, log *slog.Logger, active *ActiveMachine) (bool, error)
	// configDir is the agent configuration directory holding the applied
	// configs and the in-place journal. Empty is goalstates.AgentConfigDir.
	configDir string
}

var _ hostOperations = hostOperator{}

// newHostOperator returns the host operator that asks each mode's backend
// whether a recorded node stack runs.
func newHostOperator(backends ModeBackends) hostOperator {
	return hostOperator{slotRunning: backends.running}
}

// FindActiveMachine returns the running node, from its applied config, for
// tools outside the daemon (exec, logs) that operate it the way the daemon
// does. It never asks a backend whether a node runs, so a host with two
// applied configs and no journal is an error.
func FindActiveMachine(log *slog.Logger) (*ActiveMachine, error) {
	return hostOperator{}.FindActiveMachine(log)
}

func (h hostOperator) FindActiveMachine(log *slog.Logger) (*ActiveMachine, error) {
	dir := h.configDir
	if dir == "" {
		dir = goalstates.AgentConfigDir
	}

	return activeMachineLookup{
		pathOf:      func(name string) string { return filepath.Join(dir, name+"-applied-config.json") },
		journalPath: inPlaceJournalPathIn(dir),
		running:     h.slotRunning,
	}.find(log)
}

// slotRunningTimeout bounds the question to a backend whether a recorded node
// stack runs.
const slotRunningTimeout = 30 * time.Second

// activeMachineLookup finds the applied config of the node that runs.
type activeMachineLookup struct {
	// pathOf maps an applied config location name to its file.
	pathOf func(name string) string
	// journalPath is the in-place migration journal. Empty means none.
	journalPath string
	// running is hostOperator.slotRunning.
	running func(ctx context.Context, log *slog.Logger, active *ActiveMachine) (bool, error)
}

// findActiveMachineIn searches every applied config location, with pathOf
// mapping a location name to its file, and asks nothing else.
func findActiveMachineIn(log *slog.Logger, pathOf func(name string) string) (*ActiveMachine, error) {
	return activeMachineLookup{pathOf: pathOf}.find(log)
}

// find reads every applied config location. One record is the active node.
// Two can exist for a while: a repave records the new slot before it removes
// the old one, and an in-place migration keeps both until it is done. Picking
// one by a fixed order would then operate (and could destroy) the node that
// runs as if it were the stale one, so find decides by what it can tell, and
// fails closed when it cannot tell:
//
//  1. The in-place migration journal names the slot that is active at its
//     step.
//  2. Otherwise, when exactly one of the recorded node stacks is running, that
//     one is active.
//  3. Otherwise the host needs attention, and find returns an error.
//
// Any record it cannot read or verify is an error too: it may be the active
// one.
func (l activeMachineLookup) find(log *slog.Logger) (*ActiveMachine, error) {
	var found []*ActiveMachine

	for _, loc := range goalstates.AppliedConfigLocations() {
		active, err := readAppliedLocation(log, loc, l.pathOf(loc.Name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}

		if err != nil {
			return nil, err
		}

		found = append(found, active)
	}

	switch len(found) {
	case 0:
		return nil, fmt.Errorf("no applied config found in %s", goalstates.AgentConfigDir)
	case 1:
		return found[0], nil
	}

	if active := l.fromJournal(log, found); active != nil {
		return active, nil
	}

	return l.byRunningStack(log, found)
}

// fromJournal returns the record an in-place migration journal names as active
// at its step, or nil when no readable journal names one of found.
func (l activeMachineLookup) fromJournal(log *slog.Logger, found []*ActiveMachine) *ActiveMachine {
	if l.journalPath == "" {
		return nil
	}

	j, err := migrate.JournalStore{Path: l.journalPath}.Load()
	if err != nil {
		if !errors.Is(err, migrate.ErrNoJournal) {
			log.Warn("the isolation change journal is unreadable; it cannot say which slot is active", "error", err)
		}

		return nil
	}

	if j.Strategy != migrate.InPlace || j.SourceSlot == "" || j.TargetSlot == "" {
		return nil
	}

	mode, slot := j.From, j.SourceSlot

	switch j.Step {
	case migrate.StepAwaitingReady, migrate.StepReady, migrate.StepComplete:
		mode, slot = j.To, j.TargetSlot
	}

	for _, active := range found {
		if active.Name == slot && activeModeOf(active) == mode {
			return active
		}
	}

	return nil
}

// byRunningStack returns the only record of found whose node stack runs.
func (l activeMachineLookup) byRunningStack(log *slog.Logger, found []*ActiveMachine) (*ActiveMachine, error) {
	names := make([]string, 0, len(found))
	for _, active := range found {
		names = append(names, fmt.Sprintf("%s (%s)", active.Name, activeModeOf(active)))
	}

	ambiguous := fmt.Errorf("applied configs for %s exist in %s and nothing says which one is active; "+
		"the node needs attention: remove the applied config of the node stack that is not running",
		strings.Join(names, ", "), goalstates.AgentConfigDir)

	if l.running == nil {
		return nil, ambiguous
	}

	ctx, cancel := context.WithTimeout(context.Background(), slotRunningTimeout)
	defer cancel()

	var running []*ActiveMachine

	for _, active := range found {
		up, err := l.running(ctx, log, active)
		if err != nil {
			return nil, errors.Join(ambiguous, fmt.Errorf("ask whether %s runs: %w", active.Name, err))
		}

		if up {
			running = append(running, active)
		}
	}

	if len(running) != 1 {
		return nil, fmt.Errorf("%w (%d of them running)", ambiguous, len(running))
	}

	log.Warn("two applied configs exist; using the one whose node stack is running",
		"active_machine", running[0].Name, "candidates", strings.Join(names, ", "))

	return running[0], nil
}

// readAppliedLocation reads and verifies the applied config at path, stored at
// loc. A missing file is an error that matches os.ErrNotExist. Verify the
// SHA-256 sidecar before trusting the applied config. A missing sidecar is
// logged as a warning and not treated as an error.
func readAppliedLocation(log *slog.Logger, loc goalstates.AppliedConfigLocation, path string) (*ActiveMachine, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	if err != nil {
		return nil, fmt.Errorf("read applied config %s: %w", path, err)
	}

	// Verify the sidecar checksum before trusting the config data.
	checksumPath := path + ".sha256"
	if err := goalstates.VerifyChecksum(data, checksumPath); err != nil {
		return nil, fmt.Errorf("verify applied config checksum for %s: %w", loc.Name, err)
	}

	// If the sidecar file is missing, log a warning so operators
	// know the integrity check was skipped.
	if _, statErr := os.Stat(checksumPath); errors.Is(statErr, os.ErrNotExist) {
		log.Warn("no checksum sidecar found, skipping integrity check",
			"config_path", path,
			"checksum_path", checksumPath,
		)
	}

	active, err := decodeActiveMachine(log, loc.Slot, path, data)
	if err != nil {
		return nil, err
	}

	// The file name says which mode wrote it. A config that records
	// another mode was not written by an agent, and neither mode's
	// operations can be trusted with it.
	if active.Mode != loc.Mode {
		return nil, fmt.Errorf("applied config %s records isolation mode %q but is stored as the %q config", path, active.Mode, loc.Mode)
	}

	return active, nil
}

func (hostOperator) ResetAgentResources(ctx context.Context, log *slog.Logger) error {
	// The MachineOperation holds installation ownership through daemon stop.
	return resetUnderLock(ctx, log, installstate.DefaultStore(), resetResources(log))
}

func (hostOperator) StopDaemon(ctx context.Context, log *slog.Logger) error {
	return StopDaemon(log).Do(ctx)
}

func (hostOperator) BootID() (string, error) {
	return readBootID()
}

func (hostOperator) RebootHost(ctx context.Context, log *slog.Logger) error {
	return daemon.RebootHost(ctx, log)
}

func (hostOperator) StageAgentUpgrade(ctx context.Context, log *slog.Logger, request agentUpgradeRequest) error {
	return upgradeDaemonBinary(ctx, log, request)
}

func (hostOperator) ReconcileHostRoot(ctx context.Context, log *slog.Logger, active *ActiveMachine, lifecycle lifecycleMigrator) (bool, error) {
	return reconcileHostRoot(ctx, log, lifecycle, active)
}

// decodeActiveMachine builds the active machine from the applied config data
// of slot name, read from path, with its isolation mode.
func decodeActiveMachine(log *slog.Logger, name, path string, data []byte) (*ActiveMachine, error) {
	var cfg provision.AgentConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("decode applied config %s: %w", path, err)
	}

	// MachineName must be resolved before the node name because
	// BackfillNodeName falls back to MachineName. The applied config is
	// normally persisted with a name already set, so this is usually a
	// no-op; it covers configs written by older agents or hand-edited.
	source, err := provision.ResolveMachineName(&cfg)
	if err != nil {
		return nil, fmt.Errorf("resolve applied config machine name %s: %w", path, err)
	}

	if source != "config" {
		log.Info("resolved unbounded MachineName", "name", cfg.MachineName, "source", source)
	}

	if err := cfg.BackfillNodeName(); err != nil {
		return nil, fmt.Errorf("backfill applied config node name %s: %w", path, err)
	}

	// The applied config records the mode the machine was built in. A
	// config without one is nspawn. A mode this agent cannot parse fails
	// closed: operating it as nspawn could destroy a node stack the agent
	// does not understand.
	mode, err := cfg.IsolationMode()
	if err != nil {
		return nil, fmt.Errorf("applied config %s: %w", path, err)
	}

	return &ActiveMachine{Name: name, Config: &cfg, Mode: mode}, nil
}
