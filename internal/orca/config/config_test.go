// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
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
		{"chunking.size", c.Chunking.Size, ByteSize(8 * 1024 * 1024)},
		{"origin.awss3.region", c.Origin.AWSS3.Region, "us-east-1"},
		{"logging.level", c.Logging.Level, "info"},
	}

	for _, ch := range checks {
		if ch.got != ch.want {
			t.Errorf("%s: got %v want %v", ch.name, ch.got, ch.want)
		}
	}

	// Tiers default to the documented 2-entry ladder. Compared
	// separately since slice equality cannot use the table.
	wantTiers := []ChunkTier{
		{MinObjectSize: 1024 * 1024 * 1024, ChunkSize: 64 * 1024 * 1024},
		{MinObjectSize: 10 * 1024 * 1024 * 1024, ChunkSize: 128 * 1024 * 1024},
	}
	if len(c.Chunking.Tiers) != len(wantTiers) {
		t.Errorf("chunking.tiers length=%d want %d", len(c.Chunking.Tiers), len(wantTiers))
	} else {
		for i := range wantTiers {
			if c.Chunking.Tiers[i] != wantTiers[i] {
				t.Errorf("chunking.tiers[%d]=%+v want %+v",
					i, c.Chunking.Tiers[i], wantTiers[i])
			}
		}
	}
	// Readahead defaults to a non-nil pointer to 8.
	if c.Chunking.Readahead == nil {
		t.Errorf("chunking.readahead is nil; expected default pointer")
	} else if *c.Chunking.Readahead != 8 {
		t.Errorf("chunking.readahead=%d want 8", *c.Chunking.Readahead)
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
			yaml:    strings.ReplaceAll(validAwss3YAML, "endpoint: http://garage:3900", "endpoint: \"\""),
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

// TestValidateChunkingTiers_OK covers tier ladders that should pass
// validation: empty (feature off), single tier, multi-tier strictly
// ascending.
func TestValidateChunkingTiers_OK(t *testing.T) {
	t.Parallel()

	cases := [][]ChunkTier{
		nil,
		{},
		{{MinObjectSize: 1 << 30, ChunkSize: 64 << 20}},
		{
			{MinObjectSize: 1 << 30, ChunkSize: 64 << 20},
			{MinObjectSize: 10 << 30, ChunkSize: 128 << 20},
		},
	}

	for i, tiers := range cases {
		if err := validateChunkingTiers(tiers); err != nil {
			t.Errorf("case[%d] unexpected error: %v", i, err)
		}
	}
}

// TestValidateChunkingTiers_Errors covers the rejection paths: tiny
// chunk size, zero / negative min object size, unsorted thresholds,
// and duplicate thresholds (caught by the strict-ascending rule).
func TestValidateChunkingTiers_Errors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		tiers   []ChunkTier
		wantErr string
	}{
		{
			name: "chunk size below 1 MiB",
			tiers: []ChunkTier{
				{MinObjectSize: 1 << 30, ChunkSize: 1024},
			},
			wantErr: "chunk_size",
		},
		{
			name: "zero min object size",
			tiers: []ChunkTier{
				{MinObjectSize: 0, ChunkSize: 64 << 20},
			},
			wantErr: "min_object_size",
		},
		{
			name: "negative min object size",
			tiers: []ChunkTier{
				{MinObjectSize: -1, ChunkSize: 64 << 20},
			},
			wantErr: "min_object_size",
		},
		{
			name: "unsorted ascending rejected",
			tiers: []ChunkTier{
				{MinObjectSize: 10 << 30, ChunkSize: 64 << 20},
				{MinObjectSize: 1 << 30, ChunkSize: 128 << 20},
			},
			wantErr: "strictly ascending",
		},
		{
			name: "duplicate min object size rejected",
			tiers: []ChunkTier{
				{MinObjectSize: 1 << 30, ChunkSize: 64 << 20},
				{MinObjectSize: 1 << 30, ChunkSize: 128 << 20},
			},
			wantErr: "strictly ascending",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateChunkingTiers(tt.tiers)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}

			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// TestLoad_TiersAndReadahead drives validation through Load (full
// YAML path) to ensure the tier rejection surfaces with the rich
// error message and that an explicit readahead: 0 disables prefetch
// (i.e. survives applyDefaults and is not bumped back to 8).
func TestLoad_TiersAndReadahead(t *testing.T) {
	t.Parallel()

	t.Run("explicit_readahead_zero_preserved", func(t *testing.T) {
		yaml := validAwss3YAML + "  readahead: 0\n"
		path := writeTempYAML(t, yaml)

		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}

		if cfg.Chunking.Readahead == nil {
			t.Fatalf("Readahead should be non-nil after applyDefaults")
		}

		if *cfg.Chunking.Readahead != 0 {
			t.Errorf("Readahead=%d want 0 (explicit disable preserved)", *cfg.Chunking.Readahead)
		}

		if d := cfg.Chunking.ReadaheadDepth(); d != 0 {
			t.Errorf("ReadaheadDepth()=%d want 0", d)
		}
	})

	t.Run("explicit_empty_tiers_preserved", func(t *testing.T) {
		yaml := validAwss3YAML + "  tiers: []\n"
		path := writeTempYAML(t, yaml)

		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		// Tiers explicitly set to [] should survive applyDefaults
		// (the default ladder must not overwrite operator intent).
		if len(cfg.Chunking.Tiers) != 0 {
			t.Errorf("Tiers=%v want []; applyDefaults overwrote explicit empty",
				cfg.Chunking.Tiers)
		}

		if cfg.Chunking.AsChunkTiers() != nil {
			t.Errorf("AsChunkTiers() returned non-nil for empty tiers")
		}
	})

	t.Run("unsorted_tiers_rejected", func(t *testing.T) {
		yaml := validAwss3YAML + `  tiers:
    - min_object_size: 10737418240
      chunk_size: 67108864
    - min_object_size: 1073741824
      chunk_size: 134217728
`
		path := writeTempYAML(t, yaml)

		_, err := Load(path)
		if err == nil {
			t.Fatalf("Load accepted unsorted tiers")
		}

		if !strings.Contains(err.Error(), "strictly ascending") {
			t.Errorf("error %q does not mention strict ascending order", err.Error())
		}
	})

	t.Run("negative_readahead_rejected", func(t *testing.T) {
		yaml := validAwss3YAML + "  readahead: -1\n"
		path := writeTempYAML(t, yaml)

		_, err := Load(path)
		if err == nil {
			t.Fatalf("Load accepted negative readahead")
		}

		if !strings.Contains(err.Error(), "chunking.readahead") {
			t.Errorf("error %q does not mention chunking.readahead", err.Error())
		}
	})
}

