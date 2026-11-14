// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package transaction

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/internal/fsutil"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
)

const bootGuardScript = `#!/bin/sh
set -eu
# A stop does not remove systemd's boot enablement. Keep kubelet behind the
# same preparation boundary after a host restart as during a live transition.
[ ! -e "$1" ] || exit 1
[ ! -e "$2" ] || [ -e "$3" ]
`

// EnsureBootGuard persists the boot-time check before an upgrade can publish
// preparing intent. An ordinary stop keeps boot enablement; the condition is
// evaluated by systemd on every kubelet start, including after power loss.
func (n *Node) EnsureBootGuard(ctx context.Context) error {
	if err := n.validate(); err != nil {
		return err
	}

	base := filepath.Dir(n.p.Layout.CurrentLink())
	helper := filepath.Join(base, ".check-startup")
	stateDir := n.stateDir()

	if err := none.MkdirTraversable(base); err != nil {
		return fmt.Errorf("create node base: %w", err)
	}

	if err := fsutil.WriteFileDurable(helper, []byte(bootGuardScript), 0o755); err != nil {
		return fmt.Errorf("write node boot guard: %w", err)
	}
	// Ensure executable mode even under a restrictive umask, then persist it.
	if err := os.Chmod(helper, 0o755); err != nil { //nolint:gosec // owned helper executed by systemd
		return err
	}

	if err := syncPreparedPath(helper); err != nil {
		return err
	}

	args := []string{helper, filepath.Join(stateDir, "none-reset"), n.journalPath(), n.startupPath()}
	for i, arg := range args {
		args[i] = systemdArgument(arg)
	}

	path := filepath.Join(none.DropInDir(unitDir(n.p.SystemdDir), none.UnitKubelet), "01-startup-guard.conf")
	if err := fsutil.WriteFileDurable(path, []byte("[Service]\nExecCondition="+strings.Join(args, " ")+"\n"), 0o644); err != nil {
		return fmt.Errorf("write node boot condition: %w", err)
	}
	// Use the configured executor directly: this guard must be installed before
	// goal resolution or a running source's configuration is changed.
	if n.p.Run != nil {
		_, err := n.p.Run(ctx, n.p.Log, "systemctl", "daemon-reload")
		return err
	}

	_, err := executil.OutputCmd(ctx, n.p.Log, "systemctl", "daemon-reload")

	return err
}

func systemdArgument(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, `$`, `$$`, "\n", `\n`, "\r", `\r`, "\t", `\t`).Replace(value) + `"`
}
