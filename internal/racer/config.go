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

// Config contains controller runtime settings, not operator workload inputs.
type Config struct {
	Cluster                   wire.ClusterID
	Namespace                 string
	ControlAddress            string
	MetricsAddress            string
	ProbeAddress              string
	TLSCertificateFile        string
	TLSPrivateKeyFile         string
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
	PodName                   string
	PodUID                    string
	ControllerServiceAccount  string
	ReplicationTokenFile      string
	ReplicationTrustFile      string
	ReplicationServerName     string
	ReplicationPort           uint16
	SnapshotMaxAge            time.Duration
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
		TLSCertificateFile:        env("RACER_TLS_CERTIFICATE_FILE", "/etc/racer/tls/tls.crt"),
		TLSPrivateKeyFile:         env("RACER_TLS_PRIVATE_KEY_FILE", "/etc/racer/tls/tls.key"),
		PeerPort:                  uint16(port),
		DataplaneServiceAccount:   env("RACER_DATAPLANE_SERVICE_ACCOUNT", "racer-dataplane"),
		DaemonSetName:             env("RACER_DAEMONSET_NAME", "racer-dataplane"),
		IssuerSecretName:          env("RACER_ISSUER_SECRET_NAME", "racer-issuer"),
		KeyringSecretName:         env("RACER_KEYRING_SECRET_NAME", "racer-keyring"),
		VersionConfigMapName:      env("RACER_VERSION_CONFIGMAP_NAME", "racer-version"),
		InstallationConfigMapName: env("RACER_INSTALLATION_CONFIGMAP_NAME", "racer-installation"),
		PodName:                   env("POD_NAME", ""),
		PodUID:                    env("POD_UID", ""),
		ControllerServiceAccount:  env("RACER_CONTROLLER_SERVICE_ACCOUNT", "racer-controller"),
		ReplicationTokenFile:      env("RACER_REPLICATION_TOKEN_FILE", "/var/run/secrets/racer-controller/token"),
		ReplicationTrustFile:      env("RACER_REPLICATION_TRUST_FILE", "/etc/racer/tls/ca.crt"),
		ReplicationServerName:     env("RACER_REPLICATION_SERVER_NAME", "racer-controller."+env("POD_NAMESPACE", "unbounded-system")+".svc"),
		SnapshotMaxAge:            30 * time.Second,
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

	replicationPort, err := strconv.ParseUint(env("RACER_REPLICATION_PORT", "8443"), 10, 16)
	if err != nil || replicationPort == 0 {
		return Config{}, fmt.Errorf("RACER_REPLICATION_PORT: %w", wire.InvalidRequest)
	}

	cfg.ReplicationPort = uint16(replicationPort)

	for _, setting := range []struct {
		name  string
		value *time.Duration
	}{
		{"RACER_CERTIFICATE_LIFETIME", &cfg.CertificateLifetime},
		{"RACER_SNAPSHOT_MAX_AGE", &cfg.SnapshotMaxAge},
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
	if c.SnapshotMaxAge < 0 || c.SnapshotMaxAge > 0 && c.SnapshotMaxAge < time.Second {
		return fmt.Errorf("snapshot maximum age: %w", wire.InvalidRequest)
	}

	if !wire.ValidUUID(string(c.Cluster)) || len(validation.IsDNS1123Label(c.Namespace)) != 0 || c.PeerPort == 0 {
		return fmt.Errorf("cluster, namespace, or peer port: %w", wire.InvalidRequest)
	}

	for _, name := range []string{c.VersionConfigMapName, c.InstallationConfigMapName, c.DaemonSetName, c.IssuerSecretName, c.KeyringSecretName, c.DataplaneServiceAccount} {
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

func (c Config) snapshotMaxAge() time.Duration {
	if c.SnapshotMaxAge == 0 {
		return 30 * time.Second
	}

	return c.SnapshotMaxAge
}

func (c Config) validateReplication() error {
	if len(validation.IsDNS1123Subdomain(c.PodName)) != 0 || c.PodUID == "" || len(validation.IsDNS1123Subdomain(c.ControllerServiceAccount)) != 0 || len(validation.IsDNS1123Subdomain(c.ReplicationServerName)) != 0 || c.ReplicationPort == 0 || c.ReplicationTokenFile == "" || c.ReplicationTrustFile == "" {
		return fmt.Errorf("replication requires POD_NAME, POD_UID, controller identity, TLS trust, token, server name, and port: %w", wire.InvalidRequest)
	}

	return nil
}
