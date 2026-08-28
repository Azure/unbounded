// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package config is the single source of truth for every operator-tunable
// knob the Gantry agent exposes.
//
// The design docs enumerate configuration in many places (the design doc cache, the design doc
// direct-origin-fallback / DHT health, the design doc origin-failure circuit breaker, the design doc hosts.toml /
// upstream registries, the design doc open questions). This package collects them into
// a single Config struct with field-level documentation pointing at the
// design-doc citation.
//
// Sources, in increasing precedence:
//
// 1. Built-in defaults from NewDefault.
// 2. YAML file at --config=PATH (optional).
// 3. Environment variables prefixed GANTRY_.
// 4. Command-line flags.
//
// Later sources win. Validate runs after all sources have been merged and
// returns an aggregate of every problem found, not just the first.
package config

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Recognised StorageMode values.
const (
	// StorageModeContainerd routes reads/writes through the local
	// containerd content store (plan). This is the only
	// accepted storage mode; the legacy hostPath "gantry-cache" mode
	// was removed in plan .
	StorageModeContainerd = "containerd"

	// storageModeGantryCache is the legacy hostPath cache mode that
	// was removed in plan . It is referenced only by
	// Validate to surface a clear migration error to operators who
	// still have storage_mode: gantry-cache in their ConfigMap.
	storageModeGantryCache = "gantry-cache"
)

