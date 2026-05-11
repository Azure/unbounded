// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestApplyDefaults_EnvFallback verifies that applyDefaults populates
// credential / pod-identity fields from environment variables when
// the YAML omits them. This is the path used in production where the
// Kubernetes Secret is mounted via envFrom and the ConfigMap holds
// only the non-secret config.
//
// Each subtest sets one env var and checks that:
//   - env-set, yaml-empty -> field populated from env.
//   - env-unset, yaml-set -> field keeps yaml value.
//   - env-set, yaml-set   -> field keeps yaml value (yaml wins).
//   - env-unset, yaml-empty -> field stays empty.
func TestApplyDefaults_EnvFallback(t *testing.T) {
	tests := []struct {
		envVar string
		setVal func(c *Config, v string)
		getVal func(c *Config) string
	}{
		{
			envVar: "POD_IP",
			setVal: func(c *Config, v string) { c.Cluster.SelfPodIP = v },
			getVal: func(c *Config) string { return c.Cluster.SelfPodIP },
		},
		{
			envVar: "ORCA_AZUREBLOB_ACCOUNT_KEY",
			setVal: func(c *Config, v string) { c.Origin.Azureblob.AccountKey = v },
			getVal: func(c *Config) string { return c.Origin.Azureblob.AccountKey },
		},
		{
			envVar: "ORCA_AWSS3_ACCESS_KEY",
			setVal: func(c *Config, v string) { c.Origin.AWSS3.AccessKey = v },
			getVal: func(c *Config) string { return c.Origin.AWSS3.AccessKey },
		},
		{
			envVar: "ORCA_AWSS3_SECRET_KEY",
			setVal: func(c *Config, v string) { c.Origin.AWSS3.SecretKey = v },
			getVal: func(c *Config) string { return c.Origin.AWSS3.SecretKey },
		},
		{
			envVar: "ORCA_CACHESTORE_S3_ACCESS_KEY",
			setVal: func(c *Config, v string) { c.Cachestore.S3.AccessKey = v },
			getVal: func(c *Config) string { return c.Cachestore.S3.AccessKey },
		},
		{
			envVar: "ORCA_CACHESTORE_S3_SECRET_KEY",
			setVal: func(c *Config, v string) { c.Cachestore.S3.SecretKey = v },
			getVal: func(c *Config) string { return c.Cachestore.S3.SecretKey },
		},
	}

	for _, tt := range tests {
		t.Run(tt.envVar, func(t *testing.T) {
			t.Run("env_set/yaml_empty", func(t *testing.T) {
				t.Setenv(tt.envVar, "from-env")

				c := &Config{}
				c.applyDefaults()

				if got := tt.getVal(c); got != "from-env" {
					t.Errorf("got %q want %q", got, "from-env")
				}
			})

			t.Run("env_unset/yaml_set", func(t *testing.T) {
				_ = os.Unsetenv(tt.envVar) //nolint:errcheck // best-effort

				c := &Config{}
				tt.setVal(c, "from-yaml")
				c.applyDefaults()

				if got := tt.getVal(c); got != "from-yaml" {
					t.Errorf("got %q want %q", got, "from-yaml")
				}
			})

			t.Run("env_set/yaml_set_yaml_wins", func(t *testing.T) {
				t.Setenv(tt.envVar, "from-env")

				c := &Config{}
				tt.setVal(c, "from-yaml")
				c.applyDefaults()

				if got := tt.getVal(c); got != "from-yaml" {
					t.Errorf("got %q want %q (yaml should win)", got, "from-yaml")
				}
			})

			t.Run("env_unset/yaml_empty", func(t *testing.T) {
				_ = os.Unsetenv(tt.envVar) //nolint:errcheck // best-effort

				c := &Config{}
				c.applyDefaults()

				if got := tt.getVal(c); got != "" {
					t.Errorf("got %q want empty", got)
				}
			})
		})
	}
}

