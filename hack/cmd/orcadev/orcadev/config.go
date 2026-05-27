// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package orcadev

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/internal/orca/config"
)

// azuriteWellKnownDevKey is the documented well-known shared key for
// Azurite's default account ('devstoreaccount1'). Public Microsoft-
// documented constant, not a secret:
// https://learn.microsoft.com/azure/storage/common/storage-use-azurite
const azuriteWellKnownDevKey = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="

// globalFlags carries connection-shape flags shared by every
// subcommand. `--config` populates the origin + cachestore fields
// from an orca YAML; per-flag overrides win when both are set.
//
// Origin defaults target the dev harness's NodePort Azurite at
// localhost:30100; cachestore defaults target the planned LocalStack
// NodePort 30200 (mirrors the Azurite pattern). The orca edge URL
// default assumes `make -C hack/orca port-forward`.
type globalFlags struct {
	// Global / shared.
	configPath string
	orcaURL    string
	timeout    time.Duration
	logLevel   string

	// Origin fields. Driver selects between awss3 and azureblob.
	// originID is the value baked into chunk paths and is required
	// for any cache subcommand that scopes lookups by origin.
	originDriver     string
	originID         string
	originBucket     string
	originEndpoint   string
	originRegion     string
	originAccessKey  string
	originSecretKey  string
	originAccount    string
	originAccountKey string
	// originUsePathStyle: true for LocalStack-backed origins.
	originUsePathStyle bool

	// Cachestore fields. Always S3-shaped; the dev harness uses
	// LocalStack for both origin (awss3 mode) and cachestore but on
	// different buckets.
	cachestoreEndpoint     string
	cachestoreBucket       string
	cachestoreRegion       string
	cachestoreAccessKey    string
	cachestoreSecretKey    string
	cachestoreUsePathStyle bool

	// Misc.
	ensureContainer bool

	// autoPortForward, when true (the default), causes any
	// subcommand that talks to the orca edge listener to probe
	// --orca-url; if unreachable AND the URL is the dev default
	// (localhost:8443), orcadev spawns a managed kubectl
	// port-forward to svc/orca for the duration of the run. Set
	// false to suppress the auto-forward (e.g. in CI environments
	// where kubectl is not on PATH, or against a real ingress).
	autoPortForward bool
	// kubeContext is the kubectl context used by autoPortForward.
	// Default matches the dev harness's kind cluster name.
	kubeContext string
}

// defaultGlobalFlags returns the dev-harness-tuned defaults. These
// are deliberately weighted toward the most common dev scenario:
// orca running in the kind harness, port-forward in place for the
// edge listener, NodePorts exposing the cachestore and Azurite to
// the host. The default origin driver is azureblob (Azurite), which
// matches the dev harness's default ORIGIN_DRIVER value in
// hack/orca/.env.example. To target the awss3 (LocalStack) origin
// instead, pass --origin-driver=awss3 with the matching
// --origin-endpoint / --origin-bucket overrides.
func defaultGlobalFlags() *globalFlags {
	return &globalFlags{
		orcaURL:                "http://localhost:8443",
		timeout:                30 * time.Second,
		logLevel:               "info",
		originDriver:           "azureblob",
		originID:               "azureblob-azurite",
		originBucket:           "orca-test",
		originEndpoint:         "http://localhost:30100/devstoreaccount1/",
		originRegion:           "us-east-1",
		originAccessKey:        "test",
		originSecretKey:        "test",
		originUsePathStyle:     true,
		originAccount:          "devstoreaccount1",
		originAccountKey:       azuriteWellKnownDevKey,
		cachestoreEndpoint:     "http://localhost:30200",
		cachestoreBucket:       "orca-cache",
		cachestoreRegion:       "us-east-1",
		cachestoreAccessKey:    "test",
		cachestoreSecretKey:    "test",
		cachestoreUsePathStyle: true,
		autoPortForward:        true,
		kubeContext:            "kind-orca-dev",
	}
}