// Config is the typed configuration surface.
//
// Every field carries a yaml/json tag matching the file/env name and a comment
// citing the design-doc section it derives from. Defaults are set by
// NewDefault; see Validate for hard correctness constraints.
type Config struct {
	// ---------- Listeners ----------

	// MirrorListen is the loopback address for containerd's mirror endpoint
	// (the design doc). MUST be loopback-only unless
	// MirrorBindAllowNonLoopback is true (operator opt-in: e.g. when the
	// pod is reached via a hostPort with hostIP=127.0.0.1 DNAT'ing into
	// the pod network - see deploy/gantry/daemonset.yaml).
	MirrorListen string `yaml:"mirror_listen"`

	// MirrorBindAllowNonLoopback relaxes the loopback-only validation on
	// MirrorListen. Set to true ONLY when the deployment guarantees the
	// mirror is unreachable from off-node by some other mechanism - the
	// shipped DaemonSet uses hostPort with hostIP=127.0.0.1, which DNATs
	// the node's loopback into the pod so containerd reaches the mirror
	// over 127.0.0.1 even though the pod itself binds 0.0.0.0. Equivalent
	// alternatives: a host-level firewall blocking the mirror port on
	// non-loopback interfaces, or running the agent in the host network
	// namespace bound to 127.0.0.1. The default (false) is the safe value
	// for any non-Kubernetes deployment. See README.md "Security model"
	// for the full opt-in checklist.
	MirrorBindAllowNonLoopback bool `yaml:"mirror_bind_allow_non_loopback"`

	// TransferListen is the peer-facing HTTP/2 endpoint (the design doc). The bind is
	// typically 0.0.0.0; cluster-internal isolation comes from
	// NetworkPolicy + the `Gantry-Mirrored: 1` request-header gate +
	// digestpipe integrity verification, not from the bind address. See
	// README.md "Security model" for the full threat model - in
	// particular, the endpoint is plaintext h2c by design and assumes
	// the cluster network is trusted. Operators that need on-the-wire
	// confidentiality should layer a service mesh (Istio / Linkerd /
	// Cilium mTLS) above Gantry.
	TransferListen string `yaml:"transfer_listen"`

	// MetricsListen is the Prometheus scrape endpoint and /readyz /livez
	// kubelet-probe target (the design doc). Default is 0.0.0.0:9095 because both
	// Prometheus (off-node) and the kubelet (off-pod, node-IP source)
	// need to reach it - a loopback default would silently break the
	// DaemonSet's readiness gate. Access control belongs in NetworkPolicy
	// and pod ports, not in the bind address.
	MetricsListen string `yaml:"metrics_listen"`

	// PprofListen is an optional Go runtime profiling endpoint. Empty disables
	// profiling. When enabled it must bind loopback so profiles, command lines,
	// and runtime state are reachable only through local access such as
	// `kubectl port-forward`.
	PprofListen string `yaml:"pprof_listen"`

	// Libp2pListen is the multiaddr(s) the libp2p host advertises (the design doc).
	// Empty means "use libp2p defaults" and pick at random.
	Libp2pListen []string `yaml:"libp2p_listen"`

	// Libp2pIdentityPath is the on-disk path of the persisted libp2p key
	// (the design doc). Lost identity is not catastrophic; old DHT records age out.
	Libp2pIdentityPath string `yaml:"libp2p_identity_path"`

	// DHTProtocolPrefix isolates provider records between Gantry networks.
	// Benchmarks override it per run when reusing image digests so stale
	// provider records cannot leak across runs.
	DHTProtocolPrefix string `yaml:"dht_protocol_prefix"`

	// Libp2pBootstrapPeers is an optional list of static multiaddrs to seed
	// the libp2p host's connection set on startup. In production these are
	// usually discovered via the K8s informer (the design doc) so this field defaults
	// to empty; tests and small clusters can use it directly.
	Libp2pBootstrapPeers []string `yaml:"libp2p_bootstrap_peers"`

	// ---------- Node identity ----------

	// NodeName is the Kubernetes node this agent runs on. Sourced via the
	// Downward API (env spec.nodeName) into GANTRY_NODE_NAME. Used to label
	// locally emitted progress metrics.
	NodeName string `yaml:"node_name"`

	// PodIP is the agent's routable Pod IP. Sourced via the Downward
	// API (env status.podIP) into GANTRY_POD_IP. Used to rewrite
	// 0.0.0.0 wildcard listen addresses into dialable advertised
	// addresses so peers can reach this agent; a peer publishing
	// 0.0.0.0:5001 is otherwise unreachable from other pods,
	// defeating libp2p bootstrap on first-cluster boot. Empty when
	// running outside Kubernetes.
	PodIP string `yaml:"pod_ip"`

	// Rendezvous configures bounded first-contact discovery over a fixed
	// set of Lease slots.
	Rendezvous RendezvousConfig `yaml:"rendezvous"`

	// ---------- Storage backend ----------

	// StorageMode selects which backend the agent uses as the read/write
	// content store. Only "containerd" is supported - the legacy
	// "gantry-cache" hostPath backend was removed. The field is
	// retained so existing ConfigMaps that still set it to "containerd"
	// continue to parse, and so a clear migration error surfaces for
	// any operator still on "gantry-cache". It is not exposed as a
	// CLI flag or env var because there is no other valid value to
	// select.
	//
	// Default is "containerd". Exposed via the
	// gantry_storage_mode_info metric for observability.
	StorageMode string `yaml:"storage_mode"`

	// LegacyDeprecated captures YAML fields that used to live on
	// Config but are no longer consumed by any code path. They are
	// kept here purely so existing ConfigMaps that still set
	// `cache_dir`, `cache_budget_bytes`, etc. continue to parse
	// (LoadYAML uses KnownFields=true). New deployments should not
	// set them. A future major version will remove the struct.
	LegacyDeprecated LegacyDeprecatedConfig `yaml:",inline"`

	// ---------- containerd integration (cdsub) ----------

	// ContainerdSocket is the path to the containerd gRPC API socket
	// that the cdsub subsystem dials to discover locally-cached images
	// and announce them on the DHT (the design doc image-event -> Provide loop),
	// that the transfer endpoint reads from on cache miss to serve
	// peers without a re-download, and that the puller writes into on
	// background origin pulls (storage_mode=containerd is the only
	// supported mode; see).
	//
	// REQUIRED. Validate rejects an empty value when
	// storage_mode=containerd (the only accepted storage_mode), which
	// is enforced at startup. The default deploy manifests set it to
	// "/run/containerd/containerd.sock"; operators on non-default
	// socket paths override via `containerd_socket` in the ConfigMap
	// or GANTRY_CONTAINERD_SOCKET in the environment. The agent will
	// fail to start (and readyz will stay 503) if the socket cannot
	// be reached.
	ContainerdSocket string `yaml:"containerd_socket"`

	// ContainerdNamespace is the containerd namespace cdsub watches.
	// Kubelet uses "k8s.io" for pod containers - the default. Set to
	// "moby" for Docker-managed images, or any custom namespace.
	ContainerdNamespace string `yaml:"containerd_namespace"`

	// ContainerdLeaseTTL is the lifetime of containerd content-store
	// leases that Gantry attaches when ingesting blobs. Containerd's
	// garbage collector will not delete content while at least one
	// lease references it, so the TTL governs how long a Gantry-pulled
	// blob is protected before the pod it was pulled for is
	// responsible for keeping it alive via its own image reference.
	// Plan range: 30m–120m. Default 60m.
	ContainerdLeaseTTL time.Duration `yaml:"containerd_lease_ttl"`

	// ContainerdLeaseCleanupInterval is the period at which the agent
	// scans containerd's lease catalogue and deletes expired
	// Gantry-owned leases. Defaults to ContainerdLeaseTTL/2 if zero.
	ContainerdLeaseCleanupInterval time.Duration `yaml:"containerd_lease_cleanup_interval"`

	// ---------- Upstream registries ----------

	// UpstreamRegistries enumerates every OCI registry the agent mirrors
	// (the design doc, the design doc). The agent rejects requests whose ?ns= does not match
	// one of these once more than one is configured.
	UpstreamRegistries []UpstreamRegistry `yaml:"upstream_registries"`

	// ---------- Puller selection / coordination ----------

	// TopK is how many closest-peer candidates the cold-start probe
	// contacts before expanding. Default 3.
	TopK int `yaml:"top_k"`

	// PrefetchPullerReplicas is how many distinct ranked pullers each
	// prefetched layer digest is dispatched to. 1 designates a single origin
	// puller per layer (tightest dedup), but the whole swarm then fans out
	// from ONE initial seed, which bottlenecks a cold thundering-herd (peer
	// transfers pile onto the lone seed and stall). N>1 asks the top-N pullers
	// to origin-pull the layer in parallel, giving N initial seeds so peer
	// transfers fan out N-fold, at the cost of up to N origin copies of each
	// layer. The default is 8.
	PrefetchPullerReplicas int `yaml:"prefetch_puller_replicas"`

	// PrefetchCoordinatorReplicas limits remote speculative prefetch dispatch
	// to a deterministic ranked subset of manifest consumers. Local
	// self-selected pulls still run on every consumer. The default is 3.
	PrefetchCoordinatorReplicas int `yaml:"prefetch_coordinator_replicas"`

	// PrefetchMaxConcurrentGroups caps simultaneous outbound prefetch groups
	// per manifest. Group dispatch is best effort and target-side deduplicated.
	PrefetchMaxConcurrentGroups int `yaml:"prefetch_max_concurrent_groups"`

	// PrefetchDispatchJitter spreads manifest prefetch across requesters. Each
	// node derives a stable delay in [0, jitter) from itself and the manifest.
	PrefetchDispatchJitter time.Duration `yaml:"prefetch_dispatch_jitter"`

	// CoordMaxDigestsPerRequest caps a single please_pull batch. The default
	// 256 is intentionally far above normal manifest child counts while staying
	// well below the 1 MiB coord envelope budget.
	CoordMaxDigestsPerRequest int `yaml:"coord_max_digests_per_request"`

	// CoordMaxConcurrentPulls caps background origin pulls started by inbound
	// please_pull. Each pull holds an HTTP response body, a containerd writer,
	// goroutine state, and a lease, so this protects the node from fan-out.
	CoordMaxConcurrentPulls int `yaml:"coord_max_concurrent_pulls"`

	// PeerFetchTimeout caps the complete peer request, including streaming and
	// committing the response body. The default is 15m so progressing large
	// layers are not forced to switch providers at a size-dependent throughput
	// threshold. The HTTP/2 transport separately probes connections after 10s
	// without reads, so dead connections are detected before this safety ceiling.
	// Live stream-through still preserves a verified prefix if the ceiling fires.
	PeerFetchTimeout time.Duration `yaml:"peer_fetch_timeout"`

	// PeerMaxAttempts caps the shuffled DHT providers tried in one discovery
	// round. The default is 20, matching kad-dht's bounded provider result set.
	PeerMaxAttempts int `yaml:"peer_max_attempts"`

	// PeerRediscoverBudget bounds re-discovery after ordinary empty or failed
	// peer rounds. All-busy rounds are capacity pressure, not failure: they
	// honor Retry-After and keep retrying with bounded jitter until peer
	// progress or client cancellation so fail-open containerd does not bypass
	// Gantry merely because live providers are saturated.
	PeerRediscoverBudget time.Duration `yaml:"peer_rediscover_budget"`

	// PeerRediscoverBackoff is the pause between re-discovery rounds. It gives
	// newly-finished seeds time to advertise into the DHT before the next
	// FindProviders. Zero uses a built-in default (1s) when re-discovery is
	// enabled. Ignored when PeerRediscoverBudget is zero.
	PeerRediscoverBackoff time.Duration `yaml:"peer_rediscover_backoff"`

	// TransferMaxConcurrentServes caps concurrent peer blob-body serves on the
	// transfer endpoint. Requests over the cap receive 429 with a Retry-After
	// hint so the requester re-discovers another provider instead of queueing
	// behind a saturated seed. This load-shedding is what lets the first
	// finishers complete early and seed the swarm. Zero means unlimited; the
	// default is 10. Busy responses stay inside Gantry even with a fail-open
	// containerd hosts chain; hard Gantry failures still reach the next host.
	TransferMaxConcurrentServes int `yaml:"transfer_max_concurrent_serves"`

	// AdvertiseReconcileInterval is the cadence of the background
	// advertiser's full inventory reconcile. Eager per-digest advertise on
	// stream completion is the fast path that makes finishers discoverable
	// within milliseconds; this reconcile is the backstop that catches
	// missed events and drives withdraws. Lower values converge faster
	// after a missed event at the cost of more periodic inventory scans.
	AdvertiseReconcileInterval time.Duration `yaml:"advertise_reconcile_interval"`

	// ---------- DHT / direct-origin-fallback ----------

	// NF5JitterBase is the base delay in the direct-origin-fallback jitter window
	// `[0, base * ln(N))` (the design doc default 3 s).
	NF5JitterBase time.Duration `yaml:"nf5_jitter_base"`

	// NF5JitterCap is a hard ceiling on the computed jitter window.
	// Zero means no cap (original behaviour). Set this to bound worst-case
	// cold-start latency on large clusters: at N=300 the uncapped window is
	// ~17s; a cap of 10s limits the maximum additional delay imposed by NF5
	// regardless of cluster size. The configured NF5JitterBase still
	// controls the shape of the distribution up to the cap.
	NF5JitterCap time.Duration `yaml:"nf5_jitter_cap"`

	// NF5PerNodeRateLimit is the per-node direct-origin fallback rate
	// (token bucket, fallbacks/minute; the design doc default 2).
	NF5PerNodeRateLimit int `yaml:"nf5_per_node_rate_limit"`

	// BootstrapWindow is the time after startup during which DHT-empty is
	// not trusted as cold-start evidence (the design doc default 30 s).
	BootstrapWindow time.Duration `yaml:"bootstrap_window"`

	// BootstrapRoutingTablePct is the routing-table-size threshold that
	// supersedes BootstrapWindow once met (the design doc default 25%).
	BootstrapRoutingTablePct int `yaml:"bootstrap_routing_table_pct"`

	// TopKExpansionFactorDegraded is the multiplier applied to TopK when
	// expanding top-K under Degraded health (the step 5 / the design doc default 2).
	TopKExpansionFactorDegraded int `yaml:"topk_expansion_factor_degraded"`

	// ---------- Origin-failure circuit breaker (the design doc) ----------

	OriginFailureCooldownInitial    time.Duration `yaml:"origin_failure_cooldown_initial"`
	OriginFailureCooldownMax        time.Duration `yaml:"origin_failure_cooldown_max"`
	OriginFailureCooldownMultiplier int           `yaml:"origin_failure_cooldown_multiplier"`
	OriginFailureHonorWindowCap     time.Duration `yaml:"origin_failure_honor_window_cap"`

	// OriginFailureClassesTrustedClusterWide controls which the design doc failure
	// classes are propagated cluster-wide as 5xx-immediate (default
	// {auth, not_found, rate_limited}; `transient` is honored locally
	// only).
	OriginFailureClassesTrustedClusterWide []string `yaml:"origin_failure_classes_trusted_cluster_wide"`

	// ---------- Logging ----------

	// LogLevel is one of "debug", "info", "warn", "error".
	LogLevel string `yaml:"log_level"`

	// LogFormat is "json" (production) or "text" (development).
	LogFormat string `yaml:"log_format"`
}