// TestApplyDefaults_FieldDefaults verifies that the hard-coded
// fallback values fire for every field whose zero value is replaced.
func TestApplyDefaults_FieldDefaults(t *testing.T) {
	t.Parallel()

	c := &Config{}
	c.applyDefaults()

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"server.listen", c.Server.Listen, "0.0.0.0:8443"},
		{"server.ops_listen", c.Server.OpsListen, "0.0.0.0:8442"},
		{"origin.driver", c.Origin.Driver, "azureblob"},
		{"origin.target_global", c.Origin.TargetGlobal, 192},
		{"origin.queue_timeout", c.Origin.QueueTimeout, 5 * time.Second},
		{"origin.retry.attempts", c.Origin.Retry.Attempts, 3},
		{"origin.retry.backoff_initial", c.Origin.Retry.BackoffInitial, 100 * time.Millisecond},
		{"origin.retry.backoff_max", c.Origin.Retry.BackoffMax, 2 * time.Second},
		{"origin.retry.max_total_duration", c.Origin.Retry.MaxTotalDuration, 5 * time.Second},
		{"cachestore.driver", c.Cachestore.Driver, "s3"},
		{"cachestore.s3.region", c.Cachestore.S3.Region, "us-east-1"},
		{"cluster.membership_refresh", c.Cluster.MembershipRefresh, 5 * time.Second},
		{"cluster.internal_listen", c.Cluster.InternalListen, "0.0.0.0:8444"},
		{"cluster.target_replicas", c.Cluster.TargetReplicas, 3},
		{"cluster.internal_tls.server_name", c.Cluster.InternalTLS.ServerName, "orca.<ns>.svc"},
		{"chunk_catalog.max_entries", c.ChunkCatalog.MaxEntries, 100_000},
		{"metadata.ttl", c.Metadata.TTL, 5 * time.Minute},
		{"metadata.negative_ttl", c.Metadata.NegativeTTL, 60 * time.Second},
		{"metadata.max_entries", c.Metadata.MaxEntries, 10_000},
		{"chunking.size", c.Chunking.Size, int64(8 * 1024 * 1024)},
		{"origin.awss3.region", c.Origin.AWSS3.Region, "us-east-1"},
		{"logging.level", c.Logging.Level, "info"},
	}

	for _, ch := range checks {
		if ch.got != ch.want {
			t.Errorf("%s: got %v want %v", ch.name, ch.got, ch.want)
		}
	}
}

// TestApplyDefaults_PreservesExplicitValues verifies that explicit
// non-zero values are not overwritten by applyDefaults.
func TestApplyDefaults_PreservesExplicitValues(t *testing.T) {
	t.Parallel()

	c := &Config{
		Server: Server{Listen: "1.2.3.4:9000"},
		Origin: Origin{
			Driver:       "awss3",
			TargetGlobal: 64,
		},
		Cachestore:   Cachestore{S3: CachestoreS3{Region: "eu-west-1"}},
		Cluster:      Cluster{TargetReplicas: 7, MembershipRefresh: 10 * time.Second},
		ChunkCatalog: ChunkCatalog{MaxEntries: 50},
		Metadata:     Metadata{TTL: time.Hour, MaxEntries: 99},
		Chunking:     Chunking{Size: 16 << 20},
	}

	c.applyDefaults()

	if c.Server.Listen != "1.2.3.4:9000" {
		t.Errorf("Server.Listen overwritten: %q", c.Server.Listen)
	}

	if c.Origin.Driver != "awss3" {
		t.Errorf("Origin.Driver overwritten: %q", c.Origin.Driver)
	}

	if c.Origin.TargetGlobal != 64 {
		t.Errorf("Origin.TargetGlobal overwritten: %d", c.Origin.TargetGlobal)
	}

	if c.Cachestore.S3.Region != "eu-west-1" {
		t.Errorf("Cachestore.S3.Region overwritten: %q", c.Cachestore.S3.Region)
	}

	if c.Cluster.TargetReplicas != 7 {
		t.Errorf("Cluster.TargetReplicas overwritten: %d", c.Cluster.TargetReplicas)
	}

	if c.Cluster.MembershipRefresh != 10*time.Second {
		t.Errorf("Cluster.MembershipRefresh overwritten: %v", c.Cluster.MembershipRefresh)
	}

	if c.ChunkCatalog.MaxEntries != 50 {
		t.Errorf("ChunkCatalog.MaxEntries overwritten: %d", c.ChunkCatalog.MaxEntries)
	}

	if c.Metadata.TTL != time.Hour {
		t.Errorf("Metadata.TTL overwritten: %v", c.Metadata.TTL)
	}

	if c.Chunking.Size != 16<<20 {
		t.Errorf("Chunking.Size overwritten: %d", c.Chunking.Size)
	}
}

