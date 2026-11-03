// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package none

import (
	"fmt"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

// UnitLocalDNS is the agent-owned CoreDNS unit. LocalDNS needs no machine to
// run in, so on this backend CoreDNS is a host unit. The network unit that
// creates the dummy interface and NOTRACK rules is shared with the nspawn mode
// and is not defined here.
const UnitLocalDNS = "unbounded-localdns.service"

// UnitLocalDNSNetwork is the host network unit shared by every mode. It is
// written by the node-start phases, never by this backend.
const UnitLocalDNSNetwork = goalstates.LocalDNSNetworkUnit

// RenderLocalDNS returns the CoreDNS supervisor unit. supervisor is the path of
// the supervisor script in the version directory.
func RenderLocalDNS(supervisor string) []byte {
	return fmt.Appendf(nil, `[Unit]
Description=Unbounded LocalDNS (unbounded agent)
Wants=%[2]s
After=network.target %[2]s
Before=%[3]s %[4]s

[Service]
Type=notify
NotifyAccess=all
ExecStart=%[1]s
WatchdogSec=60
Restart=on-failure
RestartSec=2
KillMode=mixed
TimeoutStopSec=30
Slice=localdns.slice
DynamicUser=yes
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=strict
ProtectHome=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes

[Install]
WantedBy=multi-user.target
`, supervisor, UnitLocalDNSNetwork, UnitContainerd, UnitKubelet)
}

// RenderPathDropIn returns the drop-in that puts the version's bin directory
// first on PATH, so containerd finds containerd-shim-runc-v2 and runc and
// kubelet finds the tools it shells out to without touching the host's PATH.
func RenderPathDropIn(binDir string) []byte {
	return fmt.Appendf(nil, `# Managed by unbounded-agent. Do not edit directly.
[Service]
Environment="PATH=%s:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
`, binDir)
}

// RenderLocalDNSDropIn returns the drop-in that orders a node unit after
// LocalDNS and makes it depend on it, as the nspawn machine's units do.
func RenderLocalDNSDropIn() []byte {
	return fmt.Appendf(nil, `# Managed by unbounded-agent. Do not edit directly.
[Unit]
Requires=%[1]s
After=%[1]s %[2]s
`, UnitLocalDNS, UnitLocalDNSNetwork)
}