// UpstreamRegistry describes one OCI registry the agent mirrors.
type UpstreamRegistry struct {
	// Name is the canonical identifier (used as the ?ns= value from
	// containerd and as the lookup key for credentials).
	Name string `yaml:"name"`

	// Endpoint is the HTTPS URL of the registry, e.g.
	// "https://registry.example.com".
	Endpoint string `yaml:"endpoint"`

	// CredentialsPath is an optional fallback file containing registry
	// credentials. Format: "username:password" (or "_json_key:<json>" for
	// the well-known GCR pattern). A request-scoped Basic/Bearer credential
	// delegated by containerd takes precedence and is never cached. Setting
	// this file opts the registry into legacy shared-identity mode for requests
	// without delegated auth; leaving it empty enables containerd challenge
	// negotiation for private HTTPS registries.
	CredentialsPath string `yaml:"credentials_path"`

	// NSAlias lets containerd's ?ns= use a different name than Name.
	// Empty means ?ns= must equal Name.
	NSAlias string `yaml:"ns_alias"`
}

// LegacyDeprecatedConfig captures YAML field names that used to live
// on Config but no longer have any effect. They are accepted here
// purely so existing ConfigMaps that still set them parse without
// error under KnownFields=true. None of these fields are exposed via
// CLI flags or environment variables - setting them via env/flag is
// not supported, only YAML round-trips for back-compat. A future
// major version will remove this struct entirely.
//
// Removal trail :
// - cache_dir / cache_budget_bytes / cache_forced_eviction_headroom_pct
// / eviction_provider_count_threshold:
// the hostPath cache backend was deleted; containerd's own GC owns
// blob lifetime now.
type LegacyDeprecatedConfig struct {
	CacheDir                       string `yaml:"cache_dir,omitempty"`
	CacheBudgetBytes               int64  `yaml:"cache_budget_bytes,omitempty"`
	CacheForcedEvictionHeadroomPct int    `yaml:"cache_forced_eviction_headroom_pct,omitempty"`
	EvictionProviderCountThreshold int    `yaml:"eviction_provider_count_threshold,omitempty"`
}

// RendezvousConfig configures the fixed Lease key space. Defaults are
// validation values and remain operator-tunable until scale measurements select
// production values.
type RendezvousConfig struct {
	Namespace              string        `yaml:"namespace"`
	Kubeconfig             string        `yaml:"kubeconfig"`
	SlotCount              int           `yaml:"slot_count"`
	ReadsPerRound          int           `yaml:"reads_per_round"`
	ClaimAttemptsPerRound  int           `yaml:"claim_attempts_per_round"`
	ContactsPerSlot        int           `yaml:"contacts_per_slot"`
	FullScanAfter          int           `yaml:"full_scan_after"`
	RoutingTableMin        int           `yaml:"routing_table_min"`
	FallbackNodeUpperBound int           `yaml:"fallback_node_upper_bound"`
	LeaseDuration          time.Duration `yaml:"lease_duration"`
	RenewInterval          time.Duration `yaml:"renew_interval"`
	StaleContactGrace      time.Duration `yaml:"stale_contact_grace"`
	RetryMin               time.Duration `yaml:"retry_min"`
	RetryMax               time.Duration `yaml:"retry_max"`
	SingleNode             bool          `yaml:"single_node"`
	PeerCachePath          string        `yaml:"peer_cache_path"`
}

// NewDefault returns a Config populated with the design-doc defaults.
// All fields are set; Validate against this MUST pass.
func NewDefault() *Config {
	return &Config{
		MirrorListen:               "127.0.0.1:5000",
		MirrorBindAllowNonLoopback: false,
		TransferListen:             "0.0.0.0:5001",
		MetricsListen:              "0.0.0.0:9095",
		PprofListen:                "",
		Libp2pListen:               nil,
		Libp2pIdentityPath:         "/var/lib/gantry/libp2p.key",
		DHTProtocolPrefix:          "/gantry",

		NodeName: "",
		Rendezvous: RendezvousConfig{
			SlotCount:              64,
			ReadsPerRound:          8,
			ClaimAttemptsPerRound:  4,
			ContactsPerSlot:        8,
			FullScanAfter:          3,
			RoutingTableMin:        1,
			FallbackNodeUpperBound: 100000,
			LeaseDuration:          90 * time.Second,
			RenewInterval:          30 * time.Second,
			StaleContactGrace:      5 * time.Minute,
			RetryMin:               time.Second,
			RetryMax:               30 * time.Second,
			// A bare run with no ConfigMap and no environment is a
			// single-node agent. The DaemonSet force-injects false.
			SingleNode:    true,
			PeerCachePath: "/var/lib/gantry/bootstrap-peers.json",
		},

		StorageMode: StorageModeContainerd,

		ContainerdSocket:               "/run/containerd/containerd.sock",
		ContainerdNamespace:            "k8s.io",
		ContainerdLeaseTTL:             60 * time.Minute,
		ContainerdLeaseCleanupInterval: 30 * time.Minute,

		UpstreamRegistries: nil,

		TopK:                        3,
		PrefetchPullerReplicas:      8,
		PrefetchCoordinatorReplicas: 3,
		PrefetchMaxConcurrentGroups: 64,
		PrefetchDispatchJitter:      time.Second,

		CoordMaxDigestsPerRequest:   256,
		CoordMaxConcurrentPulls:     16,
		PeerFetchTimeout:            15 * time.Minute,
		PeerMaxAttempts:             20,
		PeerRediscoverBudget:        5 * time.Minute, // re-discovery cascade on by default (validated at 300 nodes)
		PeerRediscoverBackoff:       time.Second,     // pause between re-discovery rounds
		TransferMaxConcurrentServes: 10,              // serve cap preserves bandwidth per large-layer stream
		AdvertiseReconcileInterval:  time.Minute,

		NF5JitterBase:               3 * time.Second,
		NF5JitterCap:                5 * time.Minute,
		NF5PerNodeRateLimit:         2,
		BootstrapWindow:             30 * time.Second,
		BootstrapRoutingTablePct:    25,
		TopKExpansionFactorDegraded: 2,

		OriginFailureCooldownInitial:    10 * time.Second,
		OriginFailureCooldownMax:        10 * time.Minute,
		OriginFailureCooldownMultiplier: 3,
		OriginFailureHonorWindowCap:     30 * time.Second,
		OriginFailureClassesTrustedClusterWide: []string{
			"auth", "not_found", "rate_limited",
		},

		LogLevel:  "info",
		LogFormat: "json",
	}
}

