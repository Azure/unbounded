// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package host

import (
	"context"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

const checkLocalDNSConntrackName = "localdns-conntrack"

type localDNSConntrackDeps struct {
	lookupPath func(string) (string, error)
	run        func(context.Context, string, []string, string) ([]byte, error)
}

func defaultLocalDNSConntrackDeps() localDNSConntrackDeps {
	return localDNSConntrackDeps{
		lookupPath: exec.LookPath,
		run: func(ctx context.Context, name string, args []string, stdin string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, name, args...)
			cmd.Stdin = strings.NewReader(stdin)

			return cmd.CombinedOutput()
		},
	}
}

func checkLocalDNSConntrack(log *slog.Logger, cfg config.AgentConfig, deps localDNSConntrackDeps) preflight.Checker {
	return simpleHostChecker{name: checkLocalDNSConntrackName, check: func(ctx context.Context) []preflight.Result {
		if cfg.LocalDNS == nil || !cfg.LocalDNS.Enabled {
			return preflight.ResultsOK(checkLocalDNSConntrackName, "host conntrack", "LocalDNS is disabled")
		}

		if _, err := deps.lookupPath("nft"); err != nil {
			if cfg.OfflineArtifactsConfigured() {
				return preflight.ResultsError(checkLocalDNSConntrackName, "host conntrack", "nftables is required in offline mode")
			}

			return preflight.ResultsWarning(checkLocalDNSConntrackName, "host conntrack", "nftables will be installed before LocalDNS setup")
		}

		script := `add table ip unbounded_localdns_preflight
add chain ip unbounded_localdns_preflight output { type filter hook output priority raw; policy accept; }
add rule ip unbounded_localdns_preflight output ip daddr 127.0.0.1 udp dport 53 notrack
`

		args := []string{"--check", "--file", "-"}
		if output, err := deps.run(ctx, "nft", args, script); err != nil {
			log.Debug("LocalDNS nftables capability check failed", "output", string(output), "error", err)

			return preflight.ResultsError(checkLocalDNSConntrackName, "host conntrack", "nftables raw-priority notrack support is required")
		}

		return preflight.ResultsOK(checkLocalDNSConntrackName, "host conntrack", "nftables raw-priority notrack support is available")
	}}
}
