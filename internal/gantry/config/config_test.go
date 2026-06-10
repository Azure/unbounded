// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package config

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestDefaultsValidateAfterMinimalUpstream(t *testing.T) {
	c := NewDefault()
	// Defaults intentionally have no upstream registries - operator must
	// supply at least one. Seed one and re-validate.
	c.UpstreamRegistries = []UpstreamRegistry{
		{Name: "registry.example.com", Endpoint: "https://registry.example.com"},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestValidate_RequiresUpstream(t *testing.T) {
	c := NewDefault()

	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "upstream_registries") {
		t.Fatalf("want upstream_registries error, got %v", err)
	}
}

func TestValidate_MirrorListenMustBeLoopback(t *testing.T) {
	c := NewDefault()
	c.UpstreamRegistries = []UpstreamRegistry{{Name: "r", Endpoint: "https://r"}}
	c.MirrorListen = "0.0.0.0:5000"

	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("want loopback error, got %v", err)
	}
}

// MirrorBindAllowNonLoopback is the operator opt-in for deployments that
// rely on hostPort + hostIP=127.0.0.1 to keep the mirror node-local while
// still binding 0.0.0.0 inside the pod (so kube-proxy's DNAT into the pod
// network reaches the listener). When set, validation must accept the
// non-loopback bind.
func TestValidate_MirrorListenAllowNonLoopbackOptIn(t *testing.T) {
	c := NewDefault()
	c.UpstreamRegistries = []UpstreamRegistry{{Name: "r", Endpoint: "https://r"}}
	c.MirrorListen = "0.0.0.0:5000"

	c.MirrorBindAllowNonLoopback = true
	if err := c.Validate(); err != nil {
		t.Fatalf("validate (opt-in): %v", err)
	}
}

func TestValidate_DuplicateUpstreamName(t *testing.T) {
	c := NewDefault()
	c.UpstreamRegistries = []UpstreamRegistry{
		{Name: "r", Endpoint: "https://r"},
		{Name: "r", Endpoint: "https://r2"},
	}

	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "duplicates") {
		t.Fatalf("want duplicates error, got %v", err)
	}
}

func TestValidate_HRWScope(t *testing.T) {
	c := NewDefault()
	c.UpstreamRegistries = []UpstreamRegistry{{Name: "r", Endpoint: "https://r"}}
	c.HRWTopologyScope = "rack"

	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "hrw_topology_scope") {
		t.Fatalf("want hrw_topology_scope error, got %v", err)
	}
}

// TestValidate_NodeNameRequiresPodName pins the fail-fast
// rule: production K8s mode set via GANTRY_NODE_NAME but without
// GANTRY_POD_NAME is the silent-peer-coordination-failure case the
// reviewer flagged. AnnounceSelf needs PodName as the apiserver patch
// target to publish the gantry.io/peer-id, gantry.io/p2p-addrs, and
// gantry.io/transfer-addr annotations other agents use to translate
// our node-name into a dialable peer ID. Without those, the pod is in
// HRW membership but unreachable, and every Coord.PleasePull /
// PullIntentQuery RPC to it 503s silently. There is no fallback
// peer-ID-mapping mechanism - static bootstrap peers don't help.
func TestValidate_NodeNameRequiresPodName(t *testing.T) {
	c := NewDefault()
	c.UpstreamRegistries = []UpstreamRegistry{{Name: "r", Endpoint: "https://r"}}
	c.NodeName = "ip-10-0-0-7"
	// PodName intentionally left empty.
	err := c.Validate()
	if err == nil {
		t.Fatalf("validate: want error, got nil")
	}

	if !strings.Contains(err.Error(), "pod_name") || !strings.Contains(err.Error(), "node_name") {
		t.Fatalf("validate: error must mention both node_name and pod_name; got %v", err)
	}
}