// LoadYAML overlays a YAML document onto c. Removed membership keys are
// discarded so operator-retained ConfigMaps survive the direct cutover;
// every other unknown field remains an error.
func (c *Config) LoadYAML(r io.Reader) error {
	var document yaml.Node
	if err := yaml.NewDecoder(r).Decode(&document); err != nil {
		return err
	}

	stripLegacyConfigFields(&document)

	cleaned, err := yaml.Marshal(&document)
	if err != nil {
		return fmt.Errorf("marshal cleaned config: %w", err)
	}

	dec := yaml.NewDecoder(bytes.NewReader(cleaned))
	dec.KnownFields(true)

	return dec.Decode(c)
}

func stripLegacyConfigFields(document *yaml.Node) {
	if document == nil || len(document.Content) == 0 {
		return
	}

	legacyTopLevel := map[string]struct{}{
		"pod_name":                 {},
		"members_namespace":        {},
		"members_label_selector":   {},
		"members_kubeconfig":       {},
		"members_sync_timeout":     {},
		"hrw_k":                    {},
		"hrw_topology_scope":       {},
		"zone_label_key":           {},
		"prefetch_puller_fraction": {},
		"coord_peer_authz_enforce": {},
	}

	root := document.Content[0]
	removeMappingKeys(root, legacyTopLevel)

	for index := 0; index+1 < len(root.Content); index += 2 {
		if root.Content[index].Value == "rendezvous" {
			removeMappingKeys(root.Content[index+1], map[string]struct{}{"mode": {}})
			return
		}
	}
}

func removeMappingKeys(mapping *yaml.Node, keys map[string]struct{}) {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return
	}

	content := mapping.Content[:0]
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if _, remove := keys[mapping.Content[index].Value]; remove {
			continue
		}

		content = append(content, mapping.Content[index], mapping.Content[index+1])
	}

	mapping.Content = content
}

// LoadEnv overlays environment variables of the form GANTRY_<UPPER_SNAKE>.
// Returns a multi-error of any parse failures encountered.
//
// Only scalar fields are overlaid here; list fields (UpstreamRegistries,
// Libp2pListen, OriginFailureClassesTrustedClusterWide) are file-only by
// design - env vars are an awkward shape for them.
func (c *Config) LoadEnv(env func(string) string) error {
	var errs []error

	setStr := func(key string, dst *string) {
		if v, ok := lookup(env, key); ok {
			*dst = v
		}
	}
	setInt := func(key string, dst *int) {
		if v, ok := lookup(env, key); ok {
			n, err := strconv.Atoi(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("env GANTRY_%s: %w", key, err))
				return
			}

			*dst = n
		}
	}
	setDur := func(key string, dst *time.Duration) {
		if v, ok := lookup(env, key); ok {
			d, err := time.ParseDuration(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("env GANTRY_%s: %w", key, err))
				return
			}

			*dst = d
		}
	}
	setBool := func(key string, dst *bool) {
		if v, ok := lookup(env, key); ok {
			b, err := strconv.ParseBool(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("env GANTRY_%s: %w", key, err))
				return
			}

			*dst = b
		}
	}

	setStr("MIRROR_LISTEN", &c.MirrorListen)
	setBool("MIRROR_BIND_ALLOW_NON_LOOPBACK", &c.MirrorBindAllowNonLoopback)
	setStr("TRANSFER_LISTEN", &c.TransferListen)
	setStr("METRICS_LISTEN", &c.MetricsListen)
	setStr("PPROF_LISTEN", &c.PprofListen)
	setStr("LIBP2P_IDENTITY_PATH", &c.Libp2pIdentityPath)
	setStr("DHT_PROTOCOL_PREFIX", &c.DHTProtocolPrefix)

	setStr("NODE_NAME", &c.NodeName)
	setStr("POD_IP", &c.PodIP)
	setStr("RENDEZVOUS_NAMESPACE", &c.Rendezvous.Namespace)
	setStr("RENDEZVOUS_KUBECONFIG", &c.Rendezvous.Kubeconfig)
	setInt("RENDEZVOUS_SLOT_COUNT", &c.Rendezvous.SlotCount)
	setInt("RENDEZVOUS_READS_PER_ROUND", &c.Rendezvous.ReadsPerRound)
	setInt("RENDEZVOUS_CLAIM_ATTEMPTS_PER_ROUND", &c.Rendezvous.ClaimAttemptsPerRound)
	setInt("RENDEZVOUS_CONTACTS_PER_SLOT", &c.Rendezvous.ContactsPerSlot)
	setInt("RENDEZVOUS_FULL_SCAN_AFTER", &c.Rendezvous.FullScanAfter)
	setInt("RENDEZVOUS_ROUTING_TABLE_MIN", &c.Rendezvous.RoutingTableMin)
	setInt("RENDEZVOUS_FALLBACK_NODE_UPPER_BOUND", &c.Rendezvous.FallbackNodeUpperBound)
	setDur("RENDEZVOUS_LEASE_DURATION", &c.Rendezvous.LeaseDuration)
	setDur("RENDEZVOUS_RENEW_INTERVAL", &c.Rendezvous.RenewInterval)
	setDur("RENDEZVOUS_STALE_CONTACT_GRACE", &c.Rendezvous.StaleContactGrace)
	setDur("RENDEZVOUS_RETRY_MIN", &c.Rendezvous.RetryMin)
	setDur("RENDEZVOUS_RETRY_MAX", &c.Rendezvous.RetryMax)
	setBool("RENDEZVOUS_SINGLE_NODE", &c.Rendezvous.SingleNode)
	setStr("RENDEZVOUS_PEER_CACHE_PATH", &c.Rendezvous.PeerCachePath)

	// Deprecated env vars (GANTRY_CACHE_DIR, GANTRY_CACHE_BUDGET_BYTES,
	// GANTRY_CACHE_FORCED_EVICTION_HEADROOM_PCT,
	// GANTRY_EVICTION_PROVIDER_COUNT_THRESHOLD, GANTRY_STORAGE_MODE)
	// are no longer read. The fields they used to write to are either
	// removed (cache_*) or no longer operator-tunable (storage_mode is
	// fixed to "containerd" - see Validate). Existing ConfigMaps that
	// still set them in YAML continue to parse via
	// LegacyDeprecatedConfig.

	setStr("CONTAINERD_SOCKET", &c.ContainerdSocket)
	setStr("CONTAINERD_NAMESPACE", &c.ContainerdNamespace)
	setDur("CONTAINERD_LEASE_TTL", &c.ContainerdLeaseTTL)
	setDur("CONTAINERD_LEASE_CLEANUP_INTERVAL", &c.ContainerdLeaseCleanupInterval)

	setInt("TOP_K", &c.TopK)
	setInt("PREFETCH_PULLER_REPLICAS", &c.PrefetchPullerReplicas)
	setInt("PREFETCH_COORDINATOR_REPLICAS", &c.PrefetchCoordinatorReplicas)
	setInt("PREFETCH_MAX_CONCURRENT_GROUPS", &c.PrefetchMaxConcurrentGroups)
	setDur("PREFETCH_DISPATCH_JITTER", &c.PrefetchDispatchJitter)
	setInt("COORD_MAX_DIGESTS_PER_REQUEST", &c.CoordMaxDigestsPerRequest)
	setInt("COORD_MAX_CONCURRENT_PULLS", &c.CoordMaxConcurrentPulls)
	setDur("PEER_FETCH_TIMEOUT", &c.PeerFetchTimeout)
	setInt("PEER_MAX_ATTEMPTS", &c.PeerMaxAttempts)
	setDur("PEER_REDISCOVER_BUDGET", &c.PeerRediscoverBudget)
	setDur("PEER_REDISCOVER_BACKOFF", &c.PeerRediscoverBackoff)
	setInt("TRANSFER_MAX_CONCURRENT_SERVES", &c.TransferMaxConcurrentServes)
	setDur("ADVERTISE_RECONCILE_INTERVAL", &c.AdvertiseReconcileInterval)

	setDur("NF5_JITTER_BASE", &c.NF5JitterBase)
	setDur("NF5_JITTER_CAP", &c.NF5JitterCap)
	setInt("NF5_PER_NODE_RATE_LIMIT", &c.NF5PerNodeRateLimit)
	setDur("BOOTSTRAP_WINDOW", &c.BootstrapWindow)
	setInt("BOOTSTRAP_ROUTING_TABLE_PCT", &c.BootstrapRoutingTablePct)
	setInt("TOPK_EXPANSION_FACTOR_DEGRADED", &c.TopKExpansionFactorDegraded)

	setDur("ORIGIN_FAILURE_COOLDOWN_INITIAL", &c.OriginFailureCooldownInitial)
	setDur("ORIGIN_FAILURE_COOLDOWN_MAX", &c.OriginFailureCooldownMax)
	setInt("ORIGIN_FAILURE_COOLDOWN_MULTIPLIER", &c.OriginFailureCooldownMultiplier)
	setDur("ORIGIN_FAILURE_HONOR_WINDOW_CAP", &c.OriginFailureHonorWindowCap)

	setStr("LOG_LEVEL", &c.LogLevel)
	setStr("LOG_FORMAT", &c.LogFormat)

	return errors.Join(errs...)
}

