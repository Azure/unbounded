// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package node implements racer-ctrl, the per-node follower half of the racer
// control plane.
//
// racer-ctrl owns exactly one thing: the racer.config.NodeConfig file on the
// node it runs on. It reads cluster-scoped desired state that the
// unbounded-operator publishes as annotations (StorageClass = universe,
// PersistentVolume = volume, Node = identity), derives the whole NodeConfig for
// this node, and installs it by rename(2) into the directory the racer
// dataplane watches. It never allocates a cluster-scoped identifier and never
// writes another node's object; the operator is the single writer of all of
// that.
//
// It also:
//
//   - checks the host prerequisites racer needs before it can serve (R9),
//   - manages the NVMe-oF fabric: publishes one namespace per universe from the
//     local fabric ublk device and attaches the peers' namespaces, recording
//     the resulting local device paths so they can be rendered into Peer.device
//     (R7),
//   - scrapes racer's Prometheus endpoint and republishes the handful of
//     numbers the operator's sequencers need as annotations on its own Node
//     (R8), and
//   - serves the CSI Identity and Node services so a pod can consume a racer
//     volume as a raw block device.
package node

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/unbounded/internal/racerctrl"
)

// Environment variable names. racer-ctrl takes no flags: like the
// unbounded-storage supervisor it is configured entirely from the environment
// so the DaemonSet manifest is the single place a knob is set.
const (
	EnvNodeName      = "NODE_NAME"
	EnvNamespace     = "POD_NAMESPACE"
	EnvConfigDir     = "RACER_CONFIG_DIR"
	EnvStorePath     = "RACER_STORE"
	EnvMetricsURL    = "RACER_METRICS_URL"
	EnvCSIEndpoint   = "CSI_ENDPOINT"
	EnvKubeconfig    = "KUBECONFIG"
	EnvNvmetRoot     = "RACER_NVMET_ROOT"
	EnvFabricAddr    = "RACER_FABRIC_ADDR"
	EnvFabricPort    = "RACER_FABRIC_PORT"
	EnvRDMAPort      = "RACER_RDMA_PORT"
	EnvNQNPrefix     = "RACER_NQN_PREFIX"
	EnvStageTimeout  = "RACER_STAGE_TIMEOUT"
	EnvSkipPreflight = "RACER_SKIP_PREFLIGHT"
	EnvDeviceIDBase  = "RACER_DEVICE_ID_BASE"
)

// Defaults for everything the manifest does not pin.
const (
	// DefaultConfigDir is the directory racer watches. racer-ctrl is the only
	// writer of it, as R1 demands.
	DefaultConfigDir = "/etc/racer"

	// DefaultStorePath mirrors the dataplane's own default for RACER_STORE.
	DefaultStorePath = "/var/lib/racer/store.img"

	// DefaultMetricsURL is racer's METRICS_ADDR default, bound to loopback. The
	// endpoint is unauthenticated plaintext and serves one connection at a
	// time, so it is only ever reachable from inside the pod.
	DefaultMetricsURL = "http://127.0.0.1:9090/metrics"

	// DefaultCSIEndpoint is where the kubelet plugin socket is created.
	DefaultCSIEndpoint = "unix:///csi/csi.sock"

	// DefaultNvmetRoot is the kernel configfs mount point for the NVMe target.
	DefaultNvmetRoot = "/sys/kernel/config/nvmet"

	// DefaultNamespace is where the operator keeps the membership ConfigMaps.
	DefaultNamespace = "unbounded-system"

	// DefaultFabricPort is the NVMe/TCP discovery port.
	DefaultFabricPort = 4420

	// DefaultRDMAPort is the service port of the RDMA nvmet port. It is
	// distinct from the TCP one because a node publishes both at once: the
	// subsystem is linked into two nvmet ports, and a port is identified by
	// its transport and address, of which the service id is part.
	DefaultRDMAPort = 4421

	// DefaultNQNPrefix is the stem every racer subsystem NQN is built from.
	DefaultNQNPrefix = "nqn.2024-01.io.unbounded-cloud:racer"

	// DefaultStageTimeout bounds how long NodeStageVolume waits for racer to
	// accept the generation that adds the device and for the block device node
	// to appear.
	DefaultStageTimeout = 2 * time.Minute

	// DeviceIDBaseAuto is the value of RACER_DEVICE_ID_BASE that asks for a
	// window derived from this node's allocated id instead of a fixed floor.
	DeviceIDBaseAuto = "auto"
)

