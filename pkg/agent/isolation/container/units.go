// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"fmt"
	"slices"
	"strings"
)

// UnitSpec is the input for the supervising unit.
type UnitSpec struct {
	Slot string
	// RuntimePath is the OCI runtime executable.
	RuntimePath string
	// BundleDir is the OCI bundle directory.
	BundleDir string
	// HelperPath is the agent binary that provides the node-lifecycle hooks.
	// Empty omits the hooks.
	HelperPath string
	// After lists host-global units to order after.
	After []string
	// Requires lists host-global units the container needs. Each is also
	// ordered before the container, as the nspawn machine's LocalDNS drop-in
	// does.
	Requires []string
}

// RenderUnit returns the systemd unit that supervises the container.
//
// PROVISIONAL: KillSignal SIGRTMIN+3 is the signal systemd as PID 1 treats as
// poweroff; runc forwards signals it receives to the container init. Neither
// has been exercised on Azure Container Linux.
func RenderUnit(s UnitSpec) []byte {
	id := ContainerID(s.Slot)

	after := []string{"network-online.target", "local-fs.target"}

	for _, u := range append(slices.Clone(s.After), s.Requires...) {
		if !slices.Contains(after, u) {
			after = append(after, u)
		}
	}

	requires := ""
	if len(s.Requires) > 0 {
		requires = "Requires=" + strings.Join(s.Requires, " ") + "\n"
	}

	var b strings.Builder

	fmt.Fprintf(&b, `[Unit]
Description=Unbounded node container (%[1]s)
Wants=network-online.target
%[5]sAfter=%[3]s
StartLimitIntervalSec=0

[Service]
Type=simple
Delegate=yes
KillMode=mixed
KillSignal=SIGRTMIN+3
TimeoutStartSec=300
TimeoutStopSec=120
Restart=always
RestartSec=1
RestartMaxDelaySec=60
RestartSteps=10
ExecStartPre=-%[2]s delete --force %[4]s
`, s.Slot, s.RuntimePath, strings.Join(after, " "), id, requires)

	if s.HelperPath != "" {
		fmt.Fprintf(&b, "ExecStartPre=%s node-lifecycle pre-start %s\n", s.HelperPath, s.Slot)
	}

	fmt.Fprintf(&b, "ExecStart=%s run --bundle %s %s\n", s.RuntimePath, s.BundleDir, id)

	if s.HelperPath != "" {
		fmt.Fprintf(&b, "ExecStartPost=%s node-lifecycle post-start %s\n", s.HelperPath, s.Slot)
	}

	fmt.Fprintf(&b, `ExecStopPost=-%s delete --force %s

[Install]
WantedBy=multi-user.target
`, s.RuntimePath, id)

	return []byte(b.String())
}