// BindFlags registers command-line flags on fs that overlay c. Call after
// LoadYAML / LoadEnv but before fs.Parse so flags win.
func (c *Config) BindFlags(fs *flag.FlagSet) {
	fs.StringVar(&c.MirrorListen, "mirror-listen", c.MirrorListen, "address for the containerd-facing mirror endpoint (loopback)")
	fs.BoolVar(&c.MirrorBindAllowNonLoopback, "mirror-bind-allow-non-loopback", c.MirrorBindAllowNonLoopback, "opt in to a non-loopback mirror bind (e.g. when using hostPort + hostIP=127.0.0.1 in Kubernetes)")
	fs.StringVar(&c.TransferListen, "transfer-listen", c.TransferListen, "address for the peer-facing transfer endpoint")
	fs.StringVar(&c.MetricsListen, "metrics-listen", c.MetricsListen, "address for the Prometheus metrics endpoint")
	fs.StringVar(&c.PprofListen, "pprof-listen", c.PprofListen, "optional loopback address for Go runtime profiles (empty disables pprof)")
	fs.StringVar(&c.Libp2pIdentityPath, "libp2p-identity-path", c.Libp2pIdentityPath, "path to the persisted libp2p identity key")
	fs.StringVar(&c.DHTProtocolPrefix, "dht-protocol-prefix", c.DHTProtocolPrefix, "kad-dht protocol prefix used to isolate provider records")

	fs.StringVar(&c.NodeName, "node-name", c.NodeName, "Kubernetes node name this agent runs on (Downward API spec.nodeName)")
	fs.StringVar(&c.PodIP, "pod-ip", c.PodIP, "Kubernetes pod IP of this agent (Downward API status.podIP); used to rewrite 0.0.0.0 listeners into dialable advertised addresses")
	fs.StringVar(&c.Rendezvous.Namespace, "rendezvous-namespace", c.Rendezvous.Namespace, "namespace containing fixed Gantry rendezvous Lease slots")
	fs.StringVar(&c.Rendezvous.Kubeconfig, "rendezvous-kubeconfig", c.Rendezvous.Kubeconfig, "optional kubeconfig for Lease rendezvous")
	fs.IntVar(&c.Rendezvous.SlotCount, "rendezvous-slot-count", c.Rendezvous.SlotCount, "fixed number of precreated Lease slots")
	fs.IntVar(&c.Rendezvous.ReadsPerRound, "rendezvous-reads-per-round", c.Rendezvous.ReadsPerRound, "maximum exact Lease GETs in a normal discovery round")
	fs.IntVar(&c.Rendezvous.ClaimAttemptsPerRound, "rendezvous-claim-attempts-per-round", c.Rendezvous.ClaimAttemptsPerRound, "maximum slot claim attempts per round")
	fs.IntVar(&c.Rendezvous.ContactsPerSlot, "rendezvous-contacts-per-slot", c.Rendezvous.ContactsPerSlot, "maximum holder and sampled contacts accepted from one slot")
	fs.IntVar(&c.Rendezvous.FullScanAfter, "rendezvous-full-scan-after", c.Rendezvous.FullScanAfter, "discovery round cadence for scanning every fixed slot")
	fs.IntVar(&c.Rendezvous.RoutingTableMin, "rendezvous-routing-table-min", c.Rendezvous.RoutingTableMin, "minimum DHT routing-table size used by health monitoring")
	fs.IntVar(&c.Rendezvous.FallbackNodeUpperBound, "rendezvous-fallback-node-upper-bound", c.Rendezvous.FallbackNodeUpperBound, "configured upper bound used only to size capped direct-origin fallback jitter")
	fs.DurationVar(&c.Rendezvous.LeaseDuration, "rendezvous-lease-duration", c.Rendezvous.LeaseDuration, "duration published by a slot holder")
	fs.DurationVar(&c.Rendezvous.RenewInterval, "rendezvous-renew-interval", c.Rendezvous.RenewInterval, "slot holder renewal interval")
	fs.DurationVar(&c.Rendezvous.StaleContactGrace, "rendezvous-stale-contact-grace", c.Rendezvous.StaleContactGrace, "bounded grace for dialing an expired holder hint")
	fs.DurationVar(&c.Rendezvous.RetryMin, "rendezvous-retry-min", c.Rendezvous.RetryMin, "minimum bootstrap retry delay")
	fs.DurationVar(&c.Rendezvous.RetryMax, "rendezvous-retry-max", c.Rendezvous.RetryMax, "maximum bootstrap retry delay")
	fs.BoolVar(&c.Rendezvous.SingleNode, "rendezvous-single-node", c.Rendezvous.SingleNode, "allow readiness with an empty DHT routing table")
	fs.StringVar(&c.Rendezvous.PeerCachePath, "rendezvous-peer-cache-path", c.Rendezvous.PeerCachePath, "host-persisted bootstrap peer cache path")

	// Deprecated cache flags (--cache-dir, --cache-budget-bytes,
	// --cache-forced-eviction-headroom-pct,
	// --eviction-provider-count-threshold) and --storage-mode were
	// removed in plan . The cache fields are no-ops under
	// containerd-only storage; storage_mode itself is no longer an
	// operator knob because "containerd" is the only accepted value
	// (Validate enforces it). YAML-only back-compat for these names
	// lives in Config.StorageMode and Config.LegacyDeprecated.

	fs.StringVar(&c.ContainerdSocket, "containerd-socket", c.ContainerdSocket, "containerd gRPC socket path (REQUIRED; storage_mode=containerd is the only supported mode and Validate() rejects an empty value)")
	fs.StringVar(&c.ContainerdNamespace, "containerd-namespace", c.ContainerdNamespace, "containerd namespace cdsub watches (default k8s.io)")
	fs.DurationVar(&c.ContainerdLeaseTTL, "containerd-lease-ttl", c.ContainerdLeaseTTL, "TTL for containerd content leases attached by Gantry on ingest (storage_mode=containerd only)")
	fs.DurationVar(&c.ContainerdLeaseCleanupInterval, "containerd-lease-cleanup-interval", c.ContainerdLeaseCleanupInterval, "period of the expired-lease sweep loop (storage_mode=containerd only)")

	fs.IntVar(&c.TopK, "top-k", c.TopK, "closest-peer probe fan-out size")
	fs.IntVar(&c.PrefetchPullerReplicas, "prefetch-puller-replicas", c.PrefetchPullerReplicas, "number of closest-peer pullers each prefetched layer digest is pulled by; 1 = tightest dedup, N = N-fold peer fan-out at N origin copies")
	fs.IntVar(&c.PrefetchCoordinatorReplicas, "prefetch-coordinator-replicas", c.PrefetchCoordinatorReplicas, "number of deterministic manifest consumers allowed to dispatch remote speculative prefetch groups")
	fs.IntVar(&c.PrefetchMaxConcurrentGroups, "prefetch-max-concurrent-groups", c.PrefetchMaxConcurrentGroups, "maximum simultaneous outbound prefetch RPC groups per manifest")
	fs.DurationVar(&c.PrefetchDispatchJitter, "prefetch-dispatch-jitter", c.PrefetchDispatchJitter, "maximum deterministic per-node delay before dispatching manifest prefetch")
	fs.IntVar(&c.CoordMaxDigestsPerRequest, "coord-max-digests-per-request", c.CoordMaxDigestsPerRequest, "maximum digests accepted in one please_pull batch")
	fs.IntVar(&c.CoordMaxConcurrentPulls, "coord-max-concurrent-pulls", c.CoordMaxConcurrentPulls, "maximum background origin pulls started by inbound please_pull")
	fs.DurationVar(&c.PeerFetchTimeout, "peer-fetch-timeout", c.PeerFetchTimeout, "maximum time for a complete peer fetch, including body transfer and commit")
	fs.IntVar(&c.PeerMaxAttempts, "peer-max-attempts", c.PeerMaxAttempts, "maximum shuffled DHT providers tried in one discovery round")
	fs.DurationVar(&c.PeerRediscoverBudget, "peer-rediscover-budget", c.PeerRediscoverBudget, "total wall-clock budget for the peer re-discovery loop (0 disables re-discovery, restoring the single-shot provider attempt)")
	fs.DurationVar(&c.PeerRediscoverBackoff, "peer-rediscover-backoff", c.PeerRediscoverBackoff, "pause between peer re-discovery rounds (0 uses the built-in 1s default when re-discovery is enabled)")
	fs.IntVar(&c.TransferMaxConcurrentServes, "transfer-max-concurrent-serves", c.TransferMaxConcurrentServes, "cap on concurrent peer blob-body serves (over the cap returns 429; 0 = unlimited)")
	fs.DurationVar(&c.AdvertiseReconcileInterval, "advertise-reconcile-interval", c.AdvertiseReconcileInterval, "cadence of the background DHT advertiser inventory reconcile (backstop; eager advertise handles the fast path)")

	fs.DurationVar(&c.NF5JitterBase, "nf5-jitter-base", c.NF5JitterBase, "base delay for the NF5 jitter window")
	fs.DurationVar(&c.NF5JitterCap, "nf5-jitter-cap", c.NF5JitterCap, "hard ceiling on the NF5 jitter window (0 = no cap)")
	fs.IntVar(&c.NF5PerNodeRateLimit, "nf5-per-node-rate-limit", c.NF5PerNodeRateLimit, "per-node direct-origin fallback rate (per minute)")
	fs.DurationVar(&c.BootstrapWindow, "bootstrap-window", c.BootstrapWindow, "time after startup during which DHT-empty is not trusted as cold-start")
	fs.IntVar(&c.BootstrapRoutingTablePct, "bootstrap-routing-table-pct", c.BootstrapRoutingTablePct, "routing-table-size percent that ends the bootstrap window")
	fs.IntVar(&c.TopKExpansionFactorDegraded, "topk-expansion-factor-degraded", c.TopKExpansionFactorDegraded, "multiplier applied to top_k when expanding under degraded DHT health")

	fs.DurationVar(&c.OriginFailureCooldownInitial, "origin-failure-cooldown-initial", c.OriginFailureCooldownInitial, "initial cooldown for the origin-failure circuit breaker")
	fs.DurationVar(&c.OriginFailureCooldownMax, "origin-failure-cooldown-max", c.OriginFailureCooldownMax, "max cooldown for the origin-failure circuit breaker")
	fs.IntVar(&c.OriginFailureCooldownMultiplier, "origin-failure-cooldown-multiplier", c.OriginFailureCooldownMultiplier, "exponential multiplier between successive cooldowns")
	fs.DurationVar(&c.OriginFailureHonorWindowCap, "origin-failure-honor-window-cap", c.OriginFailureHonorWindowCap, "requester-side honor window cap for transient cooldowns")

	fs.StringVar(&c.LogLevel, "log-level", c.LogLevel, "log level (debug/info/warn/error)")
	fs.StringVar(&c.LogFormat, "log-format", c.LogFormat, "log format (json/text)")
}

