// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/Azure/unbounded/internal/racer/wire"
)

type Config struct {
	Cluster                   wire.ClusterID
	Namespace                 string
	ControlAddress            string
	MetricsAddress            string
	ProbeAddress              string
	ControlURL                string
	TLSCertificateFile        string
	TLSPrivateKeyFile         string
	BootstrapTrustConfigMap   string
	DataplaneImage            string
	PeerPort                  uint16
	DataplaneServiceAccount   string
	DaemonSetName             string
	IssuerSecretName          string
	KeyringSecretName         string
	VersionConfigMapName      string
	InstallationConfigMapName string
	Limits                    Limits
	Rotation                  RotationPolicy
	CertificateLifetime       time.Duration
}

type Limits struct {
	MaxPolls               int
	MaxConcurrentWrites    int
	MaxConcurrentBootstrap int
	HeaderBytes            int
	WriteTimeout           time.Duration
	ShutdownTimeout        time.Duration
}

// LoadConfig reads deployment configuration. Initialization state is deliberately
// not an environment setting: it is read authoritatively on every recovery.
func LoadConfig() (Config, error) {
	return ConfigFromLookup(os.LookupEnv)
}

// ConfigFromLookup parses the controller configuration without process-global state.
func ConfigFromLookup(lookup func(string) (string, bool)) (Config, error) {
	env := func(key, fallback string) string {
		if value, ok := lookup(key); ok {
			return value
		}

		return fallback
	}

	port, err := strconv.ParseUint(env("RACER_PEER_PORT", "8082"), 10, 16)
	if err != nil {
		return Config{}, fmt.Errorf("RACER_PEER_PORT: %w", wire.InvalidRequest)
	}

	cfg := Config{
		Cluster:                   wire.ClusterID(env("RACER_CLUSTER_ID", "")),
		Namespace:                 env("POD_NAMESPACE", "unbounded-system"),
		ControlAddress:            env("RACER_CONTROL_ADDRESS", ":8443"),
		MetricsAddress:            env("RACER_METRICS_ADDRESS", ":8080"),
		ProbeAddress:              env("RACER_PROBE_ADDRESS", ":8081"),
		ControlURL:                env("RACER_CONTROL_URL", ""),
		TLSCertificateFile:        env("RACER_TLS_CERTIFICATE_FILE", "/etc/racer/tls/tls.crt"),
		TLSPrivateKeyFile:         env("RACER_TLS_PRIVATE_KEY_FILE", "/etc/racer/tls/tls.key"),
		BootstrapTrustConfigMap:   env("RACER_BOOTSTRAP_TRUST_CONFIGMAP", "racer-bootstrap-trust"),
		DataplaneImage:            env("RACER_DATAPLANE_IMAGE", ""),
		PeerPort:                  uint16(port),
		DataplaneServiceAccount:   env("RACER_DATAPLANE_SERVICE_ACCOUNT", "racer-dataplane"),
		DaemonSetName:             env("RACER_DAEMONSET_NAME", "racer-dataplane"),
		IssuerSecretName:          env("RACER_ISSUER_SECRET_NAME", "racer-issuer"),
		KeyringSecretName:         env("RACER_KEYRING_SECRET_NAME", "racer-keyring"),
		VersionConfigMapName:      env("RACER_VERSION_CONFIGMAP_NAME", "racer-version"),
		InstallationConfigMapName: env("RACER_INSTALLATION_CONFIGMAP_NAME", "racer-installation"),
		Limits: Limits{
			MaxPolls:               wire.MaxMembers,
			MaxConcurrentWrites:    128,
			MaxConcurrentBootstrap: 32,
			HeaderBytes:            16 * 1024,
			WriteTimeout:           30 * time.Second,
			ShutdownTimeout:        10 * time.Second,
		},
		Rotation: RotationPolicy{
			Interval:   24 * time.Hour,
			PrepareFor: time.Hour,
			RetainFor:  48 * time.Hour,
		},
	}

	for _, setting := range []struct {
		name  string
		value *time.Duration
	}{
		{"RACER_CERTIFICATE_LIFETIME", &cfg.CertificateLifetime},
		{"RACER_ROTATION_INTERVAL", &cfg.Rotation.Interval},
		{"RACER_ROTATION_PREPARE_FOR", &cfg.Rotation.PrepareFor},
		{"RACER_ROTATION_RETAIN_FOR", &cfg.Rotation.RetainFor},
	} {
		if value, ok := lookup(setting.name); ok {
			duration, err := time.ParseDuration(value)
			if err != nil || duration <= 0 || duration%time.Second != 0 {
				return Config{}, fmt.Errorf("%s: %w", setting.name, wire.InvalidRequest)
			}

			*setting.value = duration
		}
	}

	return cfg, cfg.Validate()
}

// A zero value preserves the lifetime used by existing programmatic callers.
func (c Config) certificateLifetime() time.Duration {
	if c.CertificateLifetime == 0 {
		return wire.CertificateLifetime
	}

	return c.CertificateLifetime
}

func (c Config) Validate() error {
	if !wire.ValidUUID(string(c.Cluster)) || len(validation.IsDNS1123Label(c.Namespace)) != 0 || c.PeerPort == 0 {
		return fmt.Errorf("cluster, namespace, or peer port: %w", wire.InvalidRequest)
	}

	for _, name := range []string{c.VersionConfigMapName, c.InstallationConfigMapName, c.DaemonSetName, c.IssuerSecretName, c.KeyringSecretName, c.BootstrapTrustConfigMap, c.DataplaneServiceAccount} {
		if len(validation.IsDNS1123Subdomain(name)) != 0 {
			return fmt.Errorf("resource name: %w", wire.InvalidRequest)
		}
	}

	if c.VersionConfigMapName == c.InstallationConfigMapName || c.Limits.MaxPolls <= 0 || c.Limits.MaxConcurrentWrites <= 0 || c.Limits.MaxConcurrentBootstrap <= 0 || c.Limits.HeaderBytes <= 0 || c.Limits.WriteTimeout <= 0 || c.Limits.ShutdownTimeout <= 0 {
		return fmt.Errorf("resource names or limits: %w", wire.InvalidRequest)
	}

	// Two minutes leaves a full poll turn between renewal at two-thirds of the
	// lifetime and expiry. X.509 and rotation deadlines have second precision.
	lifetime := c.certificateLifetime()
	if lifetime < 2*time.Minute || lifetime > wire.CertificateLifetime || lifetime%time.Second != 0 {
		return fmt.Errorf("certificate lifetime: %w", wire.InvalidRequest)
	}

	if c.IssuerSecretName == c.KeyringSecretName || c.Rotation.PrepareFor <= 0 || c.Rotation.Interval < c.Rotation.PrepareFor || c.Rotation.RetainFor < lifetime || c.Rotation.Interval > 365*24*time.Hour || c.Rotation.RetainFor > 365*24*time.Hour {
		return fmt.Errorf("credential names or rotation policy: %w", wire.InvalidRequest)
	}

	return nil
}
