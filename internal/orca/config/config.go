// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package config defines Orca's YAML configuration shape and loading
// helpers.
//
// The schema is an intentional subset of the full Orca configuration
// surface; extending it later is a matter of adding fields and keeping
// zero-values backward-compatible.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level Orca configuration.
type Config struct {
	Server       Server       `yaml:"server"`
	Origin       Origin       `yaml:"origin"`
	Cachestore   Cachestore   `yaml:"cachestore"`
	Cluster      Cluster      `yaml:"cluster"`
	ChunkCatalog ChunkCatalog `yaml:"chunk_catalog"`
	Metadata     Metadata     `yaml:"metadata"`
	Chunking     Chunking     `yaml:"chunking"`
}

// Server holds the client-edge listener configuration plus the
// ops listener used for kubelet probes (/healthz and /readyz).
type Server struct {
	Listen string     `yaml:"listen"`
	Auth   ServerAuth `yaml:"auth"`

	// OpsListen is the bind address for the operations endpoint
	// hosting /healthz and /readyz. Plain HTTP, no auth. Kubelet
	// liveness and readiness probes target this address; production
	// Service objects do not forward this port externally.
	OpsListen string `yaml:"ops_listen"`
}

// ServerAuth governs the client-edge authentication path.
//
// Production: enabled=true with mode=bearer or mode=mtls.
// Dev: enabled=false disables authentication entirely (no token
// or client cert required). This is a single security knob, not a
// dev_mode flag.
type ServerAuth struct {
	Enabled          bool   `yaml:"enabled"`
	Mode             string `yaml:"mode"`
	BearerSecretFile string `yaml:"bearer_secret_file"`
}

// Origin describes the upstream origin (Azure Blob or AWS S3 in v1).
type Origin struct {
	ID           string        `yaml:"id"`
	Driver       string        `yaml:"driver"` // "azureblob" or "awss3"
	TargetGlobal int           `yaml:"target_global"`
	QueueTimeout time.Duration `yaml:"queue_timeout"`
	Retry        OriginRetry   `yaml:"retry"`
	Azureblob    Azureblob     `yaml:"azureblob"`
	AWSS3        AWSS3         `yaml:"awss3"`
}

// OriginRetry captures the leader-side pre-header retry budget.
type OriginRetry struct {
	Attempts         int           `yaml:"attempts"`
	BackoffInitial   time.Duration `yaml:"backoff_initial"`
	BackoffMax       time.Duration `yaml:"backoff_max"`
	MaxTotalDuration time.Duration `yaml:"max_total_duration"`
}

// Azureblob is the azureblob origin adapter configuration.
//
// Page and Append blobs are unconditionally rejected at Head: their
// random-access mutation model is incompatible with the chunked,
// immutable cache contract orca relies on. There is no configuration
// switch for this behaviour.
type Azureblob struct {
	Account    string `yaml:"account"`
	AccountKey string `yaml:"account_key"`
	Container  string `yaml:"container"`

	// Endpoint, when set, overrides the default Azure Blob service URL
	// (https://<account>.blob.core.windows.net/). Used in dev to point
	// at Azurite (http://azurite:10000/devstoreaccount1) so the
	// azureblob driver path can be exercised without a real Azure
	// account.
	Endpoint string `yaml:"endpoint"`
}

// AWSS3 is the awss3 origin adapter configuration. In dev this points
// at LocalStack alongside the cachestore (different bucket); in
// production it points at real AWS S3 with no Endpoint override.
type AWSS3 struct {
	Endpoint     string `yaml:"endpoint"` // empty for real AWS S3
	Region       string `yaml:"region"`
	Bucket       string `yaml:"bucket"`
	AccessKey    string `yaml:"access_key"`
	SecretKey    string `yaml:"secret_key"`
	UsePathStyle bool   `yaml:"use_path_style"` // true for LocalStack
}

// Cachestore is the in-DC chunk store configuration.
type Cachestore struct {
	Driver string       `yaml:"driver"` // "s3" in v1
	S3     CachestoreS3 `yaml:"s3"`
}

