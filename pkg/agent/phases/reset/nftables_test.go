// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package reset

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"text/template"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

// stubSystemctl puts a systemctl on PATH that records its arguments and exits
// with the given status.
func stubSystemctl(t *testing.T, exit int) string {
	t.Helper()

	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "systemctl"),
		[]byte("#!/bin/sh\necho \"$@\" >> \""+calls+"\"\nexit "+strconv.Itoa(exit)+"\n"), 0o755))
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	return calls
}

func flushTask(t *testing.T) (*removeNFTablesFlush, string) {
	t.Helper()

	task := &removeNFTablesFlush{
		log:       slog.New(slog.DiscardHandler),
		unitDir:   t.TempDir(),
		clearPath: filepath.Join(t.TempDir(), "nftables-clear.nft"),
	}

	return task, filepath.Join(task.unitDir, goalstates.NFTablesFlushUnit)
}

// Left enabled, the flush erases the host's own firewall on every boot of the
// reset host, so reset disables it before it deletes it, in every mode.
func TestRemoveNFTablesFlushDisablesAndRemovesTheUnitAndRules(t *testing.T) {
	calls := stubSystemctl(t, 0)
	task, unitPath := flushTask(t)

	require.NoError(t, os.WriteFile(unitPath, []byte("[Install]\nWantedBy=multi-user.target\n"), 0o644))
	require.NoError(t, os.WriteFile(task.clearPath, []byte("flush ruleset\n"), 0o600))

	require.NoError(t, task.Do(t.Context()))
	require.NoFileExists(t, unitPath)
	require.NoFileExists(t, task.clearPath)

	recorded, err := os.ReadFile(calls)
	require.NoError(t, err)
	require.Equal(t, "disable "+goalstates.NFTablesFlushUnit+"\n", string(recorded),
		"removing the file without disabling leaves the WantedBy links behind")
}

// A host bootstrap never reached, or one already reset, has nothing to remove,
// and systemd is not asked about a unit it does not have.
func TestRemoveNFTablesFlushIsIdempotent(t *testing.T) {
	calls := stubSystemctl(t, 0)
	task, _ := flushTask(t)

	require.NoError(t, task.Do(t.Context()))
	require.NoFileExists(t, calls)

	// A rules file without its unit is still the agent's file.
	require.NoError(t, os.WriteFile(task.clearPath, []byte("flush ruleset\n"), 0o600))
	require.NoError(t, task.Do(t.Context()))
	require.NoFileExists(t, task.clearPath)
}

// A failed disable stops the reset with the unit still on disk, so a retry
// finds it and tries again instead of leaving its links enabled.
func TestRemoveNFTablesFlushKeepsTheUnitWhenDisableFails(t *testing.T) {
	stubSystemctl(t, 1)
	task, unitPath := flushTask(t)

	require.NoError(t, os.WriteFile(unitPath, []byte("[Unit]\n"), 0o644))

	require.Error(t, task.Do(t.Context()))
	require.FileExists(t, unitPath)
}

// The none and container modes install the node variant of the unit, wanted by
// multi-user.target, so it runs at every boot. Reset disables it, which removes
// its multi-user.target.wants link, and deletes the file. The variant is
// rendered from the template bootstrap uses (host.ConfigureNFTablesFor) with
// the container mode's units; the host package imports this one, so the
// template is read from disk rather than through it.
func TestRemoveNFTablesFlushRemovesTheContainerVariant(t *testing.T) {
	calls := stubSystemctl(t, 0)
	task, unitPath := flushTask(t)

	tmplText, err := os.ReadFile(filepath.Join("..", "host", "assets", "nftables-flush-node.service"))
	require.NoError(t, err)

	var unit bytes.Buffer

	require.NoError(t, template.Must(template.New("unit").Parse(string(tmplText))).Execute(&unit, map[string]string{
		"NFTablesClearPath": task.clearPath,
		"Before":            "unbounded-node-kube1.service unbounded-node-kube2.service",
	}))
	require.Contains(t, unit.String(), "WantedBy=multi-user.target")

	// As `systemctl enable` leaves it.
	wants := filepath.Join(task.unitDir, "multi-user.target.wants")
	require.NoError(t, os.MkdirAll(wants, 0o755))
	require.NoError(t, os.WriteFile(unitPath, unit.Bytes(), 0o644))
	require.NoError(t, os.Symlink(unitPath, filepath.Join(wants, goalstates.NFTablesFlushUnit)))
	require.NoError(t, os.WriteFile(task.clearPath, []byte("flush ruleset\n"), 0o600))

	require.NoError(t, task.Do(t.Context()))
	require.NoFileExists(t, unitPath)
	require.NoFileExists(t, task.clearPath)

	recorded, err := os.ReadFile(calls)
	require.NoError(t, err)
	require.Equal(t, "disable "+goalstates.NFTablesFlushUnit+"\n", string(recorded),
		"disable is what removes the multi-user.target.wants link")
}