// TestLoad_Validate covers the validate() error paths.
func TestLoad_Validate(t *testing.T) {
	// No t.Parallel: subtests use t.Setenv to neutralize POD_IP.
	tests := []struct {
		name    string
		yaml    string
		wantErr string
		wantOK  bool
	}{
		{
			name:   "valid awss3 config",
			yaml:   validAwss3YAML,
			wantOK: true,
		},
		{
			name:    "missing origin.id",
			yaml:    strings.ReplaceAll(validAwss3YAML, "id: test-origin", "id: \"\""),
			wantErr: "origin.id is required",
		},
		{
			name:    "unsupported driver",
			yaml:    strings.ReplaceAll(validAwss3YAML, "driver: awss3", "driver: ftp"),
			wantErr: "origin.driver",
		},
		{
			name:    "missing awss3 bucket",
			yaml:    strings.ReplaceAll(validAwss3YAML, "bucket: orca-origin", "bucket: \"\""),
			wantErr: "origin.awss3.bucket is required",
		},
		{
			name:    "missing cachestore endpoint",
			yaml:    strings.ReplaceAll(validAwss3YAML, "endpoint: http://localstack:4566", "endpoint: \"\""),
			wantErr: "cachestore.s3.endpoint is required",
		},
		{
			name:    "missing cluster service",
			yaml:    strings.ReplaceAll(validAwss3YAML, "service: orca-peers.svc", "service: \"\""),
			wantErr: "cluster.service is required",
		},
		{
			name:    "missing self_pod_ip when POD_IP unset",
			yaml:    strings.ReplaceAll(validAwss3YAML, "self_pod_ip: 10.0.0.1", "self_pod_ip: \"\""),
			wantErr: "self_pod_ip is required",
		},
		{
			name:    "target_replicas negative",
			yaml:    strings.ReplaceAll(validAwss3YAML, "target_replicas: 3", "target_replicas: -1"),
			wantErr: "target_replicas",
		},
		{
			name:    "chunking size below minimum",
			yaml:    strings.ReplaceAll(validAwss3YAML, "size: 8388608", "size: 4096"),
			wantErr: "chunking.size",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Ensure no leakage of POD_IP from the test process env.
			t.Setenv("POD_IP", "")

			path := writeTempYAML(t, tt.yaml)

			_, err := Load(path)
			if tt.wantOK {
				if err != nil {
					t.Fatalf("expected nil error, got %v", err)
				}

				return
			}

			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}

			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// TestParseLogLevel covers the orca log-level string -> slog.Level
// mapping. Both empty and "info" map to LevelInfo so the YAML default
// path matches the explicit-info path; "warn" and "warning" are
// accepted equivalently. Unknown values return a descriptive error
// so misconfiguration is surfaced rather than silently downgrading.
func TestParseLogLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      string
		want    slog.Level
		wantErr bool
	}{
		{"", slog.LevelInfo, false},
		{"info", slog.LevelInfo, false},
		{"INFO", slog.LevelInfo, false},
		{"debug", slog.LevelDebug, false},
		{" Debug ", slog.LevelDebug, false},
		{"warn", slog.LevelWarn, false},
		{"warning", slog.LevelWarn, false},
		{"error", slog.LevelError, false},
		{"trace", 0, true},
		{"verbose", 0, true},
		{"5", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseLogLevel(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ParseLogLevel(%q) = %v, want error", tt.in, got)
				}

				return
			}

			if err != nil {
				t.Errorf("ParseLogLevel(%q) unexpected err: %v", tt.in, err)
				return
			}

			if got != tt.want {
				t.Errorf("ParseLogLevel(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// TestValidate_RejectsInvalidLogLevel verifies that an unrecognised
// logging.level value is caught at config.Load time rather than at
// process startup.
func TestValidate_RejectsInvalidLogLevel(t *testing.T) {
	t.Parallel()

	yaml := validAwss3YAML + `
logging:
  level: trace
`
	path := writeTempYAML(t, yaml)

	_, err := Load(path)
	if err == nil {
		t.Fatalf("Load accepted invalid logging.level: trace")
	}

	if !strings.Contains(err.Error(), "logging.level") {
		t.Errorf("error does not mention logging.level: %v", err)
	}
}

func writeTempYAML(t *testing.T, content string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp yaml: %v", err)
	}

	return path
}

const validAwss3YAML = `
server:
  listen: 0.0.0.0:8443
origin:
  id: test-origin
  driver: awss3
  awss3:
    endpoint: http://localstack:4566
    region: us-east-1
    bucket: orca-origin
    access_key: test
    secret_key: test
    use_path_style: true
cachestore:
  driver: s3
  s3:
    endpoint: http://localstack:4566
    bucket: orca-cache
    region: us-east-1
    access_key: test
    secret_key: test
    use_path_style: true
cluster:
  service: orca-peers.svc
  self_pod_ip: 10.0.0.1
  target_replicas: 3
chunking:
  size: 8388608
`