// CachestoreS3 is the s3 driver configuration. In dev this points at
// LocalStack; in production at VAST or another in-DC S3-compatible
// store.
//
// Bucket versioning is unconditionally validated at startup: a
// versioned bucket silently breaks the no-clobber atomic-commit
// primitive (PutObject + If-None-Match: *) the driver depends on.
// There is no configuration switch for this gate.
type CachestoreS3 struct {
	Endpoint     string `yaml:"endpoint"`
	Bucket       string `yaml:"bucket"`
	Region       string `yaml:"region"`
	AccessKey    string `yaml:"access_key"`
	SecretKey    string `yaml:"secret_key"`
	UsePathStyle bool   `yaml:"use_path_style"` // true for LocalStack
}

// Cluster captures peer discovery + internal-listener configuration.
type Cluster struct {
	Service           string        `yaml:"service"`            // headless Service FQDN
	MembershipRefresh time.Duration `yaml:"membership_refresh"` // DNS poll interval
	InternalListen    string        `yaml:"internal_listen"`
	InternalTLS       InternalTLS   `yaml:"internal_tls"`
	TargetReplicas    int           `yaml:"target_replicas"`
	SelfPodIP         string        `yaml:"self_pod_ip"` // resolved from POD_IP env
}

// InternalTLS governs the internal-listener mTLS posture.
//
// Production: enabled=true (mTLS required).
// Dev: enabled=false (plain HTTP/2). The binary logs WARN at startup.
type InternalTLS struct {
	Enabled    bool   `yaml:"enabled"`
	CertFile   string `yaml:"cert_file"`
	KeyFile    string `yaml:"key_file"`
	CAFile     string `yaml:"ca_file"`
	ServerName string `yaml:"server_name"`
}

// ChunkCatalog is the in-memory chunk-presence cache configuration.
type ChunkCatalog struct {
	MaxEntries int `yaml:"max_entries"`
}

// Metadata is the object-metadata cache configuration.
type Metadata struct {
	TTL         time.Duration `yaml:"ttl"`
	NegativeTTL time.Duration `yaml:"negative_ttl"`
	MaxEntries  int           `yaml:"max_entries"`
}

// Chunking governs chunk size and prefetch.
type Chunking struct {
	Size int64 `yaml:"size"` // bytes per chunk; default 8 MiB
}

// Load reads the YAML config file at path and returns a populated
// Config. Defaults are applied for fields left at zero-value.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	cfg := &Config{}
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("yaml unmarshal: %w", err)
	}

	cfg.applyDefaults()

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config invalid: %w", err)
	}

	return cfg, nil
}

func (c *Config) applyDefaults() {
	// Server.
	if c.Server.Listen == "" {
		c.Server.Listen = "0.0.0.0:8443"
	}

	if c.Server.OpsListen == "" {
		c.Server.OpsListen = "0.0.0.0:8442"
	}
	// Origin.
	if c.Origin.Driver == "" {
		c.Origin.Driver = "azureblob"
	}

	if c.Origin.TargetGlobal == 0 {
		c.Origin.TargetGlobal = 192
	}

	if c.Origin.QueueTimeout == 0 {
		c.Origin.QueueTimeout = 5 * time.Second
	}

	if c.Origin.Retry.Attempts == 0 {
		c.Origin.Retry.Attempts = 3
	}

	if c.Origin.Retry.BackoffInitial == 0 {
		c.Origin.Retry.BackoffInitial = 100 * time.Millisecond
	}

	if c.Origin.Retry.BackoffMax == 0 {
		c.Origin.Retry.BackoffMax = 2 * time.Second
	}

	if c.Origin.Retry.MaxTotalDuration == 0 {
		c.Origin.Retry.MaxTotalDuration = 5 * time.Second
	}
	// Cachestore.
	if c.Cachestore.Driver == "" {
		c.Cachestore.Driver = "s3"
	}

	if c.Cachestore.S3.Region == "" {
		c.Cachestore.S3.Region = "us-east-1"
	}
	// Cluster.
	if c.Cluster.MembershipRefresh == 0 {
		c.Cluster.MembershipRefresh = 5 * time.Second
	}

	if c.Cluster.InternalListen == "" {
		c.Cluster.InternalListen = "0.0.0.0:8444"
	}

	if c.Cluster.TargetReplicas == 0 {
		c.Cluster.TargetReplicas = 3
	}

	if c.Cluster.InternalTLS.ServerName == "" {
		c.Cluster.InternalTLS.ServerName = "orca.<ns>.svc"
	}
	// Resolve self pod IP from env if not set in YAML.
	if c.Cluster.SelfPodIP == "" {
		c.Cluster.SelfPodIP = os.Getenv("POD_IP")
	}
	// Resolve credentials from env if not set in YAML. This lets the
	// non-secret config live in a ConfigMap while credentials come from
	// a Kubernetes Secret mounted as env vars (envFrom: secretRef).
	if c.Origin.Azureblob.AccountKey == "" {
		c.Origin.Azureblob.AccountKey = os.Getenv("ORCA_AZUREBLOB_ACCOUNT_KEY")
	}

	if c.Origin.AWSS3.AccessKey == "" {
		c.Origin.AWSS3.AccessKey = os.Getenv("ORCA_AWSS3_ACCESS_KEY")
	}

	if c.Origin.AWSS3.SecretKey == "" {
		c.Origin.AWSS3.SecretKey = os.Getenv("ORCA_AWSS3_SECRET_KEY")
	}

	if c.Cachestore.S3.AccessKey == "" {
		c.Cachestore.S3.AccessKey = os.Getenv("ORCA_CACHESTORE_S3_ACCESS_KEY")
	}

	if c.Cachestore.S3.SecretKey == "" {
		c.Cachestore.S3.SecretKey = os.Getenv("ORCA_CACHESTORE_S3_SECRET_KEY")
	}
	// awss3 region default.
	if c.Origin.AWSS3.Region == "" {
		c.Origin.AWSS3.Region = "us-east-1"
	}
	// Chunk catalog.
	if c.ChunkCatalog.MaxEntries == 0 {
		c.ChunkCatalog.MaxEntries = 100_000
	}
	// Metadata.
	if c.Metadata.TTL == 0 {
		c.Metadata.TTL = 5 * time.Minute
	}

	if c.Metadata.NegativeTTL == 0 {
		c.Metadata.NegativeTTL = 60 * time.Second
	}

	if c.Metadata.MaxEntries == 0 {
		c.Metadata.MaxEntries = 10_000
	}
	// Chunking.
	if c.Chunking.Size == 0 {
		c.Chunking.Size = 8 * 1024 * 1024
	}
}