// Config is the resolved node agent configuration.
type Config struct {
	// NodeName is the Node object this agent owns. Required.
	NodeName string

	// Namespace is the operator's namespace, where the per-zone membership
	// ConfigMaps live. The agent reads them and writes nothing there.
	Namespace string

	// ConfigDir is the directory racer watches for its config file.
	ConfigDir string

	// StorePath is the racer backing store. racer-ctrl only checks that its
	// filesystem behaves; racer itself formats and grows it.
	StorePath string

	// MetricsURL is racer's Prometheus endpoint.
	MetricsURL string

	// CSIEndpoint is the listen address for the CSI plugin socket.
	CSIEndpoint string

	// Kubeconfig is an explicit kubeconfig path. Empty means in-cluster.
	Kubeconfig string

	// NvmetRoot is the nvmet configfs root.
	NvmetRoot string

	// FabricAddr is the address peers reach this node's NVMe target on. Empty
	// disables fabric management entirely, which is the right setting for a
	// single-node universe where no peer link is needed.
	FabricAddr string

	// FabricPort is the NVMe/TCP service port.
	FabricPort int

	// RDMAPort is the service port of the RDMA nvmet port, used only when this
	// node publishes an RDMA address.
	RDMAPort int

	// NQNPrefix is the subsystem NQN stem.
	NQNPrefix string

	// StageTimeout bounds NodeStageVolume.
	StageTimeout time.Duration

	// SkipPreflight disables the host prerequisite checks. Only useful for
	// development against a host that cannot satisfy them.
	SkipPreflight bool

	// DeviceIDBase is the lowest ublk minor this instance may allocate, and it
	// takes racerctrl.MaxExports ids from there.
	//
	// ublk minors are global to the kernel, not to the node object, so this
	// only needs setting where more than one racer shares a kernel: a test
	// harness running a whole zone on one box, or a host that already runs
	// another ublk user on the low minors. Zero means the bottom of the space.
	DeviceIDBase uint32

	// DeriveDeviceIDBase asks for a window derived from this node's allocated
	// id rather than a fixed one, which is what RACER_DEVICE_ID_BASE=auto
	// means.
	//
	// A fixed base only helps when the operator can give each instance a
	// different one, and a DaemonSet cannot: every pod reads the same template
	// and the downward API has no arithmetic. Node ids are already unique
	// across the cluster and already allocated before the agent needs a minor,
	// so deriving the window from the id gives every instance on a shared
	// kernel a disjoint slice with nothing to coordinate.
	//
	// This is not the production arrangement. One racer per kernel wants the
	// bottom of the space, because the derived window for a node id in the
	// thousands runs past what the driver will accept.
	DeriveDeviceIDBase bool
}

// ConfigPath is the full path of the file racer reads.
func (c Config) ConfigPath() string {
	return c.ConfigDir + "/" + racerctrl.ConfigFileName
}

// FabricEnabled reports whether this node should publish and attach NVMe-oF
// namespaces. Without a reachable address there is nothing to publish.
func (c Config) FabricEnabled() bool {
	return c.FabricAddr != ""
}

// LoadConfig resolves the agent configuration from the environment.
func LoadConfig() (Config, error) {
	cfg := Config{
		NodeName:      os.Getenv(EnvNodeName),
		Namespace:     envOr(EnvNamespace, DefaultNamespace),
		ConfigDir:     envOr(EnvConfigDir, DefaultConfigDir),
		StorePath:     envOr(EnvStorePath, DefaultStorePath),
		MetricsURL:    envOr(EnvMetricsURL, DefaultMetricsURL),
		CSIEndpoint:   envOr(EnvCSIEndpoint, DefaultCSIEndpoint),
		Kubeconfig:    os.Getenv(EnvKubeconfig),
		NvmetRoot:     envOr(EnvNvmetRoot, DefaultNvmetRoot),
		FabricAddr:    strings.TrimSpace(os.Getenv(EnvFabricAddr)),
		FabricPort:    DefaultFabricPort,
		RDMAPort:      DefaultRDMAPort,
		NQNPrefix:     envOr(EnvNQNPrefix, DefaultNQNPrefix),
		StageTimeout:  DefaultStageTimeout,
		SkipPreflight: truthy(os.Getenv(EnvSkipPreflight)),
	}

	if cfg.NodeName == "" {
		return Config{}, fmt.Errorf("%s must be set: racer-ctrl owns exactly one Node and must be told which", EnvNodeName)
	}

	if raw := os.Getenv(EnvFabricPort); raw != "" {
		port, err := strconv.Atoi(raw)
		if err != nil || port <= 0 || port > 65535 {
			return Config{}, fmt.Errorf("%s: %q is not a valid TCP port", EnvFabricPort, raw)
		}

		cfg.FabricPort = port
	}

	if raw := os.Getenv(EnvRDMAPort); raw != "" {
		port, err := strconv.Atoi(raw)
		if err != nil || port <= 0 || port > 65535 {
			return Config{}, fmt.Errorf("%s: %q is not a valid service port", EnvRDMAPort, raw)
		}

		cfg.RDMAPort = port
	}

	if raw := os.Getenv(EnvDeviceIDBase); raw != "" {
		if strings.EqualFold(strings.TrimSpace(raw), DeviceIDBaseAuto) {
			cfg.DeriveDeviceIDBase = true
		} else {
			base, err := strconv.ParseUint(raw, 10, 32)
			if err != nil || base < racerctrl.MinDeviceID ||
				base+racerctrl.MaxExports-1 > math.MaxUint32 {
				return Config{}, fmt.Errorf("%s: %q is neither %q nor a usable ublk minor",
					EnvDeviceIDBase, raw, DeviceIDBaseAuto)
			}

			cfg.DeviceIDBase = uint32(base)
		}
	}

	if raw := os.Getenv(EnvStageTimeout); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("%s: %q is not a positive duration", EnvStageTimeout, raw)
		}

		cfg.StageTimeout = d
	}

	return cfg, nil
}

func envOr(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}

	return fallback
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