// TestChunking_AsChunkTiers covers the config -> chunk.Tier mapping
// preserves order and field values, and returns nil for empty.
func TestChunking_AsChunkTiers(t *testing.T) {
	t.Parallel()

	c := Chunking{
		Size: 8 << 20,
		Tiers: []ChunkTier{
			{MinObjectSize: 1 << 30, ChunkSize: 64 << 20},
			{MinObjectSize: 10 << 30, ChunkSize: 128 << 20},
		},
	}

	got := c.AsChunkTiers()
	if len(got) != 2 {
		t.Fatalf("len=%d want 2", len(got))
	}

	if got[0].MinObjectSize != 1<<30 || got[0].ChunkSize != 64<<20 {
		t.Errorf("got[0]=%+v", got[0])
	}

	if got[1].MinObjectSize != 10<<30 || got[1].ChunkSize != 128<<20 {
		t.Errorf("got[1]=%+v", got[1])
	}

	if (Chunking{}).AsChunkTiers() != nil {
		t.Errorf("empty Chunking.AsChunkTiers() should be nil")
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

// TestByteSize_UnmarshalYAML covers the accepted scalar forms for
// ByteSize: numeric byte counts, IEC-suffixed strings, SI-suffixed
// strings, fractional strings, and quoted numeric strings. The
// table includes the legacy bare-integer form to lock in
// backward compatibility with configs predating this type.
func TestByteSize_UnmarshalYAML_Accepts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		yaml string
		want ByteSize
	}{
		{"bare integer (legacy)", "v: 8388608", 8 * 1024 * 1024},
		{"quoted integer string", `v: "8388608"`, 8 * 1024 * 1024},
		{"IEC MiB with space", `v: "8 MiB"`, 8 * 1024 * 1024},
		{"IEC MiB no space", `v: "8MiB"`, 8 * 1024 * 1024},
		// SI suffix is power-of-ten per humanize/IEC convention; this
		// asserts the answer-(1) decision (accept upstream library's
		// SI-vs-IEC semantics) and would fail a regression that
		// silently retreats to power-of-two SI.
		{"SI MB is decimal", `v: "1 MB"`, 1_000_000},
		{"SI MB no space", `v: "1MB"`, 1_000_000},
		{"IEC KiB is binary", `v: "1 KiB"`, 1024},
		{"IEC GiB", `v: "1 GiB"`, 1024 * 1024 * 1024},
		{"SI GB", `v: "1 GB"`, 1_000_000_000},
		{"IEC TiB", `v: "1 TiB"`, 1024 * 1024 * 1024 * 1024},
		// Fractional values are allowed (answer-(4)). The underlying
		// humanize.ParseBytes truncates the resulting byte count to
		// int64 semantics.
		{"fractional GiB", `v: "1.5 GiB"`, 1610612736},
		{"fractional MB", `v: "2.5 MB"`, 2_500_000},
		{"plain bytes via B suffix", `v: "100 B"`, 100},
		{"zero is allowed", "v: 0", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var doc struct {
				V ByteSize `yaml:"v"`
			}

			if err := yaml.Unmarshal([]byte(tt.yaml), &doc); err != nil {
				t.Fatalf("yaml.Unmarshal(%q): %v", tt.yaml, err)
			}

			if doc.V != tt.want {
				t.Errorf("got %d (%s), want %d (%s)",
					int64(doc.V), doc.V, int64(tt.want), tt.want)
			}
		})
	}
}