// TestValidate_PodNameWithoutNodeNameOK confirms the inverse is
// allowed: a Config with PodName but no NodeName isn't useful in
// production but is occasionally used in local kubelet-less tests
// (the membership informer simply won't construct). The check is
// strictly directional: NodeName without PodName, not PodName
// without NodeName.
func TestValidate_PodNameWithoutNodeNameOK(t *testing.T) {
	c := NewDefault()
	c.UpstreamRegistries = []UpstreamRegistry{{Name: "r", Endpoint: "https://r"}}

	c.PodName = "gantry-abc12"
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

// TestValidate_FullProdTripleOK confirms the canonical DaemonSet
// wiring (all three Downward API vars set) passes validation.
func TestValidate_FullProdTripleOK(t *testing.T) {
	c := NewDefault()
	c.UpstreamRegistries = []UpstreamRegistry{{Name: "r", Endpoint: "https://r"}}
	c.NodeName = "ip-10-0-0-7"
	c.PodName = "gantry-abc12"

	c.MembersNamespace = "gantry-system"
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

// TestValidate_NodeNameAndPodNameRequireMembersNamespace pins the
// the fail-fast rule: production K8s mode set via
// GANTRY_NODE_NAME + GANTRY_POD_NAME but WITHOUT
// GANTRY_MEMBERS_NAMESPACE is the stuck-unready case the reviewer
// flagged. selfAnnounceRequiredForReadiness gates /readyz on a
// successful AnnounceSelf, but members.AnnounceSelf refuses to run
// when Options.Namespace == "" because Pods(ns).Patch needs a
// concrete namespace - cluster-wide list/watch cannot self-patch.
// Without this validation the agent boots cleanly, runs forever,
// and never goes ready, with the only signal being a recurring
// "AnnounceSelf requires Options.Namespace" log line.
func TestValidate_NodeNameAndPodNameRequireMembersNamespace(t *testing.T) {
	c := NewDefault()
	c.UpstreamRegistries = []UpstreamRegistry{{Name: "r", Endpoint: "https://r"}}
	c.NodeName = "ip-10-0-0-7"
	c.PodName = "gantry-abc12"
	// MembersNamespace intentionally left empty.
	err := c.Validate()
	if err == nil {
		t.Fatalf("validate: want error, got nil")
	}

	if !strings.Contains(err.Error(), "members_namespace") {
		t.Fatalf("validate: error must mention members_namespace; got %v", err)
	}
	// Must NOT alias the node-name-without-pod-name message; the two
	// production-mode checks have distinct remediation paths and we
	// want operators to read the right one.
	if strings.Contains(err.Error(), "pod_name is empty") {
		t.Fatalf("validate: error wrongly matched the pod_name check: %v", err)
	}
}

// TestValidate_PodNameOnlyDoesNotRequireMembersNamespace mirrors the
// PodName-without-NodeName carve-out from
// TestValidate_PodNameWithoutNodeNameOK: a Config with only PodName
// set is dev-mode and the AnnounceSelf path isn't engaged because
// production-mode gating in cmd/gantry needs NodeName too. The
// new members_namespace check MUST share that directionality.
func TestValidate_PodNameOnlyDoesNotRequireMembersNamespace(t *testing.T) {
	c := NewDefault()
	c.UpstreamRegistries = []UpstreamRegistry{{Name: "r", Endpoint: "https://r"}}
	c.PodName = "gantry-abc12"
	// NodeName + MembersNamespace intentionally left empty.
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

// TestValidate_DevModeAllEmptyOK confirms dev mode (no Downward API
// envs) still passes validation. The codepath downstream falls back
// to a single-self members stub and disables cold-start coordination.
func TestValidate_DevModeAllEmptyOK(t *testing.T) {
	c := NewDefault()

	c.UpstreamRegistries = []UpstreamRegistry{{Name: "r", Endpoint: "https://r"}}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestLoadYAML_KnownFieldsOnly(t *testing.T) {
	c := NewDefault()

	in := []byte("totally_unknown_field: 1\n")
	if err := c.LoadYAML(bytes.NewReader(in)); err == nil {
		t.Fatal("expected unknown-field error")
	}
}

func TestLoadYAML_Roundtrip(t *testing.T) {
	c := NewDefault()

	in := []byte(`
cache_dir: /tmp/gantry-cache
cache_budget_bytes: 12345
upstream_registries:
  - name: registry.example.com
    endpoint: https://registry.example.com
    credentials_path: /etc/gantry/creds.txt
hrw_k: 5
nf5_jitter_base: 7s
log_level: debug
`)
	if err := c.LoadYAML(bytes.NewReader(in)); err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}
	// cache_dir / cache_budget_bytes are accepted as legacy YAML
	// keys only (no Go consumer reads them); they land on
	// LegacyDeprecated so existing ConfigMaps continue to parse.
	if c.LegacyDeprecated.CacheDir != "/tmp/gantry-cache" || c.LegacyDeprecated.CacheBudgetBytes != 12345 {
		t.Errorf("legacy YAML overlay failed: %+v", c.LegacyDeprecated)
	}

	if len(c.UpstreamRegistries) != 1 || c.UpstreamRegistries[0].Name != "registry.example.com" {
		t.Errorf("upstream overlay failed: %+v", c.UpstreamRegistries)
	}

	if c.HRWK != 5 {
		t.Errorf("HRWK = %d, want 5", c.HRWK)
	}

	if c.NF5JitterBase != 7*time.Second {
		t.Errorf("NF5JitterBase = %v, want 7s", c.NF5JitterBase)
	}

	if c.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", c.LogLevel)
	}
}

func TestLoadEnv(t *testing.T) {
	c := NewDefault()
	env := map[string]string{
		// Deprecated env vars are no longer read; setting them
		// here proves they are ignored.
		"GANTRY_CACHE_DIR":          "/etc/gantry/cache",
		"GANTRY_CACHE_BUDGET_BYTES": "7777",
		"GANTRY_HRW_K":              "9",
		"GANTRY_NF5_JITTER_BASE":    "4500ms",
	}

	getenv := func(k string) string { return env[k] }
	if err := c.LoadEnv(getenv); err != nil {
		t.Fatalf("LoadEnv: %v", err)
	}

	if c.LegacyDeprecated.CacheDir != "" {
		t.Errorf("LegacyDeprecated.CacheDir = %q; deprecated env should not write to it", c.LegacyDeprecated.CacheDir)
	}

	if c.LegacyDeprecated.CacheBudgetBytes != 0 {
		t.Errorf("LegacyDeprecated.CacheBudgetBytes = %d; deprecated env should not write to it", c.LegacyDeprecated.CacheBudgetBytes)
	}

	if c.HRWK != 9 {
		t.Errorf("HRWK = %d", c.HRWK)
	}

	if c.NF5JitterBase != 4500*time.Millisecond {
		t.Errorf("NF5JitterBase = %v", c.NF5JitterBase)
	}
}

func TestLoadEnv_RejectsBadDuration(t *testing.T) {
	c := NewDefault()

	getenv := func(k string) string {
		if k == "GANTRY_NF5_JITTER_BASE" {
			return "not-a-duration"
		}

		return ""
	}
	if err := c.LoadEnv(getenv); err == nil {
		t.Fatal("expected duration parse error")
	}
}

func TestResolveUpstream(t *testing.T) {
	c := NewDefault()

	c.UpstreamRegistries = []UpstreamRegistry{
		{Name: "registry.example.com", Endpoint: "https://registry.example.com"},
		{Name: "ghcr.io", Endpoint: "https://ghcr.io", NSAlias: "github"},
	}
	if _, ok := c.ResolveUpstream("registry.example.com"); !ok {
		t.Error("ResolveUpstream(name) miss")
	}

	if _, ok := c.ResolveUpstream("github"); !ok {
		t.Error("ResolveUpstream(alias) miss")
	}

	if _, ok := c.ResolveUpstream("unknown"); ok {
		t.Error("ResolveUpstream(unknown) hit")
	}
}

// removed the gantry-cache storage backend; Validate must
// emit a clear migration error so an operator running an old
// ConfigMap doesn't see a generic "unknown storage_mode" message.
func TestValidate_RejectsLegacyGantryCacheMode(t *testing.T) {
	c := NewDefault()
	c.UpstreamRegistries = []UpstreamRegistry{{Name: "r", Endpoint: "https://r"}}
	c.StorageMode = "gantry-cache"

	err := c.Validate()
	if err == nil {
		t.Fatal("expected validate error for storage_mode=gantry-cache")
	}

	if !strings.Contains(err.Error(), "removed") || !strings.Contains(err.Error(), "containerd") {
		t.Errorf("error %q should mention removal + containerd migration", err)
	}
}

// An empty StorageMode is a misconfiguration after (NewDefault
// sets containerd; an explicit empty string means the operator zeroed
// it deliberately). Validate must catch that, not silently treat it
// as containerd.
func TestValidate_EmptyStorageModeRejected(t *testing.T) {
	c := NewDefault()
	c.UpstreamRegistries = []UpstreamRegistry{{Name: "r", Endpoint: "https://r"}}
	c.StorageMode = ""

	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "storage_mode") {
		t.Fatalf("want storage_mode error, got %v", err)
	}
}

// lease lifecycle requires positive TTL + cleanup interval.
// Zero values would degenerate into either no protection (TTL=0) or
// a tight cleanup loop (interval=0), both of which we want to catch
// at config time, not runtime.
func TestValidate_ContainerdLeaseTTLMustBePositive(t *testing.T) {
	c := NewDefault()
	c.UpstreamRegistries = []UpstreamRegistry{{Name: "r", Endpoint: "https://r"}}
	c.ContainerdLeaseTTL = 0

	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "containerd_lease_ttl") {
		t.Fatalf("want containerd_lease_ttl error, got %v", err)
	}
}

func TestValidate_ContainerdLeaseCleanupIntervalMustBePositive(t *testing.T) {
	c := NewDefault()
	c.UpstreamRegistries = []UpstreamRegistry{{Name: "r", Endpoint: "https://r"}}
	c.ContainerdLeaseCleanupInterval = 0

	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "containerd_lease_cleanup_interval") {
		t.Fatalf("want containerd_lease_cleanup_interval error, got %v", err)
	}
}

// storage_mode=containerd requires a containerd socket. The
// agent has no other way to reach the content store.
func TestValidate_ContainerdModeRequiresSocket(t *testing.T) {
	c := NewDefault()
	c.UpstreamRegistries = []UpstreamRegistry{{Name: "r", Endpoint: "https://r"}}
	c.ContainerdSocket = ""

	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "containerd_socket") {
		t.Fatalf("want containerd_socket error, got %v", err)
	}
}