func (c *Config) validate() error {
	if c.Origin.ID == "" {
		return fmt.Errorf("origin.id is required")
	}

	switch c.Origin.Driver {
	case "azureblob":
		if c.Origin.Azureblob.Account == "" {
			return fmt.Errorf("origin.azureblob.account is required")
		}

		if c.Origin.Azureblob.Container == "" {
			return fmt.Errorf("origin.azureblob.container is required")
		}
	case "awss3":
		if c.Origin.AWSS3.Bucket == "" {
			return fmt.Errorf("origin.awss3.bucket is required")
		}
	default:
		return fmt.Errorf("origin.driver %q unsupported; supported: azureblob, awss3",
			c.Origin.Driver)
	}

	if c.Cachestore.Driver != "s3" {
		return fmt.Errorf("cachestore.driver %q unsupported; only s3 in v1", c.Cachestore.Driver)
	}

	if c.Cachestore.S3.Endpoint == "" {
		return fmt.Errorf("cachestore.s3.endpoint is required")
	}

	if c.Cachestore.S3.Bucket == "" {
		return fmt.Errorf("cachestore.s3.bucket is required")
	}

	if c.Cluster.Service == "" {
		return fmt.Errorf("cluster.service is required (headless Service FQDN)")
	}

	if c.Cluster.SelfPodIP == "" {
		return fmt.Errorf("cluster.self_pod_ip is required (typically resolved from POD_IP env)")
	}

	if c.Cluster.TargetReplicas < 1 {
		return fmt.Errorf("cluster.target_replicas must be >= 1")
	}

	if c.Origin.TargetGlobal < c.Cluster.TargetReplicas {
		return fmt.Errorf(
			"origin.target_global=%d must be >= cluster.target_replicas=%d",
			c.Origin.TargetGlobal, c.Cluster.TargetReplicas,
		)
	}

	if c.Chunking.Size < 1024*1024 {
		return fmt.Errorf("chunking.size %d too small; minimum 1 MiB", c.Chunking.Size)
	}

	return nil
}

// TargetPerReplica returns the per-replica origin concurrency cap
// derived from origin.target_global divided by cluster.target_replicas.
// This bounds the number of concurrent in-flight origin requests this
// replica will issue.
func (c *Config) TargetPerReplica() int {
	if c.Cluster.TargetReplicas <= 0 {
		return c.Origin.TargetGlobal
	}

	return c.Origin.TargetGlobal / c.Cluster.TargetReplicas
}