// resolve loads --config (if set) and overlays per-flag overrides on
// top, producing the effective configuration. It is wired as a
// PersistentPreRunE so every subcommand sees fully-resolved fields
// without re-running this logic.
//
// Precedence: per-flag override > YAML value > built-in default. A
// flag is considered "overridden" when the operator actually passed
// it on the command line, detected via cobra's Flag.Changed bit.
// This is more correct than comparing against the default value:
// passing --origin-bucket=orca-test (which happens to be the
// default) now wins over a YAML value pointing at a different
// bucket.
func (g *globalFlags) resolve(cmd *cobra.Command) error {
	if g.configPath == "" {
		return nil
	}

	cfg, err := config.Load(g.configPath)
	if err != nil {
		return fmt.Errorf("load --config: %w", err)
	}

	flags := cmd.Flags()
	notSet := func(name string) bool { return !flags.Changed(name) }

	// Origin: pull from YAML unless the user supplied the flag.
	if notSet("origin-driver") && cfg.Origin.Driver != "" {
		g.originDriver = cfg.Origin.Driver
	}

	if notSet("origin-id") && cfg.Origin.ID != "" {
		g.originID = cfg.Origin.ID
	}

	switch g.originDriver {
	case "awss3":
		if notSet("origin-bucket") && cfg.Origin.AWSS3.Bucket != "" {
			g.originBucket = cfg.Origin.AWSS3.Bucket
		}

		if notSet("origin-endpoint") && cfg.Origin.AWSS3.Endpoint != "" {
			g.originEndpoint = cfg.Origin.AWSS3.Endpoint
		}

		if notSet("origin-region") && cfg.Origin.AWSS3.Region != "" {
			g.originRegion = cfg.Origin.AWSS3.Region
		}

		if notSet("origin-access-key") && cfg.Origin.AWSS3.AccessKey != "" {
			g.originAccessKey = cfg.Origin.AWSS3.AccessKey
		}

		if notSet("origin-secret-key") && cfg.Origin.AWSS3.SecretKey != "" {
			g.originSecretKey = cfg.Origin.AWSS3.SecretKey
		}
		// Path-style is a bool; the YAML can flip it on. We treat
		// the default as truthy (LocalStack assumption) and let the
		// YAML override unless the operator passed the flag.
		if notSet("origin-use-path-style") {
			g.originUsePathStyle = cfg.Origin.AWSS3.UsePathStyle
		}
	case "azureblob":
		if notSet("origin-bucket") && cfg.Origin.Azureblob.Container != "" {
			g.originBucket = cfg.Origin.Azureblob.Container
		}

		if notSet("origin-endpoint") && cfg.Origin.Azureblob.Endpoint != "" {
			g.originEndpoint = cfg.Origin.Azureblob.Endpoint
		}

		if notSet("origin-account") && cfg.Origin.Azureblob.Account != "" {
			g.originAccount = cfg.Origin.Azureblob.Account
		}

		if notSet("origin-account-key") && cfg.Origin.Azureblob.AccountKey != "" {
			g.originAccountKey = cfg.Origin.Azureblob.AccountKey
		}
	}

	// Cachestore.
	if notSet("cachestore-bucket") && cfg.Cachestore.S3.Bucket != "" {
		g.cachestoreBucket = cfg.Cachestore.S3.Bucket
	}

	if notSet("cachestore-endpoint") && cfg.Cachestore.S3.Endpoint != "" {
		// YAML endpoint is the in-cluster one (svc.cluster.local);
		// the dev tool runs on the host so we cannot reach it
		// directly. Only adopt the YAML value when it is NOT an
		// in-cluster Service DNS - operators who want the YAML value
		// to win for any reason can pass --cachestore-endpoint
		// explicitly.
		if !strings.Contains(cfg.Cachestore.S3.Endpoint, ".svc.cluster.local") {
			g.cachestoreEndpoint = cfg.Cachestore.S3.Endpoint
		}
	}

	if notSet("cachestore-region") && cfg.Cachestore.S3.Region != "" {
		g.cachestoreRegion = cfg.Cachestore.S3.Region
	}

	if notSet("cachestore-access-key") && cfg.Cachestore.S3.AccessKey != "" {
		g.cachestoreAccessKey = cfg.Cachestore.S3.AccessKey
	}

	if notSet("cachestore-secret-key") && cfg.Cachestore.S3.SecretKey != "" {
		g.cachestoreSecretKey = cfg.Cachestore.S3.SecretKey
	}

	if notSet("cachestore-use-path-style") {
		g.cachestoreUsePathStyle = cfg.Cachestore.S3.UsePathStyle
	}

	return nil
}