// Load is the convenience composition of NewDefault -> LoadYAML(file) ->
// LoadEnv -> BindFlags -> fs.Parse(args). It returns the fully-merged Config
// and the FlagSet for callers that want to print -help text. Validate is
// the caller's responsibility - Load does not call it so callers can
// inspect partial configs (e.g., for `gantry version`).
func Load(args []string, env func(string) string, configPath string) (*Config, *flag.FlagSet, error) {
	c := NewDefault()

	if configPath != "" {
		f, err := os.Open(configPath) //#nosec G304 -- operator-supplied path
		if err != nil {
			return nil, nil, fmt.Errorf("config: open %s: %w", configPath, err)
		}

		defer func() { _ = f.Close() }() //nolint:errcheck // best-effort close

		if err := c.LoadYAML(f); err != nil {
			return nil, nil, fmt.Errorf("config: parse %s: %w", configPath, err)
		}
	}

	if err := c.LoadEnv(env); err != nil {
		return nil, nil, err
	}

	fs := flag.NewFlagSet("gantry", flag.ContinueOnError)
	c.BindFlags(fs)
	// --config is parsed by the caller (chicken-and-egg with the file load)
	// but we still register it so -help lists it.
	_ = fs.String("config", configPath, "path to YAML config file") //nolint:errcheck // best-effort
	if err := fs.Parse(args); err != nil {
		return nil, fs, err
	}

	return c, fs, nil
}

