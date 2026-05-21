// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package orcadev implements the `orcadev` developer / debug tool.
//
// orcadev is a multi-purpose CLI for working with a running orca dev
// cluster. It subsumes the older `orcaseed` tool: every orcaseed
// capability (synthetic-blob generation, single-file upload, origin
// listing, bulk delete) is reachable here as a subcommand, plus a
// broader debugging surface:
//
//	upload     - seed the origin (file or N synthetic blobs); both
//	             azureblob and awss3 drivers supported
//	list       - enumerate origin objects
//	delete     - bulk delete origin objects
//	roundtrip  - upload data -> request through orca -> compare
//	             SHA-256 of source vs response; the headline
//	             correctness check
//	cache      - inspect / clear the chunk cachestore
//	  list       - enumerate cachestore objects (raw chunk paths)
//	  inspect    - given (bucket, key), show which chunks are
//	               present in the cachestore
//	  clear      - bulk delete chunks
//	bench      - parallel GET throughput / latency benchmark with
//	             JSON output and log-spaced latency histogram
//	scenario   - canned end-to-end scenarios (cold-warm, range-stress,
//	             multi-object, etag-change, empty-object, range-large)
//
// All subcommands accept --config <path> to point at an orca YAML
// (the same shape internal/orca/config consumes); per-flag overrides
// let the operator point at a different origin or cachestore without
// editing the YAML. The default `--orca-url http://localhost:8443`
// targets the dev harness's port-forwarded edge listener.
package orcadev

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

// Run is the entrypoint invoked by hack/cmd/orcadev/main.go. Wires
// the cobra command tree, parses flags, dispatches to the chosen
// subcommand. On error, prints to stderr and exits non-zero.
func Run() {
	g := defaultGlobalFlags()

	root := &cobra.Command{
		Use:           "orcadev",
		Short:         "Orca dev and debug tool (seed, roundtrip, cache inspect, bench, scenarios)",
		SilenceUsage:  true,
		SilenceErrors: false,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			return g.resolve(cmd.Context())
		},
	}

	// Connection-shape flags shared by every subcommand. --config is
	// the recommended path: it populates origin + cachestore +
	// orca-url defaults from the same YAML the orca daemon consumes,
	// so the tool always sees the same world the daemon does.
	root.PersistentFlags().StringVar(&g.configPath, "config", g.configPath,
		"Orca YAML config to populate origin + cachestore coordinates")
	root.PersistentFlags().StringVar(&g.orcaURL, "orca-url", g.orcaURL,
		"Edge URL of the orca instance (default http://localhost:8443 via kubectl port-forward)")

	// Origin overrides.
	root.PersistentFlags().StringVar(&g.originDriver, "origin-driver", g.originDriver,
		"Origin driver: azureblob or awss3 (overrides --config)")
	root.PersistentFlags().StringVar(&g.originID, "origin-id", g.originID,
		"Origin identifier baked into chunk paths (overrides --config; required for cache subcommands)")
	root.PersistentFlags().StringVar(&g.originBucket, "origin-bucket", g.originBucket,
		"Origin bucket/container name (overrides --config)")
	root.PersistentFlags().StringVar(&g.originEndpoint, "origin-endpoint", g.originEndpoint,
		"Origin endpoint URL (overrides --config; required for LocalStack / Azurite)")
	// awss3 origin auth.
	root.PersistentFlags().StringVar(&g.originRegion, "origin-region", g.originRegion,
		"awss3 origin region (overrides --config)")
	root.PersistentFlags().StringVar(&g.originAccessKey, "origin-access-key", g.originAccessKey,
		"awss3 origin access key (overrides --config)")
	root.PersistentFlags().StringVar(&g.originSecretKey, "origin-secret-key", g.originSecretKey,
		"awss3 origin secret key (overrides --config)")
	root.PersistentFlags().BoolVar(&g.originUsePathStyle, "origin-use-path-style", g.originUsePathStyle,
		"awss3 origin path-style addressing (overrides --config; true for LocalStack)")
	// azureblob origin auth.
	root.PersistentFlags().StringVar(&g.originAccount, "origin-account", g.originAccount,
		"azureblob origin account name (overrides --config)")
	root.PersistentFlags().StringVar(&g.originAccountKey, "origin-account-key", g.originAccountKey,
		"azureblob origin account shared key (overrides --config)")

	// Cachestore overrides.
	root.PersistentFlags().StringVar(&g.cachestoreEndpoint, "cachestore-endpoint", g.cachestoreEndpoint,
		"Cachestore S3 endpoint (overrides --config; default http://localhost:30200 via LocalStack NodePort)")
	root.PersistentFlags().StringVar(&g.cachestoreBucket, "cachestore-bucket", g.cachestoreBucket,
		"Cachestore bucket name (overrides --config)")
	root.PersistentFlags().StringVar(&g.cachestoreRegion, "cachestore-region", g.cachestoreRegion,
		"Cachestore region (overrides --config)")
	root.PersistentFlags().StringVar(&g.cachestoreAccessKey, "cachestore-access-key", g.cachestoreAccessKey,
		"Cachestore access key (overrides --config)")
	root.PersistentFlags().StringVar(&g.cachestoreSecretKey, "cachestore-secret-key", g.cachestoreSecretKey,
		"Cachestore secret key (overrides --config)")
	root.PersistentFlags().BoolVar(&g.cachestoreUsePathStyle, "cachestore-use-path-style", g.cachestoreUsePathStyle,
		"Cachestore path-style addressing (overrides --config; true for LocalStack)")

	// Misc.
	root.PersistentFlags().BoolVar(&g.ensureContainer, "ensure-container", g.ensureContainer,
		"Create the origin bucket/container if it does not already exist (azureblob only)")
	root.PersistentFlags().DurationVar(&g.timeout, "timeout", g.timeout,
		"Per-operation timeout for blocking calls")
	root.PersistentFlags().StringVar(&g.logLevel, "log-level", g.logLevel,
		"Log level: debug, info, warn, error")

	root.AddCommand(newUploadCmd(g))
	root.AddCommand(newListCmd(g))
	root.AddCommand(newDeleteCmd(g))
	root.AddCommand(newRoundtripCmd(g))
	root.AddCommand(newCacheCmd(g))
	root.AddCommand(newBenchCmd(g))
	root.AddCommand(newScenarioCmd(g))

	// Signal-aware context: ctrl-C cancels in-flight operations
	// instead of leaving partial uploads / benchmarks running on a
	// detached goroutine.
	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
