// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestServiceOverride_SelfHealingDirectives ensures the rendered systemd
// drop-in carries the directives that let systemd-nspawn@kube1 recover from
// systemd-machined being restarted out from under it (for example by
// needrestart during an unattended-upgrades run that touches libcap).
func TestServiceOverride_SelfHealingDirectives(t *testing.T) {
	t.Parallel()

	data := nspawnTemplateData{
		MachineName: "kube1",
	}

	var buf bytes.Buffer
	require.NoError(t, nspawnTemplates.ExecuteTemplate(&buf, "service-override.conf", data))

	out := buf.String()

	for _, want := range []string{
		"Restart=on-failure",
		"RestartSec=10s",
		"StartLimitIntervalSec=0",
		"ExecStartPre=-/usr/bin/machinectl terminate kube1",
		"Environment=SYSTEMD_NSPAWN_UNIFIED_HIERARCHY=1",
		"Environment=SYSTEMD_NSPAWN_API_VFS_WRITABLE=network",
	} {
		require.Contains(t, out, want, "missing directive in rendered drop-in")
	}

	// ExecStartPre must come before Environment so the cleanup runs before
	// the unit's main start logic.
	require.Less(t,
		strings.Index(out, "ExecStartPre=-/usr/bin/machinectl terminate kube1"),
		strings.Index(out, "Environment=SYSTEMD_NSPAWN_UNIFIED_HIERARCHY=1"),
		"ExecStartPre should appear before Environment lines",
	)
}

// TestServiceOverride_MachineNameSubstitution verifies the template uses the
// caller-provided machine name rather than a hard-coded "kube1".
func TestServiceOverride_MachineNameSubstitution(t *testing.T) {
	t.Parallel()

	data := nspawnTemplateData{MachineName: "kube7"}

	var buf bytes.Buffer
	require.NoError(t, nspawnTemplates.ExecuteTemplate(&buf, "service-override.conf", data))

	require.Contains(t, buf.String(), "machinectl terminate kube7")
	require.NotContains(t, buf.String(), "machinectl terminate kube1")
}
