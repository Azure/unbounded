// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Azure/unbounded/pkg/agent/nodelifecycle"
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

// StartTimeout bounds the supervising unit's start: the pre-start hook, runc
// starting the container, and the post-start hook's wait for the node stack
// and its GPU step, plus a margin. It is derived from the post-start budget so
// the two cannot drift apart again.
const StartTimeout = nodelifecycle.PostStartBudget + time.Minute

// The start rate limit: a unit whose start keeps failing (for example a GPU
// hook that cannot succeed) stops being restarted after StartLimitBurst failed
// starts within StartLimitInterval, and stays failed until the agent starts it
// again, instead of restarting forever.
const (
	StartLimitInterval = time.Hour
	StartLimitBurst    = 10
)

// RenderUnit returns the systemd unit that supervises the container.
//
// Before runc starts, the unit makes the host's /var/lib/kubelet a shared mount
// (a bind of the directory on itself when nothing is mounted there), so the
// container's rshared bind of it joins its peer group and pod volume mounts the
// node stack makes there are visible on the host. It runs at every start, so it
// holds after a host reboot too. The slot's bpffs is mounted the same way.
//
// The unit conflicts with the other slot's unit, so systemd never runs both
// slots' containers at once, and the pre-start hook refuses to start while the
// other slot's container is still alive.
//
// PROVISIONAL: KillSignal SIGRTMIN+3 is the signal systemd as PID 1 treats as
// poweroff; runc forwards signals it receives to the container init.
// DelegateSubgroup= needs systemd 254. Neither has been exercised on Azure
// Container Linux.
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

	bpffs := BPFFSPath(s.Slot)

	var b strings.Builder

	fmt.Fprintf(&b, `[Unit]
Description=Unbounded node container (%[1]s)
Wants=network-online.target
%[5]sAfter=%[3]s
Conflicts=%[6]s
StartLimitIntervalSec=%[8]d
StartLimitBurst=%[9]d

[Service]
Type=simple
Slice=%[10]s
Delegate=yes
DelegateSubgroup=%[11]s
KillMode=mixed
KillSignal=SIGRTMIN+3
TimeoutStartSec=%[7]d
TimeoutStopSec=120
Restart=always
RestartSec=1
RestartMaxDelaySec=60
RestartSteps=10
ExecStartPre=-%[2]s delete --force %[4]s
ExecStartPre=/usr/bin/mkdir -p %[12]s
ExecStartPre=/bin/sh -c '/usr/bin/mountpoint -q %[12]s || /usr/bin/mount -t bpf bpf %[12]s'
ExecStartPre=/usr/bin/mkdir -p %[13]s
ExecStartPre=/bin/sh -c '/usr/bin/mountpoint -q %[13]s || /usr/bin/mount --bind %[13]s %[13]s'
ExecStartPre=/usr/bin/mount --make-rshared %[13]s
`, s.Slot, s.RuntimePath, strings.Join(after, " "), id, requires, UnitName(OtherSlot(s.Slot)),
		int(StartTimeout.Seconds()), int(StartLimitInterval.Seconds()), StartLimitBurst,
		unitSlice, supervisorSubgroup, bpffs, KubeletDir)

	if s.HelperPath != "" {
		fmt.Fprintf(&b, "ExecStartPre=%s node-lifecycle pre-start %s\n", s.HelperPath, s.Slot)
	}

	fmt.Fprintf(&b, "ExecStart=%s run --bundle %s %s\n", s.RuntimePath, s.BundleDir, id)

	if s.HelperPath != "" {
		fmt.Fprintf(&b, "ExecStartPost=%s node-lifecycle post-start %s\n", s.HelperPath, s.Slot)
	}

	// The container's processes are in the unit's cgroup, so systemd's stop
	// kills whatever survives SIGRTMIN+3. The delete only clears runc's state;
	// Backend.Stop verifies the container is gone.
	fmt.Fprintf(&b, `ExecStopPost=-%s delete --force %s

[Install]
WantedBy=multi-user.target
`, s.RuntimePath, id)

	return []byte(b.String())
}
