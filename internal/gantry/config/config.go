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

	"github.com/Azure/unbounded/internal/gantry/listener"
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

	// TransferAdvertise is the routable TCP endpoint published to peers when
	// TransferListen is a Unix socket behind a proxy. Empty reuses TransferListen.
	TransferAdvertise string `yaml:"transfer_advertise"`

	// MetricsListen is the Prometheus scrape endpoint and /readyz /livez
	// kubelet-probe target (the design doc). The default Unix socket is exposed
	// to Prometheus and kubelet by the deployment's operations proxy.
	MetricsListen string `yaml:"metrics_listen"`

	// Libp2pListen is the multiaddr(s) the libp2p host advertises (the design doc).
	// Empty means "use libp2p defaults" and pick at random.
	Libp2pListen []string `yaml:"libp2p_listen"`

	// Libp2pIdentityPath is the on-disk path of the persisted libp2p key
	// (the design doc). Lost identity is not catastrophic; old DHT records age out.
	Libp2pIdentityPath string `yaml:"libp2p_identity_path"`

	// Libp2pBootstrapPeers is an optional list of static multiaddrs to seed
	// the libp2p host's connection set on startup. In production these are
	// usually discovered via the K8s informer (the design doc) so this field defaults
	// to empty; tests and small clusters can use it directly.
	Libp2pBootstrapPeers []string `yaml:"libp2p_bootstrap_peers"`

	// ---------- Cluster membership (the design doc) ----------

	// NodeName is the Kubernetes node this agent runs on. Sourced via the
	// Downward API (env spec.nodeName) into GANTRY_NODE_NAME. Used as the
	// stable HRW NodeID and as the join key against the Node informer for
	// zone resolution.
	NodeName string `yaml:"node_name"`

	// PodName is the Kubernetes pod name of this agent. Sourced via the
	// Downward API (env metadata.name) into GANTRY_POD_NAME. Used to
	// self-patch pod annotations with the libp2p peer.ID and transfer
	// addr so other agents can discover this peer (the design doc, the design doc).
	PodName string `yaml:"pod_name"`

	// PodIP is the agent's routable Pod IP. Sourced via the Downward
	// API (env status.podIP) into GANTRY_POD_IP. Used to rewrite
	// 0.0.0.0 wildcard listen addresses into dialable advertised
	// addresses when self-announcing on Pod annotations (the design doc): a peer
	// publishing 0.0.0.0:5001 is otherwise unreachable from other
	// pods, defeating libp2p bootstrap on first-cluster boot. Empty
	// when running outside Kubernetes; self-announce then publishes
	// only non-wildcard listen addresses.
	PodIP string `yaml:"pod_ip"`

	// MembersNamespace restricts the Pod informer to a single namespace.
	// Empty means cluster-wide list/watch - useful for read-only
	// scenarios, but production deployments MUST set this. The
	// self-announce write path (members.AnnounceSelf) patches the
	// agent's own pod via Pods(namespace).Patch and refuses to run
	// when namespace == "" because the apiserver does not infer a
	// pod's home namespace from the pod name alone. Without a
	// namespace the agent's three peer-coordination annotations
	// (gantry.io/peer-id, gantry.io/p2p-addrs, gantry.io/transfer-addr)
	// are never published, so peers cannot translate this agent's
	// node name into a dialable libp2p peer ID - every inbound
	// Coord.PleasePull / PullIntentQuery 503s silently.
	//
	// The shipped DaemonSet wires GANTRY_MEMBERS_NAMESPACE via the
	// Downward API (`fieldRef: metadata.namespace`) so operators
	// following deploy/gantry/daemonset.yaml satisfy this for free. Hand-
	// rolled envFrom that misses it is the failure mode this
	// validation catches at startup rather than at first
	// Coord.PleasePull miss.
	MembersNamespace string `yaml:"members_namespace"`

	// MembersLabelSelector is the K8s label selector that identifies Gantry
	// DaemonSet pods. Used to find peer agents (the design doc). Default matches the
	// canonical app.kubernetes.io label.
	MembersLabelSelector string `yaml:"members_label_selector"`

	// MembersKubeconfig is an optional path to a kubeconfig file. Empty
	// means in-cluster service-account discovery (the production path).
	MembersKubeconfig string `yaml:"members_kubeconfig"`

	// MembersSyncTimeout is how long the agent waits for the initial
	// pod and node informer list-and-watch to complete at startup.
	// In production mode a timeout is fatal (it surfaces broken RBAC /
	// API egress early rather than silently degrading). Raise this on
	// clusters with a slow API server or during large-scale simultaneous
	// DaemonSet rollouts where the apiserver is under elevated load.
	// Zero means "use the built-in default of 30s".
	MembersSyncTimeout time.Duration `yaml:"members_sync_timeout"`

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

	// ---------- HRW / coordination ----------

	// HRWK is the top-K size for HRW probe (the step 3 default 3; the design doc
	// open question).
	HRWK int `yaml:"hrw_k"`

	// PrefetchPullerReplicas is how many distinct HRW-ranked pullers each
	// prefetched layer digest is dispatched to. 1 designates a single origin
	// puller per layer (tightest dedup), but the whole swarm then fans out
	// from ONE initial seed, which bottlenecks a cold thundering-herd (peer
	// transfers pile onto the lone seed and stall). N>1 asks the top-N pullers
	// to origin-pull the layer in parallel, giving N initial seeds so peer
	// transfers fan out N-fold, at the cost of up to N origin copies of each
	// layer. The default is 8.
	PrefetchPullerReplicas int `yaml:"prefetch_puller_replicas"`

	// HRWTopologyScope selects "cluster" (HRW over all nodes) or "zone"
	// (HRW within the requester's zone) - the design doc / the design doc open question.
	HRWTopologyScope string `yaml:"hrw_topology_scope"`

	// ZoneLabelKey is the Kubernetes node label that identifies the zone
	// when HRWTopologyScope == "zone". Default
	// `topology.kubernetes.io/zone` (the design doc).
	ZoneLabelKey string `yaml:"zone_label_key"`

	// CoordPeerAuthzEnforce flips coord peer authorization from
	// observe-only (record the unauthorized-peer metric, still serve) to
	// enforce (reject inbound coord requests whose libp2p peer ID is not
	// in the membership view). Default false: ship observe-only first,
	// verify peer-id annotations are visible for every ready Gantry pod,
	// size p2p_coord_unauthorized_peer_total across a full rollout, then
	// flip to true once it stays at zero.
	CoordPeerAuthzEnforce bool `yaml:"coord_peer_authz_enforce"`

	// CoordMaxDigestsPerRequest caps a single please_pull batch. The default
	// 256 is intentionally far above normal manifest child counts while staying
	// well below the 1 MiB coord envelope budget.
	CoordMaxDigestsPerRequest int `yaml:"coord_max_digests_per_request"`

	// CoordMaxConcurrentPulls caps background origin pulls started by inbound
	// please_pull. Each pull holds an HTTP response body, a containerd writer,
	// goroutine state, and a lease, so this protects the node from fan-out.
	CoordMaxConcurrentPulls int `yaml:"coord_max_concurrent_pulls"`

	// PeerFetchTimeout caps the complete peer request, including streaming and
	// committing the response body. It is deliberately SHORT (60s): a requester
	// stuck on a lockstep-saturated seed must bail and re-select a fresher
	// finisher-seed rather than ride the slow stream to completion. That
	// bail-and-re-select is what drives the cold-start cascade (paired with the
	// strict containerd hosts.toml, where an exhausted fetch 503s and containerd
	// retries Gantry, re-discovering recent finishers). Setting this too high
	// (e.g. 1h) removes the re-selection and collapses distribution to the
	// single-seed bandwidth bound (the ~12min lockstep regression).
	PeerFetchTimeout time.Duration `yaml:"peer_fetch_timeout"`

	// PeerRediscoverBudget bounds the total wall-clock time the mirror keeps
	// re-running DHT FindProviders and retrying peer fetches on a cache miss
	// before it gives up and falls to origin. Re-discovery lets a node pick up
	// finisher-seeds that advertise mid-swarm (the cascade) instead of
	// exhausting a fixed provider set and going to origin. Zero disables
	// re-discovery, restoring the historical single-shot provider attempt.
	// The default is 5m: paired with the strict containerd hosts.toml (shipped
	// in deploy/gantry/node-config.yaml) and TransferMaxConcurrentServes, this
	// drives the validated cold-start cascade.
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
	// default is 100. Shedding only preserves dedup with the strict containerd
	// hosts.toml (mirror-only, no origin fall-through), where a shed request
	// retries Gantry rather than falling through to origin.
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

	// TopKExpansionFactorDegraded is the multiplier applied to HRWK when
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