// Validate runs hard-correctness checks on c. Returns nil if c is usable;
// otherwise returns a joined error listing every problem found.
func (c *Config) Validate() error {
	var errs []error

	mustAddr := func(field, val string) {
		if val == "" {
			errs = append(errs, fmt.Errorf("%s: required", field))
			return
		}

		if _, _, err := net.SplitHostPort(val); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", field, err))
		}
	}
	mustAddr("mirror_listen", c.MirrorListen)
	mustAddr("transfer_listen", c.TransferListen)
	mustAddr("metrics_listen", c.MetricsListen)

	if c.PprofListen != "" {
		mustAddr("pprof_listen", c.PprofListen)

		if host, portText, err := net.SplitHostPort(c.PprofListen); err == nil {
			ip := net.ParseIP(host)
			if host == "" || (ip != nil && !ip.IsLoopback()) || (ip == nil && host != "localhost") {
				errs = append(errs, fmt.Errorf("pprof_listen %q must bind loopback (127.0.0.1, ::1, or localhost)", c.PprofListen))
			}

			port, portErr := strconv.Atoi(portText)
			if portErr != nil || port < 1 || port > 65535 {
				errs = append(errs, fmt.Errorf("pprof_listen %q must use a numeric port between 1 and 65535", c.PprofListen))
			}
		}
	}

	// MirrorListen MUST be loopback (the design doc, the design doc) unless the operator has
	// explicitly opted in to a non-loopback bind. See the field comment on
	// Config.MirrorBindAllowNonLoopback for when that's safe.
	if !c.MirrorBindAllowNonLoopback {
		if host, _, err := net.SplitHostPort(c.MirrorListen); err == nil {
			ip := net.ParseIP(host)
			if ip != nil && !ip.IsLoopback() {
				errs = append(errs, fmt.Errorf("mirror_listen %q is not loopback; only 127.0.0.1 / ::1 are safe - set mirror_bind_allow_non_loopback: true to override (operator opt-in)", c.MirrorListen))
			}

			// Empty host (e.g. ":5000") binds all interfaces, which is
			// equivalent to 0.0.0.0 and must also require the opt-in.
			if host == "" {
				errs = append(errs, fmt.Errorf("mirror_listen %q binds all interfaces; only loopback is safe - set mirror_bind_allow_non_loopback: true to override", c.MirrorListen))
			}

			if ip == nil && host != "localhost" && host != "" {
				errs = append(errs, fmt.Errorf("mirror_listen host %q: must be loopback (or set mirror_bind_allow_non_loopback: true)", host))
			}
		}
	}

	// Deprecated cache fields (CacheDir, CacheBudgetBytes,
	// CacheForcedEvictionHeadroomPct, EvictionProviderCountThreshold)
	// are no longer validated - they are silently ignored under
	// storage_mode=containerd. Validation here would force operators
	// to either keep sensible-looking values (defeating the
	// "deprecated" signal) or remove them from their ConfigMap (a
	// breaking change). We do neither; the fields exist as no-ops
	// until a future major version removes them.

	switch c.StorageMode {
	case StorageModeContainerd:
		// valid
	case storageModeGantryCache:
		errs = append(errs, errors.New("storage_mode \"gantry-cache\" was removed in ; set storage_mode: containerd and remove the cache_dir/cache_budget_bytes hostPath volume from your DaemonSet"))
	case "":
		errs = append(errs, errors.New("storage_mode: required (must be \"containerd\")"))
	default:
		errs = append(errs, fmt.Errorf("storage_mode %q: must be \"containerd\"", c.StorageMode))
	}

	if c.StorageMode == StorageModeContainerd && c.ContainerdSocket == "" {
		errs = append(errs, errors.New("storage_mode=containerd requires containerd_socket to be set"))
	}

	if c.StorageMode == StorageModeContainerd {
		// mandates a 30m–120m TTL. We accept a wider
		// range with warnings deferred to log; pure validation just
		// requires positive values so the cleanup interval cannot
		// degenerate into a tight loop.
		if c.ContainerdLeaseTTL <= 0 {
			errs = append(errs, fmt.Errorf("containerd_lease_ttl: must be > 0 in storage_mode=containerd, got %s", c.ContainerdLeaseTTL))
		}

		if c.ContainerdLeaseCleanupInterval <= 0 {
			errs = append(errs, fmt.Errorf("containerd_lease_cleanup_interval: must be > 0 in storage_mode=containerd, got %s", c.ContainerdLeaseCleanupInterval))
		}
	}

	if len(c.UpstreamRegistries) == 0 {
		errs = append(errs, errors.New("upstream_registries: at least one entry required"))
	}

	seen := map[string]int{}
	for i, ur := range c.UpstreamRegistries {
		if ur.Name == "" {
			errs = append(errs, fmt.Errorf("upstream_registries[%d].name: required", i))
		} else if prev, ok := seen[ur.Name]; ok {
			errs = append(errs, fmt.Errorf("upstream_registries[%d].name: duplicates upstream_registries[%d]", i, prev))
		} else {
			seen[ur.Name] = i
		}

		// Check NSAlias duplicates and alias-vs-name collisions.
		if ur.NSAlias != "" {
			if prev, ok := seen[ur.NSAlias]; ok {
				errs = append(errs, fmt.Errorf("upstream_registries[%d].ns_alias %q: collides with upstream_registries[%d]", i, ur.NSAlias, prev))
			} else {
				seen[ur.NSAlias] = i
			}
		}

		if ur.Endpoint == "" {
			errs = append(errs, fmt.Errorf("upstream_registries[%d].endpoint: required", i))
		} else if !strings.HasPrefix(ur.Endpoint, "http://") && !strings.HasPrefix(ur.Endpoint, "https://") {
			errs = append(errs, fmt.Errorf("upstream_registries[%d].endpoint %q: must start with http:// or https://", i, ur.Endpoint))
		}
	}

	if c.TopK < 1 {
		errs = append(errs, fmt.Errorf("top_k: must be >= 1, got %d", c.TopK))
	}

	{
		if c.Rendezvous.Namespace == "" && !c.Rendezvous.SingleNode {
			errs = append(errs, errors.New("rendezvous.namespace: required unless single_node is true"))
		}

		if c.PodIP == "" && !c.Rendezvous.SingleNode {
			errs = append(errs, errors.New("pod_ip: required in a clustered deployment to advertise a dialable libp2p address"))
		} else if c.PodIP != "" && !c.Rendezvous.SingleNode {
			podIP := net.ParseIP(c.PodIP)
			if podIP == nil || !podIP.IsGlobalUnicast() {
				errs = append(errs, errors.New("pod_ip: a clustered deployment requires a globally unicast IPv4 or IPv6 address"))
			}
		}

		if c.Rendezvous.SlotCount < 1 {
			errs = append(errs, errors.New("rendezvous.slot_count: must be >= 1"))
		}

		if c.Rendezvous.ReadsPerRound < 1 || c.Rendezvous.ReadsPerRound > c.Rendezvous.SlotCount {
			errs = append(errs, fmt.Errorf("rendezvous.reads_per_round: must be in [1,%d]", c.Rendezvous.SlotCount))
		}

		if c.Rendezvous.ClaimAttemptsPerRound < 1 || c.Rendezvous.ClaimAttemptsPerRound > c.Rendezvous.SlotCount {
			errs = append(errs, fmt.Errorf("rendezvous.claim_attempts_per_round: must be in [1,%d]", c.Rendezvous.SlotCount))
		}

		if c.Rendezvous.ContactsPerSlot < 1 {
			errs = append(errs, errors.New("rendezvous.contacts_per_slot: must be >= 1"))
		}

		if c.Rendezvous.FullScanAfter < 1 {
			errs = append(errs, errors.New("rendezvous.full_scan_after: must be >= 1"))
		}

		if c.Rendezvous.RoutingTableMin < 1 && !c.Rendezvous.SingleNode {
			errs = append(errs, errors.New("rendezvous.routing_table_min: must be >= 1 in clustered mode"))
		}

		if c.Rendezvous.FallbackNodeUpperBound < 1 {
			errs = append(errs, errors.New("rendezvous.fallback_node_upper_bound: must be >= 1"))
		}

		if c.Rendezvous.LeaseDuration < time.Second {
			errs = append(errs, errors.New("rendezvous.lease_duration: must be at least 1s because Kubernetes stores whole seconds"))
		}

		if c.Rendezvous.RenewInterval <= 0 || c.Rendezvous.RenewInterval >= c.Rendezvous.LeaseDuration {
			errs = append(errs, errors.New("rendezvous.renew_interval: must be > 0 and less than lease_duration"))
		}

		if c.Rendezvous.StaleContactGrace < 0 {
			errs = append(errs, errors.New("rendezvous.stale_contact_grace: must be >= 0"))
		}

		if c.Rendezvous.RetryMin <= 0 || c.Rendezvous.RetryMax < c.Rendezvous.RetryMin {
			errs = append(errs, errors.New("rendezvous retry bounds: retry_min must be > 0 and retry_max must be >= retry_min"))
		}

		if c.NF5JitterCap <= 0 {
			errs = append(errs, errors.New("nf5_jitter_cap: a finite jitter cap is required because there is no exact cluster size to scale against"))
		}
	}

	if !strings.HasPrefix(c.DHTProtocolPrefix, "/") || len(c.DHTProtocolPrefix) < 2 {
		errs = append(errs, fmt.Errorf("dht_protocol_prefix: must start with '/' and contain a name, got %q", c.DHTProtocolPrefix))
	}

	if c.PrefetchPullerReplicas < 1 {
		errs = append(errs, fmt.Errorf("prefetch_puller_replicas: must be >= 1, got %d", c.PrefetchPullerReplicas))
	}

	if c.PrefetchCoordinatorReplicas < 1 {
		errs = append(errs, fmt.Errorf("prefetch_coordinator_replicas: must be >= 1, got %d", c.PrefetchCoordinatorReplicas))
	}

	if c.CoordMaxDigestsPerRequest < 1 {
		errs = append(errs, fmt.Errorf("coord_max_digests_per_request: must be >= 1, got %d", c.CoordMaxDigestsPerRequest))
	}

	if c.PrefetchMaxConcurrentGroups < 1 {
		errs = append(errs, fmt.Errorf("prefetch_max_concurrent_groups: must be >= 1, got %d", c.PrefetchMaxConcurrentGroups))
	}

	if c.PrefetchDispatchJitter < 0 {
		errs = append(errs, fmt.Errorf("prefetch_dispatch_jitter: must be >= 0, got %v", c.PrefetchDispatchJitter))
	}

	if c.CoordMaxConcurrentPulls < 1 {
		errs = append(errs, fmt.Errorf("coord_max_concurrent_pulls: must be >= 1, got %d", c.CoordMaxConcurrentPulls))
	}

	if c.PeerFetchTimeout <= 0 {
		errs = append(errs, fmt.Errorf("peer_fetch_timeout: must be > 0, got %v", c.PeerFetchTimeout))
	}

	if c.PeerMaxAttempts < 1 {
		errs = append(errs, fmt.Errorf("peer_max_attempts: must be >= 1, got %d", c.PeerMaxAttempts))
	}

	if c.PeerRediscoverBudget < 0 {
		errs = append(errs, fmt.Errorf("peer_rediscover_budget: must be >= 0, got %v", c.PeerRediscoverBudget))
	}

	if c.PeerRediscoverBackoff < 0 {
		errs = append(errs, fmt.Errorf("peer_rediscover_backoff: must be >= 0, got %v", c.PeerRediscoverBackoff))
	}

	if c.TransferMaxConcurrentServes < 0 {
		errs = append(errs, fmt.Errorf("transfer_max_concurrent_serves: must be >= 0, got %d", c.TransferMaxConcurrentServes))
	}

	if c.AdvertiseReconcileInterval <= 0 {
		errs = append(errs, fmt.Errorf("advertise_reconcile_interval: must be > 0, got %v", c.AdvertiseReconcileInterval))
	}

	if c.NF5JitterBase <= 0 {
		errs = append(errs, fmt.Errorf("nf5_jitter_base: must be > 0, got %v", c.NF5JitterBase))
	}

	if c.NF5JitterCap < 0 {
		errs = append(errs, fmt.Errorf("nf5_jitter_cap: must be >= 0, got %v", c.NF5JitterCap))
	}

	if c.NF5JitterCap > 0 && c.NF5JitterBase > 0 && c.NF5JitterCap < c.NF5JitterBase {
		errs = append(errs, fmt.Errorf("nf5_jitter_cap (%v) must be >= nf5_jitter_base (%v) when set", c.NF5JitterCap, c.NF5JitterBase))
	}

	if c.NF5PerNodeRateLimit < 1 {
		errs = append(errs, fmt.Errorf("nf5_per_node_rate_limit: must be >= 1, got %d", c.NF5PerNodeRateLimit))
	}

	if c.BootstrapWindow <= 0 {
		errs = append(errs, fmt.Errorf("bootstrap_window: must be > 0, got %v", c.BootstrapWindow))
	}

	if c.BootstrapRoutingTablePct < 1 || c.BootstrapRoutingTablePct > 100 {
		errs = append(errs, fmt.Errorf("bootstrap_routing_table_pct: must be in [1,100], got %d", c.BootstrapRoutingTablePct))
	}

	if c.TopKExpansionFactorDegraded < 1 {
		errs = append(errs, fmt.Errorf("topk_expansion_factor_degraded: must be >= 1, got %d", c.TopKExpansionFactorDegraded))
	}

	if c.OriginFailureCooldownInitial <= 0 {
		errs = append(errs, fmt.Errorf("origin_failure_cooldown_initial: must be > 0, got %v", c.OriginFailureCooldownInitial))
	}

	if c.OriginFailureCooldownMax < c.OriginFailureCooldownInitial {
		errs = append(errs, fmt.Errorf("origin_failure_cooldown_max %v: must be >= origin_failure_cooldown_initial %v", c.OriginFailureCooldownMax, c.OriginFailureCooldownInitial))
	}

	if c.OriginFailureCooldownMultiplier < 2 {
		errs = append(errs, fmt.Errorf("origin_failure_cooldown_multiplier: must be >= 2, got %d", c.OriginFailureCooldownMultiplier))
	}

	// Validate failure classes if explicitly set.
	validClasses := map[string]bool{"auth": true, "not_found": true, "rate_limited": true, "transient": true}
	for _, cls := range c.OriginFailureClassesTrustedClusterWide {
		if !validClasses[cls] {
			errs = append(errs, fmt.Errorf("origin_failure_classes_trusted_cluster_wide: unknown class %q (valid: auth, not_found, rate_limited, transient)", cls))
		}
	}

	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("log_level %q: must be debug|info|warn|error", c.LogLevel))
	}

	switch c.LogFormat {
	case "json", "text":
	default:
		errs = append(errs, fmt.Errorf("log_format %q: must be json|text", c.LogFormat))
	}

	return errors.Join(errs...)
}

// ResolveUpstream returns the UpstreamRegistry whose Name (or NSAlias)
// equals ns. Returns false if ns does not match any configured registry.
func (c *Config) ResolveUpstream(ns string) (UpstreamRegistry, bool) {
	for _, ur := range c.UpstreamRegistries {
		if ur.Name == ns || (ur.NSAlias != "" && ur.NSAlias == ns) {
			return ur, true
		}
	}

	return UpstreamRegistry{}, false
}

// Redacted returns a copy of c suitable for logging. Currently, credentials
// are referenced only by path, so nothing requires actual redaction; the
// method exists so future secret-bearing fields have one obvious place to
// be sanitized.
func (c *Config) Redacted() *Config {
	cp := *c
	cp.UpstreamRegistries = append([]UpstreamRegistry(nil), c.UpstreamRegistries...)
	// CredentialsPath is a path, not the secret; safe to log as-is.
	return &cp
}

func lookup(env func(string) string, key string) (string, bool) {
	v := env("GANTRY_" + key)
	return v, v != ""
}
