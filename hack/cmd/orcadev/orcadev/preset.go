// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package orcadev

import (
	"fmt"
	"time"

	"github.com/spf13/pflag"

	"github.com/Azure/unbounded/internal/unbounded"
)

// defaultNamespace is the namespace hack/orca/setup-orca.sh installs
// Orca + Azurite + Garage into. Mirrored here so the dev preset
// can drive its own auto-port-forward without needing to read a
// manifest or kubeconfig context annotation.
const defaultNamespace = unbounded.SystemNamespace

// Service name constants used by the dev preset. These match the
// Service objects produced by deploy/orca/**/*.yaml.tmpl, which the
// setup-orca.sh install script applies into defaultNamespace.
const (
	devSvcOrca    = "orca"
	devSvcAzurite = "azurite"
	devSvcGarage  = "garage"
)

// Deterministic Garage dev credentials. Garage enforces SigV4, so the
// S3 clients orcadev builds (cachestore inspection and awss3-origin
// seeding) need real keys. These mirror the constants in
// hack/orca/setup-orca.sh and the Garage bootstrap's `key import`;
// dev-only, not secret. The access key id must be "GK" + 12 hex bytes.
const (
	devGarageAccessKey = "GK0123456789abcdef01234567"
	devGarageSecretKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// Port constants used by the dev preset.
//
//   - 8443  is the orca edge listener (deploy/orca/05-service.yaml.tmpl).
//   - 10000 is the azurite blob endpoint inside the Pod (deploy/orca/dev/03-azurite.yaml.tmpl).
//   - 3900  is the Garage S3 API endpoint inside the Pod (deploy/orca/dev/01-garage.yaml.tmpl).
//
// The local-side ports (30100, 30200) match the kind extraPortMappings
// in hack/orca/kind-config.yaml. The preset still works on clusters
// without those mappings because orcadev's auto-port-forward will
// open kubectl forwards from the local-side port to the in-Pod
// port. On kind the probes succeed without spawning anything (the
// NodePort already binds those local ports); on other clusters the
// forward is spawned automatically. Either way the dev URLs are the
// same.
const (
	devLocalPortOrca     = 8443
	devRemotePortOrca    = 8443
	devLocalPortAzurite  = 30100
	devRemotePortAzurite = 10000
	devLocalPortGarage   = 30200
	devRemotePortGarage  = 3900
)

// devOrcaURL is the edge URL all dev-install orcadev invocations
// target. The auto-port-forward keeps this reachable transparently.
const devOrcaURL = "http://localhost:8443"

// devOriginAzuriteEndpoint is the azureblob driver endpoint pointing
// at the in-cluster Azurite emulator via the local-side port. The
// trailing path segment is the well-known Azurite account name; the
// Azure SDK uses path-style addressing when the account name appears
// here rather than in the URL host.
const devOriginAzuriteEndpoint = "http://localhost:30100/devstoreaccount1/"

// devCachestoreEndpoint is the S3 cachestore endpoint pointing at the
// in-cluster Garage emulator via the local-side port.
const devCachestoreEndpoint = "http://localhost:30200"

// devOriginAWSS3Endpoint is the awss3-flavor origin endpoint pointing
// at the in-cluster Garage emulator (same Service as the cachestore
// but a different bucket; see the deploy/orca/dev manifests).
const devOriginAWSS3Endpoint = "http://localhost:30200"

// supportedPresets lists the values --preset accepts. Single entry
// today; extension is one new applyPreset* + an entry here.
var supportedPresets = []string{PresetDev, "none"}

// validatePreset returns nil if name is a recognised preset.
func validatePreset(name string) error {
	for _, p := range supportedPresets {
		if name == p {
			return nil
		}
	}

	return fmt.Errorf("unknown --preset=%q (supported: %v)", name, supportedPresets)
}

// applyPresetDev populates g with the azureblob-flavor dev defaults.
// It is called from defaultGlobalFlags() (so the dev preset is the
// out-of-the-box behavior) and never re-applied later (a preset
// switch via the CLI would require a `--preset=...` round-trip; the
// only such case today is awss3, handled by applyPresetDevAWSS3
// below).
func applyPresetDev(g *globalFlags) {
	g.orcaURL = devOrcaURL
	g.timeout = 30 * time.Second

	g.logLevel = "info"

	g.originDriver = "azureblob"
	g.originID = "azureblob-azurite"
	g.originBucket = "orca-test"
	g.originEndpoint = devOriginAzuriteEndpoint
	g.originRegion = "us-east-1"
	g.originAccessKey = "test"
	g.originSecretKey = "test"
	g.originUsePathStyle = true
	g.originAccount = "devstoreaccount1"
	g.originAccountKey = azuriteWellKnownDevKey

	g.cachestoreEndpoint = devCachestoreEndpoint
	g.cachestoreBucket = "orca-cache"
	g.cachestoreRegion = "us-east-1"
	g.cachestoreAccessKey = devGarageAccessKey
	g.cachestoreSecretKey = devGarageSecretKey
	g.cachestoreUsePathStyle = true

	g.autoPortForward = true
	g.kubeContext = "" // empty = current kubectl context
}

// applyPresetDevAWSS3 overlays the awss3-flavor origin defaults on
// top of applyPresetDev's azureblob defaults. Called from resolve()
// only when --origin-driver=awss3 AND the matching origin-* flags
// were not individually overridden by the operator. Mirrors the
// awss3 branch of the retired orcadev-flags.sh.
func applyPresetDevAWSS3(g *globalFlags, flags *pflag.FlagSet) {
	notSet := func(name string) bool { return !flags.Changed(name) }

	if notSet("origin-id") {
		g.originID = "awss3-garage"
	}

	if notSet("origin-bucket") {
		g.originBucket = "orca-origin"
	}

	if notSet("origin-endpoint") {
		g.originEndpoint = devOriginAWSS3Endpoint
	}

	if notSet("origin-region") {
		g.originRegion = "us-east-1"
	}

	if notSet("origin-use-path-style") {
		g.originUsePathStyle = true
	}

	if notSet("origin-access-key") {
		g.originAccessKey = devGarageAccessKey
	}

	if notSet("origin-secret-key") {
		g.originSecretKey = devGarageSecretKey
	}
}
