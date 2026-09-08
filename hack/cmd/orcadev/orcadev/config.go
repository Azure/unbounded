// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package orcadev

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/internal/orca/config"
	"github.com/Azure/unbounded/internal/unbounded"
)

// azuriteWellKnownDevKey is the documented well-known shared key for
// Azurite's default account ('devstoreaccount1'). Public Microsoft-
// documented constant, not a secret:
// https://learn.microsoft.com/azure/storage/common/storage-use-azurite
const azuriteWellKnownDevKey = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="

// PresetDev is the only currently-supported value for --preset. It
// selects the dev-install defaults: azureblob origin pointing at the
// in-cluster Azurite, S3 cachestore pointing at the in-cluster
// Garage, well-known credentials for both, and auto-port-forward
// to every relevant Service. See preset.go for the full definition.
const PresetDev = "dev"

// globalFlags carries connection-shape flags shared by every
// subcommand. `--config` populates the origin + cachestore fields
// from an orca YAML; per-flag overrides win when both are set.
//
// The defaults baked into defaultGlobalFlags() reflect --preset=dev:
// origin = Azurite via localhost:30100, cachestore = Garage via
// localhost:30200, orca edge = localhost:8443. orcadev auto-opens
// kubectl port-forwards for any of these services that isn't already
// bound on localhost, so the same defaults work on kind (where the
// NodePorts mean the probes succeed without spawning kubectl) and on
// any other cluster (AKS, EKS, k3d, ...) reachable via kubectl.
type globalFlags struct {
	// Global / shared.
	configPath string
	orcaURL    string
	timeout    time.Duration
	logLevel   string

	// preset is the named defaults bundle. Only "dev" is currently
	// supported. Validated after PersistentPreRunE resolves
	// --config; the flag-precedence is unchanged (per-flag overrides
	// still win over preset defaults, which still win over YAML
	// values pulled by --config).
	preset string

	// namespace is the Kubernetes namespace where Orca and its
	// emulator dependencies are deployed. Used by the auto
	// port-forward to address `svc/<service>` correctly.
	namespace string

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
	// originUsePathStyle: true for Garage-backed origins.
	originUsePathStyle bool

	// Cachestore fields. Always S3-shaped; the dev install uses
	// Garage as the cachestore (and also as the origin in
	// awss3 mode, on a different bucket).
	cachestoreEndpoint     string
	cachestoreBucket       string
	cachestoreRegion       string
	cachestoreAccessKey    string
	cachestoreSecretKey    string
	cachestoreUsePathStyle bool

	// Misc.
	ensureContainer bool

	// autoPortForward, when true (the default), causes any
	// subcommand that talks to the orca edge listener (and, when
	// the preset implies localhost-routed origin/cachestore
	// endpoints, the corresponding emulator Services) to probe the
	// configured localhost ports; if unreachable, orcadev spawns
	// managed kubectl port-forwards to the in-cluster Services
	// for the duration of the run. Set false to suppress every
	// auto-forward (e.g. in CI environments without kubectl on
	// PATH, or against a real ingress).
	autoPortForward bool
	// kubeContext is the kubectl context used by autoPortForward.
	// Empty means "current context" (matches kubectl's default
	// behavior).
	kubeContext string
}

// defaultGlobalFlags returns the dev-install-tuned defaults. These
// are the same as applyPresetDev would produce, baked in directly so
// that the zero-config invocation `bin/orcadev <verb>` works out of
// the box against a freshly installed dev cluster on whichever
// kubectl context is current.
//
// To target a non-dev orca (production-shape config, real cloud
// origins, ...), pass --preset=none and use --config plus the
// per-flag overrides.
func defaultGlobalFlags() *globalFlags {
	g := &globalFlags{
		// preset+namespace are the structural defaults; everything
		// else is filled by applyPresetDev below.
		preset:    PresetDev,
		namespace: unbounded.SystemNamespace(),
	}

	applyPresetDev(g)

	return g
}

// resolve loads --config (if set) and overlays per-flag overrides on
// top, producing the effective configuration. It is wired as a
// PersistentPreRunE so every subcommand sees fully-resolved fields
// without re-running this logic.
//
// Precedence: per-flag override > YAML value > preset default >
// built-in default. A flag is considered "overridden" when the
// operator actually passed it on the command line, detected via
// cobra's Flag.Changed bit.
//
// When --preset=dev is selected AND --origin-driver=awss3 is passed
// without other origin flags, the awss3-flavor preset defaults are
// re-applied to the un-overridden origin fields (origin-id,
// origin-bucket, origin-endpoint, etc.). This is the same logic the
// retired orcadev-flags.sh shell wrapper used to perform.
func (g *globalFlags) resolve(cmd *cobra.Command) error {
	flags := cmd.Flags()

	if err := validatePreset(g.preset); err != nil {
		return err
	}

	// If preset=dev and the operator switched to awss3 without
	// re-specifying the origin fields, swap in the awss3 dev
	// defaults. The default-driver case (azureblob) is already
	// baked in by defaultGlobalFlags().
	if g.preset == PresetDev && g.originDriver == "awss3" {
		applyPresetDevAWSS3(g, flags)
	}

	if g.configPath == "" {
		return nil
	}

	cfg, err := config.Load(g.configPath)
	if err != nil {
		return fmt.Errorf("load --config: %w", err)
	}

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
		// the default as truthy (Garage assumption) and let the
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