// NewDefault returns a Config populated with the design-doc defaults.
// All fields are set; Validate against this MUST pass.
func NewDefault() *Config {
	return &Config{
		MirrorListen:               "unix:///run/gantry/mirror.sock",
		MirrorBindAllowNonLoopback: false,
		TransferListen:             "unix:///run/gantry/transfer.sock",
		TransferAdvertise:          "0.0.0.0:5001",
		MetricsListen:              "unix:///run/gantry/ops.sock",
		Libp2pListen:               nil,
		Libp2pIdentityPath:         "/var/lib/gantry/libp2p.key",

		NodeName:             "",
		PodName:              "",
		MembersNamespace:     "",
		MembersLabelSelector: "app.kubernetes.io/name=gantry",
		MembersKubeconfig:    "",
		MembersSyncTimeout:   0, // zero means use built-in default of 30s

		StorageMode: StorageModeContainerd,

		ContainerdSocket:               "/run/containerd/containerd.sock",
		ContainerdNamespace:            "k8s.io",
		ContainerdLeaseTTL:             60 * time.Minute,
		ContainerdLeaseCleanupInterval: 30 * time.Minute,

		UpstreamRegistries: nil,

		HRWK:                   3,
		PrefetchPullerReplicas: 8,
		HRWTopologyScope:       "cluster",
		ZoneLabelKey:           "topology.kubernetes.io/zone",

		CoordPeerAuthzEnforce:       false,
		CoordMaxDigestsPerRequest:   256,
		CoordMaxConcurrentPulls:     16,
		PeerFetchTimeout:            60 * time.Second,
		PeerRediscoverBudget:        5 * time.Minute, // re-discovery cascade on by default (validated at 300 nodes)
		PeerRediscoverBackoff:       time.Second,     // pause between re-discovery rounds
		TransferMaxConcurrentServes: 100,             // serve cap sheds excess GETs with 429 (validated cascade)
		AdvertiseReconcileInterval:  time.Minute,

		NF5JitterBase:               3 * time.Second,
		NF5JitterCap:                0, // no cap by default (original behaviour)
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

// LoadYAML overlays a YAML document onto c. Unknown fields are an error so
// typos in config files don't silently no-op.
func (c *Config) LoadYAML(r io.Reader) error {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)

	return dec.Decode(c)
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
	setStr("TRANSFER_ADVERTISE", &c.TransferAdvertise)
	setStr("METRICS_LISTEN", &c.MetricsListen)
	setStr("LIBP2P_IDENTITY_PATH", &c.Libp2pIdentityPath)

	setStr("NODE_NAME", &c.NodeName)
	setStr("POD_NAME", &c.PodName)
	setStr("POD_IP", &c.PodIP)
	setStr("MEMBERS_NAMESPACE", &c.MembersNamespace)
	setStr("MEMBERS_LABEL_SELECTOR", &c.MembersLabelSelector)
	setStr("MEMBERS_KUBECONFIG", &c.MembersKubeconfig)
	setDur("MEMBERS_SYNC_TIMEOUT", &c.MembersSyncTimeout)

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

	setInt("HRW_K", &c.HRWK)
	setInt("PREFETCH_PULLER_REPLICAS", &c.PrefetchPullerReplicas)
	setStr("HRW_TOPOLOGY_SCOPE", &c.HRWTopologyScope)
	setStr("ZONE_LABEL_KEY", &c.ZoneLabelKey)
	setBool("COORD_PEER_AUTHZ_ENFORCE", &c.CoordPeerAuthzEnforce)
	setInt("COORD_MAX_DIGESTS_PER_REQUEST", &c.CoordMaxDigestsPerRequest)
	setInt("COORD_MAX_CONCURRENT_PULLS", &c.CoordMaxConcurrentPulls)
	setDur("PEER_FETCH_TIMEOUT", &c.PeerFetchTimeout)
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
	fs.StringVar(&c.MirrorListen, "mirror-listen", c.MirrorListen, "endpoint for the containerd-facing mirror server (host:port or unix:///path)")
	fs.BoolVar(&c.MirrorBindAllowNonLoopback, "mirror-bind-allow-non-loopback", c.MirrorBindAllowNonLoopback, "opt in to a non-loopback mirror bind (e.g. when using hostPort + hostIP=127.0.0.1 in Kubernetes)")
	fs.StringVar(&c.TransferListen, "transfer-listen", c.TransferListen, "endpoint for the peer-facing transfer server (host:port or unix:///path)")
	fs.StringVar(&c.TransferAdvertise, "transfer-advertise", c.TransferAdvertise, "routable TCP transfer endpoint published to peers when the server listens on a Unix socket")
	fs.StringVar(&c.MetricsListen, "metrics-listen", c.MetricsListen, "endpoint for the Prometheus and probe server (host:port or unix:///path)")
	fs.StringVar(&c.Libp2pIdentityPath, "libp2p-identity-path", c.Libp2pIdentityPath, "path to the persisted libp2p identity key")

	fs.StringVar(&c.NodeName, "node-name", c.NodeName, "Kubernetes node name this agent runs on (Downward API spec.nodeName)")
	fs.StringVar(&c.PodName, "pod-name", c.PodName, "Kubernetes pod name of this agent (Downward API metadata.name)")
	fs.StringVar(&c.PodIP, "pod-ip", c.PodIP, "Kubernetes pod IP of this agent (Downward API status.podIP); used to rewrite 0.0.0.0 listeners into dialable advertised addresses")
	fs.StringVar(&c.MembersNamespace, "members-namespace", c.MembersNamespace, "namespace to scope the pod informer (REQUIRED when node_name+pod_name are set - AnnounceSelf needs it to self-patch; empty is dev-only)")
	fs.StringVar(&c.MembersLabelSelector, "members-label-selector", c.MembersLabelSelector, "label selector identifying Gantry DaemonSet pods")
	fs.StringVar(&c.MembersKubeconfig, "members-kubeconfig", c.MembersKubeconfig, "optional path to a kubeconfig file (empty = in-cluster)")
	fs.DurationVar(&c.MembersSyncTimeout, "members-sync-timeout", c.MembersSyncTimeout, "how long to wait for the pod/node informer initial sync at startup (0 = use built-in default of 30s)")

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

	fs.IntVar(&c.HRWK, "hrw-k", c.HRWK, "HRW top-K size")
	fs.IntVar(&c.PrefetchPullerReplicas, "prefetch-puller-replicas", c.PrefetchPullerReplicas, "number of HRW-ranked pullers each prefetched layer digest is pulled by (initial seeds); 1 = single puller/tightest dedup, N = N-fold peer fan-out at N origin copies")
	fs.StringVar(&c.HRWTopologyScope, "hrw-topology-scope", c.HRWTopologyScope, `HRW scope: "cluster" or "zone"`)
	fs.StringVar(&c.ZoneLabelKey, "zone-label-key", c.ZoneLabelKey, "Kubernetes node label identifying the zone (used when hrw-topology-scope=zone)")
	fs.BoolVar(&c.CoordPeerAuthzEnforce, "coord-peer-authz-enforce", c.CoordPeerAuthzEnforce, "reject inbound coord requests from peers not in the membership view (default false = observe-only)")
	fs.IntVar(&c.CoordMaxDigestsPerRequest, "coord-max-digests-per-request", c.CoordMaxDigestsPerRequest, "maximum digests accepted in one please_pull batch")
	fs.IntVar(&c.CoordMaxConcurrentPulls, "coord-max-concurrent-pulls", c.CoordMaxConcurrentPulls, "maximum background origin pulls started by inbound please_pull")
	fs.DurationVar(&c.PeerFetchTimeout, "peer-fetch-timeout", c.PeerFetchTimeout, "maximum time for a complete peer fetch, including body transfer and commit")
	fs.DurationVar(&c.PeerRediscoverBudget, "peer-rediscover-budget", c.PeerRediscoverBudget, "total wall-clock budget for the peer re-discovery loop (0 disables re-discovery, restoring the single-shot provider attempt)")
	fs.DurationVar(&c.PeerRediscoverBackoff, "peer-rediscover-backoff", c.PeerRediscoverBackoff, "pause between peer re-discovery rounds (0 uses the built-in 1s default when re-discovery is enabled)")
	fs.IntVar(&c.TransferMaxConcurrentServes, "transfer-max-concurrent-serves", c.TransferMaxConcurrentServes, "cap on concurrent peer blob-body serves (over the cap returns 429; 0 = unlimited)")
	fs.DurationVar(&c.AdvertiseReconcileInterval, "advertise-reconcile-interval", c.AdvertiseReconcileInterval, "cadence of the background DHT advertiser inventory reconcile (backstop; eager advertise handles the fast path)")

	fs.DurationVar(&c.NF5JitterBase, "nf5-jitter-base", c.NF5JitterBase, "base delay for the NF5 jitter window")
	fs.DurationVar(&c.NF5JitterCap, "nf5-jitter-cap", c.NF5JitterCap, "hard ceiling on the NF5 jitter window (0 = no cap)")
	fs.IntVar(&c.NF5PerNodeRateLimit, "nf5-per-node-rate-limit", c.NF5PerNodeRateLimit, "per-node direct-origin fallback rate (per minute)")
	fs.DurationVar(&c.BootstrapWindow, "bootstrap-window", c.BootstrapWindow, "time after startup during which DHT-empty is not trusted as cold-start")
	fs.IntVar(&c.BootstrapRoutingTablePct, "bootstrap-routing-table-pct", c.BootstrapRoutingTablePct, "routing-table-size percent that ends the bootstrap window")
	fs.IntVar(&c.TopKExpansionFactorDegraded, "topk-expansion-factor-degraded", c.TopKExpansionFactorDegraded, "multiplier applied to HRW K when expanding under Degraded health")

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

	mustEndpoint := func(field, val string) {
		if val == "" {
			errs = append(errs, fmt.Errorf("%s: required", field))
			return
		}

		if _, _, err := listener.Parse(val); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", field, err))
		}
	}
	mustEndpoint("mirror_listen", c.MirrorListen)
	mustEndpoint("transfer_listen", c.TransferListen)
	mustEndpoint("metrics_listen", c.MetricsListen)

	if c.TransferAdvertise != "" {
		if _, _, err := net.SplitHostPort(c.TransferAdvertise); err != nil {
			errs = append(errs, fmt.Errorf("transfer_advertise: %w", err))
		}
	}

	if strings.HasPrefix(c.TransferListen, "unix://") && c.TransferAdvertise == "" {
		errs = append(errs, fmt.Errorf("transfer_advertise: required when transfer_listen is a Unix endpoint"))
	}

	// MirrorListen MUST be loopback (the design doc, the design doc) unless the operator has
	// explicitly opted in to a non-loopback bind. See the field comment on
	// Config.MirrorBindAllowNonLoopback for when that's safe.
	if !c.MirrorBindAllowNonLoopback && !strings.HasPrefix(c.MirrorListen, "unix://") {
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

	if c.HRWK < 1 {
		errs = append(errs, fmt.Errorf("hrw_k: must be >= 1, got %d", c.HRWK))
	}

	if c.PrefetchPullerReplicas < 1 {
		errs = append(errs, fmt.Errorf("prefetch_puller_replicas: must be >= 1, got %d", c.PrefetchPullerReplicas))
	}

	switch c.HRWTopologyScope {
	case "cluster", "zone":
	default:
		errs = append(errs, fmt.Errorf("hrw_topology_scope %q: must be \"cluster\" or \"zone\"", c.HRWTopologyScope))
	}

	if c.CoordMaxDigestsPerRequest < 1 {
		errs = append(errs, fmt.Errorf("coord_max_digests_per_request: must be >= 1, got %d", c.CoordMaxDigestsPerRequest))
	}

	if c.CoordMaxConcurrentPulls < 1 {
		errs = append(errs, fmt.Errorf("coord_max_concurrent_pulls: must be >= 1, got %d", c.CoordMaxConcurrentPulls))
	}

	if c.PeerFetchTimeout <= 0 {
		errs = append(errs, fmt.Errorf("peer_fetch_timeout: must be > 0, got %v", c.PeerFetchTimeout))
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

	// Production K8s mode: if NodeName is set but PodName is empty,
	// fail fast. NodeName tells peers our HRW/membership identity;
	// PodName is the apiserver target AnnounceSelf patches with the
	// three pod annotations (gantry.io/peer-id, gantry.io/p2p-addrs,
	// gantry.io/transfer-addr) that other agents use to translate
	// our node-name into a dialable libp2p peer-ID/addr pair. With
	// NodeName-without-PodName the agent is reachable to itself
	// (members informer can find peers, HRW can hash, /readyz can
	// even go green because selfAnnounceRequiredForReadiness is
	// false in this configuration) but is INVISIBLE to peers: every
	// inbound Coord.PleasePull / PullIntentQuery 503s silently
	// because no peer can resolve our node name to a peer ID. There
	// is no fallback peer-ID-mapping mechanism in the codebase that
	// would rescue this case - static bootstrap peers solve DHT
	// seeding, not annotation publication.
	//
	// The Downward API DaemonSet pattern shipped in deploy/ wires
	// all three env vars together (spec.nodeName, metadata.name,
	// metadata.namespace) so this misconfiguration only happens when
	// an operator hand-rolls envFrom - exactly the case where a
	// clear startup error beats hours of silent peer-coordination
	// failure.
	if c.NodeName != "" && c.PodName == "" {
		errs = append(errs, errors.New("node_name is set but pod_name is empty: production K8s mode requires pod_name (GANTRY_POD_NAME / metadata.name via the Downward API) so AnnounceSelf can publish gantry.io/peer-id, gantry.io/p2p-addrs, and gantry.io/transfer-addr on this agent's own pod; without it, other agents see this node in HRW/membership but cannot translate the node name to a dialable libp2p peer ID, silently 503-ing every Coord.PleasePull and PullIntentQuery RPC"))
	}

	// Production K8s mode also requires members_namespace. When
	// NodeName + PodName are both set, the agent will (a) participate
	// in HRW and (b) try to publish its three coordination annotations
	// via AnnounceSelf at startup. The self-announce path is a
	// Pods(namespace).Patch call that REQUIRES a concrete namespace -
	// members.AnnounceSelf refuses to run with an empty namespace
	// because the apiserver cannot infer a pod's home namespace from
	// the pod name alone (different namespaces can hold pods with the
	// same name). Without members_namespace set, AnnounceSelf fails on
	// every retry, /readyz never goes green (production readiness
	// requires a successful self-announce - see
	// selfAnnounceRequiredForReadiness in cmd/gantry/main.go), and the
	// agent is stuck unready forever - but the misconfiguration is
	// silent at config-load time because cluster-wide list/watch is a
	// supported informer mode in other contexts. Catch it at Validate
	// so the operator gets a clear startup error rather than a stuck
	// /readyz endpoint.
	//
	// The shipped DaemonSet at deploy/gantry/daemonset.yaml wires this via
	// the Downward API (fieldRef: metadata.namespace), so operators
	// following the canonical deploy path satisfy this for free; the
	// failure mode is a hand-rolled envFrom that omits the namespace
	// env var.
	if c.NodeName != "" && c.PodName != "" && c.MembersNamespace == "" {
		errs = append(errs, errors.New("members_namespace is empty but node_name and pod_name are set (production K8s mode): self-announce (members.AnnounceSelf) needs Options.Namespace to patch this agent's own pod with gantry.io/peer-id, gantry.io/p2p-addrs, and gantry.io/transfer-addr, and refuses to run cluster-wide because the apiserver cannot infer a pod's home namespace from name alone; set GANTRY_MEMBERS_NAMESPACE / members_namespace (typically via Downward API fieldRef: metadata.namespace, see deploy/gantry/daemonset.yaml) - without it the agent will never go ready because production-mode readiness requires a successful self-announce"))
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