// TestByteSize_UnmarshalYAML_Rejects covers malformed scalars,
// negative values, and overflow above int64 max. Every rejection
// surfaces via the unmarshal error path (i.e. config.Load fails,
// validate is never reached) so operators see the offending YAML
// line via the error message.
func TestByteSize_UnmarshalYAML_Rejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		yaml    string
		wantSub string
	}{
		{"junk string", `v: "huge"`, "parse bytesize"},
		{"negative integer", `v: -1`, "must be >= 0"},
		{"negative string", `v: "-1 MiB"`, "must be >= 0"},
		// Two empty-scalar shapes: a quoted empty value and a key
		// with no value (which YAML resolves to the null tag, not
		// a scalar string). The first reaches the empty-string
		// guard; the second is rejected because the scalar value
		// is empty after trim.
		{"empty quoted scalar", `v: ""`, "bytesize is empty"},
		// 9 EiB > int64 max (~8 EiB). humanize.ParseBytes accepts
		// uint64 values larger than int64 max so the overflow guard
		// fires.
		{"overflow above int64 max", `v: "9 EiB"`, "overflows int64"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var doc struct {
				V ByteSize `yaml:"v"`
			}

			err := yaml.Unmarshal([]byte(tt.yaml), &doc)
			if err == nil {
				t.Fatalf("yaml.Unmarshal(%q) succeeded; want error containing %q",
					tt.yaml, tt.wantSub)
			}

			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantSub)
			}
		})
	}
}

// TestByteSize_NonScalarRejected covers the rare case of a YAML
// sequence or mapping where a scalar is expected. yaml.v3 will
// route the node into our UnmarshalYAML hook regardless of kind,
// so the hook must produce a clear error rather than panic.
func TestByteSize_NonScalarRejected(t *testing.T) {
	t.Parallel()

	var doc struct {
		V ByteSize `yaml:"v"`
	}

	err := yaml.Unmarshal([]byte("v: [1, 2, 3]"), &doc)
	if err == nil {
		t.Fatal("expected error for sequence value, got nil")
	}

	if !strings.Contains(err.Error(), "must be a scalar") {
		t.Errorf("error %q does not mention scalar requirement", err.Error())
	}
}

// TestByteSize_String covers the IEC rendering used in validation
// error messages. Renders rely on humanize.IBytes so the values are
// in the same units operators see when writing the YAML, regardless
// of whether the input was a raw byte count or a human string.
func TestByteSize_String(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   ByteSize
		want string
	}{
		{0, "0 B"},
		{1, "1 B"},
		{1024, "1.0 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{8 * 1024 * 1024, "8.0 MiB"},
		{1 << 30, "1.0 GiB"},
		// Negative byte counts are not produced by valid YAML, but
		// the String formatter must not panic if a test or future
		// code path manufactures one.
		{-1, "-1 B"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.in.String(); got != tt.want {
				t.Errorf("ByteSize(%d).String() = %q, want %q", int64(tt.in), got, tt.want)
			}
		})
	}
}

// TestLoad_ChunkingHumanUnits drives a full YAML load with human
// units for every byte-typed field under chunking, including a tier
// ladder written entirely in IEC strings. The integration check
// matters because Load wires UnmarshalYAML + applyDefaults +
// validate together; a regression in any of those would surface
// here even if the focused unit tests still passed.
func TestLoad_ChunkingHumanUnits(t *testing.T) {
	t.Parallel()

	yamlBody := strings.ReplaceAll(validAwss3YAML, "size: 8388608", `size: "16 MiB"`)
	yamlBody += `  tiers:
    - min_object_size: 1 GiB
      chunk_size: 64 MiB
    - min_object_size: 10 GiB
      chunk_size: 128 MiB
`
	path := writeTempYAML(t, yamlBody)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Chunking.Size != 16*1024*1024 {
		t.Errorf("Chunking.Size = %s (%d), want 16 MiB",
			cfg.Chunking.Size, int64(cfg.Chunking.Size))
	}

	wantTiers := []ChunkTier{
		{MinObjectSize: 1024 * 1024 * 1024, ChunkSize: 64 * 1024 * 1024},
		{MinObjectSize: 10 * 1024 * 1024 * 1024, ChunkSize: 128 * 1024 * 1024},
	}
	if len(cfg.Chunking.Tiers) != len(wantTiers) {
		t.Fatalf("len(Tiers) = %d, want %d", len(cfg.Chunking.Tiers), len(wantTiers))
	}

	for i, wt := range wantTiers {
		if cfg.Chunking.Tiers[i] != wt {
			t.Errorf("Tiers[%d] = %+v, want %+v",
				i, cfg.Chunking.Tiers[i], wt)
		}
	}
}

