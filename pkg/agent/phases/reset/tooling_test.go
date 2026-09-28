// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package reset

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// hostMissingOurPackages builds a PATH resembling a host where bootstrap died
// inside host preparation: systemd and iproute2 are present because the distro
// ships them, but machinectl and nft are not, because installing
// systemd-container and nftables is the step that did not finish.
//
// Removing everything from PATH would be a easier fixture and a wrong one.
// systemctl always exists on a systemd host, so a test that also hides it
// proves nothing about the hosts this tolerance is for.
func hostMissingOurPackages(t *testing.T) {
	t.Helper()

	dir := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(dir, "systemctl"), []byte(
		"#!/bin/sh\n"+
			"case \"$1\" in show) echo \"\" ;; esac\n"+
			"exit 0\n"), 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(dir, "ip"), []byte(
		"#!/bin/sh\n"+
			"for a in \"$@\"; do if test \"$a\" = -j; then echo \"[]\"; exit 0; fi; done\n"+
			"exit 0\n"), 0o755))

	t.Setenv("PATH", dir)
}

// TestResetCompletesWithoutOurPackages is the case this tolerance exists for,
// and it runs through the reset tasks rather than the helper they call.
//
// A bootstrap that died inside host preparation leaves an installation record
// on a host without machinectl or nft. Reset has to finish there. If it cannot,
// the record stays forever and every later start is refused against an
// installation that nothing can clear.
func TestResetCompletesWithoutOurPackages(t *testing.T) {
	hostMissingOurPackages(t)

	log := slog.New(slog.DiscardHandler)

	require.NoError(t, (&stopMachine{log: log, machineName: "kube1"}).Do(t.Context()),
		"a machine cannot be running if machinectl was never installed")

	require.NoError(t, (&cleanupLocalDNSRules{log: log}).Do(t.Context()),
		"a LocalDNS ruleset cannot exist if nft was never installed")

	require.NoError(t, (&cleanupRoutes{log: log}).Do(t.Context()),
		"policy routing rules cannot exist without our packages")
}

// TestAdmissionStillFailsClosedWithoutOurPackages is the other half, and the
// reason the tolerance is a separate function rather than a change to
// RegisteredMachine.
//
// Bootstrap asks the same question for the opposite reason: it must prove the
// host is clean before building on it. A host it cannot inspect is not a host
// it can prove anything about, so an absent tool stays an error here.
func TestAdmissionStillFailsClosedWithoutOurPackages(t *testing.T) {
	hostMissingOurPackages(t)

	log := slog.New(slog.DiscardHandler)

	_, err := RegisteredMachine(t.Context(), log, "kube1")
	require.Error(t, err, "bootstrap must not read an uninspectable host as a clean one")

	_, err = FirstRegisteredMachine(t.Context(), log)
	require.Error(t, err, "bootstrap must not read an uninspectable host as a clean one")
}

// TestCleanupStillFailsOnRealInspectionErrors keeps the tolerance narrow. A
// tool that is present and fails is a genuine problem: it may be reporting a
// machine that is actually there, and reset must not delete around it.
func TestCleanupStillFailsOnRealInspectionErrors(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "machinectl"),
		[]byte("#!/bin/sh\necho denied >&2\nexit 1\n"), 0o755))
	t.Setenv("PATH", dir)

	_, err := registeredMachineForCleanup(t.Context(), slog.New(slog.DiscardHandler), "kube1")
	require.Error(t, err, "a tool that ran and failed is not proof of absence")
}