// TestLoad_ChunkingHumanUnits_BelowMinimum confirms the existing
// 1 MiB floor still bites when the operator writes the offending
// value in human units. The error message must surface the value
// in IEC units (via ByteSize.String) so the operator does not have
// to convert bytes by hand.
func TestLoad_ChunkingHumanUnits_BelowMinimum(t *testing.T) {
	t.Parallel()

	yamlBody := strings.ReplaceAll(validAwss3YAML, "size: 8388608", `size: "512 KiB"`)
	path := writeTempYAML(t, yamlBody)

	_, err := Load(path)
	if err == nil {
		t.Fatalf("Load accepted size below 1 MiB minimum")
	}

	if !strings.Contains(err.Error(), "chunking.size") {
		t.Errorf("error %q does not mention chunking.size", err.Error())
	}

	if !strings.Contains(err.Error(), "512 KiB") {
		t.Errorf("error %q does not render the offending value in IEC units",
			err.Error())
	}
}

// TestLoad_ChunkingHumanUnits_TierRejectionWithIECRender verifies
// the per-tier minimum-size error renders the offending tier's
// chunk_size in IEC units. Same motivation as the chunking.size
// counterpart above.
func TestLoad_ChunkingHumanUnits_TierRejectionWithIECRender(t *testing.T) {
	t.Parallel()

	yamlBody := validAwss3YAML + `  tiers:
    - min_object_size: 1 GiB
      chunk_size: "512 KiB"
`
	path := writeTempYAML(t, yamlBody)

	_, err := Load(path)
	if err == nil {
		t.Fatalf("Load accepted tier chunk_size below 1 MiB minimum")
	}

	if !strings.Contains(err.Error(), "512 KiB") {
		t.Errorf("error %q does not render the offending value in IEC units",
			err.Error())
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

// TestExampleConfigLoads guards the operator-facing reference YAML
// at hack/orca/config.example.yaml. The file is hand-maintained and
// must remain loadable end-to-end (yaml.Unmarshal + applyDefaults +
// validate) so an operator can `cp` it as a starting point. A
// schema change that breaks the example surfaces here rather than
// at the next operator's first `orca -config`. The test resolves
// the file path relative to the package using runtime.Caller so it
// works from any working directory `go test` is invoked from.
func TestExampleConfigLoads(t *testing.T) {
	// No t.Parallel: t.Setenv("POD_IP", ...) is used to simulate the
	// downward-API value the standard Deployment supplies.
	t.Setenv("POD_IP", "10.0.0.1")

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot resolve example config path")
	}
	// Walk up from internal/orca/config/config_test.go (3 levels) to
	// the repo root, then down into hack/orca/config.example.yaml.
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", ".."))
	examplePath := filepath.Join(repoRoot, "hack", "orca", "config.example.yaml")

	cfg, err := Load(examplePath)
	if err != nil {
		t.Fatalf("Load(%s): %v", examplePath, err)
	}
	// Sanity-check a few load-bearing values so a regression that
	// silently drops a section (e.g. tiers becomes the in-code default
	// because the YAML key was renamed) surfaces here.
	if cfg.Origin.ID == "" {
		t.Errorf("origin.id is empty; example must declare a non-empty origin.id")
	}

	if cfg.Cachestore.Driver != "s3" {
		t.Errorf("cachestore.driver = %q; want s3", cfg.Cachestore.Driver)
	}

	if cfg.Cluster.Service == "" {
		t.Errorf("cluster.service is empty; example must declare a non-empty service")
	}

	if cfg.Chunking.Size <= 0 {
		t.Errorf("chunking.size = %s; want > 0", cfg.Chunking.Size)
	}
	// The default tier ladder ships 2 entries; the example file
	// spells them out, so this asserts the ladder did parse rather
	// than fall through to applyDefaults.
	if len(cfg.Chunking.Tiers) < 2 {
		t.Errorf("chunking.tiers len = %d; expected the example to declare >= 2 tiers",
			len(cfg.Chunking.Tiers))
	}
}

const validAwss3YAML = `
server:
  listen: 0.0.0.0:8443
origin:
  id: test-origin
  driver: awss3
  awss3:
    endpoint: http://garage:3900
    region: us-east-1
    bucket: orca-origin
    access_key: test
    secret_key: test
    use_path_style: true
cachestore:
  driver: s3
  s3:
    endpoint: http://garage:3900
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
