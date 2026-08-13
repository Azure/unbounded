// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/unbounded/internal/gantry/snapshotter"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/blockmap"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/catalog"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/ingest"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/segment"
)

// Defaults for the daemon. They are all overridable, but the values here are
// the ones the shipped DaemonSet uses.
const (
	DefaultSocket        = "/run/gantry-snapshotter/snapshotter.sock"
	DefaultRoot          = "/var/lib/gantry-snapshotter"
	DefaultWorkDir       = "/var/lib/gantry-snapshotter/ingest"
	DefaultDevices       = segment.DefaultPath
	DefaultNamespace     = snapshotter.DefaultNamespace
	DefaultSocketMode    = 0o660
	DefaultCleanup       = 10 * time.Minute
	DefaultCatalogSync   = 30 * time.Second
	DefaultShutdownGrace = 15 * time.Second
)

// Config is the daemon's whole configuration.
type Config struct {
	// Socket is the unix socket containerd's proxy plugin connects to.
	Socket string

	// SocketMode is the permission bits on that socket. containerd runs as
	// root in every deployment we support, so the default is
	// owner-and-group only rather than world writable: a world writable
	// snapshotter socket is a straightforward container escape.
	SocketMode os.FileMode

	// Root is where snapshot directories and the bbolt metadata live.
	Root string

	// WorkDir is scratch space for ingest: one layer tar plus one EROFS
	// image at a time.
	WorkDir string

	// WorkHeadroom is the free space on WorkDir's filesystem that ingest
	// refuses to spend. WorkDir is a hostPath shared with every other pod
	// on the node, so this is what stops a large layer from filling the
	// node's disk and evicting workloads that have nothing to do with it.
	WorkHeadroom uint64

	// Devices is the operator-rendered description of this node's image
	// devices.
	Devices string

	// DeviceInterval is how often that file is re-read.
	DeviceInterval time.Duration

	// FormatCatalog allows this node to lay down a fresh catalog when the
	// catalog device is blank. It is off by default: the catalog is
	// cluster state, and a node that formats one because it happened to
	// read a device that was not the catalog would erase the cluster's
	// image index.
	FormatCatalog bool

	// ConflictErrnos names the errnos the catalog device reports for a
	// failed optimistic write. See catalog.ParseConflictErrnos.
	ConflictErrnos string

	// SegmentBlocks is the segment table size used when formatting.
	SegmentBlocks uint32

	// AdoptSegments lets this node add segments it can see to the catalog's
	// segment table and open one when none is open. This is in-band
	// cluster state, but every mutation is a compare-and-swap and is
	// idempotent, so it is safe for every node to attempt it.
	AdoptSegments bool

	// ContainerdSocket and ContainerdNamespace locate the content store
	// ingest reads layer tars from.
	ContainerdSocket    string
	ContainerdNamespace string

	// NodeName is this node, used to score layers for ingest election.
	NodeName string

	// MembersSelector is the label selector identifying peer snapshotters.
	// Empty disables the Kubernetes membership view and makes every node
	// an eager ingester, which is what a single-node development cluster
	// wants.
	MembersSelector string

	// MembersNamespace restricts the pod informer.
	MembersNamespace string

	// Kubeconfig is an out-of-cluster kubeconfig for development.
	Kubeconfig string

	// ZoneLabel is the node label carrying the topology zone.
	ZoneLabel string

	// ElectionStep is the delay added per rendezvous rank.
	ElectionStep time.Duration

	// ElectionMaxDelay caps that delay.
	ElectionMaxDelay time.Duration

	// IngestWorkers, IngestDepth and IngestRetry configure the queue.
	IngestWorkers int
	IngestDepth   int
	IngestRetry   time.Duration

	// SkipVerify drops the read-back check after a blob is written.
	SkipVerify bool

	// MkfsErofs is the erofs image builder binary.
	MkfsErofs string

	// MountOptions are extra overlayfs options.
	MountOptions []string

	// MapRoot is where layer mounts are placed. Keep it short: every
	// mounted layer's path ends up in an overlay option string that has to
	// fit in a page.
	MapRoot string

	// CleanupInterval is how often orphan directories and stale layer
	// mappings are swept. Zero disables the sweep.
	CleanupInterval time.Duration

	// CatalogSync is how often the catalog is polled in the background.
	// The container start path also syncs on a miss, so this only bounds
	// how stale the index gets while nothing is starting.
	CatalogSync time.Duration

	// HoleGrace is how long a hole in the catalog's record slots has to
	// persist before this node concludes its writer died and retires it.
	// Zero disables the repair, which leaves a crashed writer's hole to
	// block every node's view of the catalog until an operator intervenes.
	HoleGrace time.Duration

	// ShutdownGrace bounds the graceful stop before connections are cut.
	ShutdownGrace time.Duration

	// LogLevel is debug, info, warn or error.
	LogLevel string

	// LogFormat is text or json.
	LogFormat string

	// MetricsAddr serves Prometheus metrics and the liveness endpoint when
	// set.
	MetricsAddr string

	// EnablePprof adds the net/http/pprof handlers to the metrics listener.
	//
	// Off by default because that listener is reachable from the pod
	// network: pprof hands out heap contents and execution traces to
	// anything that can dial it, and the liveness probe means the port
	// cannot simply be moved to loopback.
	EnablePprof bool
}

// parseConfig builds a Config from the command line, with environment
// variables as the fallback layer so the DaemonSet can use the Downward API
// for the node name without a wrapper script.
func parseConfig(args []string, stderr io.Writer) (*Config, error) {
	c := &Config{}
	fs := flag.NewFlagSet("gantry-snapshotter", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		mountOptions, socketMode string
		segmentBlocks            uint64
	)

	fs.StringVar(&c.Socket, "socket", envOr("GANTRY_SNAPSHOTTER_SOCKET", DefaultSocket), "unix socket to serve the snapshotter API on")
	fs.StringVar(&socketMode, "socket-mode", envOr("GANTRY_SNAPSHOTTER_SOCKET_MODE", "0660"), "permission bits on the socket")
	fs.StringVar(&c.Root, "root", envOr("GANTRY_SNAPSHOTTER_ROOT", DefaultRoot), "directory for snapshot metadata and local layers")
	fs.StringVar(&c.WorkDir, "work-dir", envOr("GANTRY_SNAPSHOTTER_WORK_DIR", DefaultWorkDir), "scratch directory for ingest")
	fs.Uint64Var(&c.WorkHeadroom, "work-headroom", envUint64("GANTRY_SNAPSHOTTER_WORK_HEADROOM", ingest.DefaultHeadroom), "free bytes on the work filesystem an ingest will not spend")
	fs.StringVar(&c.Devices, "devices", envOr("GANTRY_SNAPSHOTTER_DEVICES", DefaultDevices), "operator-rendered image device description")
	fs.DurationVar(&c.DeviceInterval, "device-interval", envDuration("GANTRY_SNAPSHOTTER_DEVICE_INTERVAL", segment.DefaultWatchInterval), "how often to re-read the device description")
	fs.BoolVar(&c.FormatCatalog, "format-catalog", envBool("GANTRY_SNAPSHOTTER_FORMAT_CATALOG", false), "format the catalog device when it is blank")
	fs.StringVar(&c.ConflictErrnos, "conflict-errnos", envOr("GANTRY_SNAPSHOTTER_CONFLICT_ERRNOS", ""), "errnos the catalog device reports for a failed optimistic write")
	fs.Uint64Var(&segmentBlocks, "segment-blocks", uint64(envInt("GANTRY_SNAPSHOTTER_SEGMENT_BLOCKS", catalog.DefaultSegmentBlocks)), "segment table size in blocks, used only when formatting")
	fs.BoolVar(&c.AdoptSegments, "adopt-segments", envBool("GANTRY_SNAPSHOTTER_ADOPT_SEGMENTS", true), "register visible segments in the catalog and open one when none is open")
	fs.StringVar(&c.ContainerdSocket, "containerd-socket", envOr("GANTRY_SNAPSHOTTER_CONTAINERD_SOCKET", "/run/containerd/containerd.sock"), "containerd socket for reading layer blobs")
	fs.StringVar(&c.ContainerdNamespace, "containerd-namespace", envOr("GANTRY_SNAPSHOTTER_CONTAINERD_NAMESPACE", DefaultNamespace), "containerd namespace holding image content")
	fs.StringVar(&c.NodeName, "node-name", envOr("GANTRY_NODE_NAME", ""), "this node's name, used to elect an ingester per layer")
	fs.StringVar(&c.MembersSelector, "members-selector", envOr("GANTRY_SNAPSHOTTER_MEMBERS_SELECTOR", ""), "label selector for peer snapshotter pods; empty disables the cluster view")
	fs.StringVar(&c.MembersNamespace, "members-namespace", envOr("GANTRY_SNAPSHOTTER_MEMBERS_NAMESPACE", ""), "namespace to restrict the peer pod informer to")
	fs.StringVar(&c.Kubeconfig, "kubeconfig", envOr("GANTRY_SNAPSHOTTER_KUBECONFIG", ""), "out-of-cluster kubeconfig for the peer view")
	fs.StringVar(&c.ZoneLabel, "zone-label", envOr("GANTRY_SNAPSHOTTER_ZONE_LABEL", ""), "node label carrying the topology zone")
	fs.DurationVar(&c.ElectionStep, "election-step", envDuration("GANTRY_SNAPSHOTTER_ELECTION_STEP", ingest.DefaultStep), "delay added per rendezvous rank before ingesting a layer")
	fs.DurationVar(&c.ElectionMaxDelay, "election-max-delay", envDuration("GANTRY_SNAPSHOTTER_ELECTION_MAX_DELAY", ingest.DefaultMaxDelay), "cap on the ingest election delay")
	fs.IntVar(&c.IngestWorkers, "ingest-workers", envInt("GANTRY_SNAPSHOTTER_INGEST_WORKERS", ingest.DefaultWorkers), "concurrent layer ingests")
	fs.IntVar(&c.IngestDepth, "ingest-depth", envInt("GANTRY_SNAPSHOTTER_INGEST_DEPTH", ingest.DefaultQueueDepth), "ingest queue depth")
	fs.DurationVar(&c.IngestRetry, "ingest-retry", envDuration("GANTRY_SNAPSHOTTER_INGEST_RETRY", ingest.DefaultRetryDelay), "delay before a failed ingest is retried once")
	fs.BoolVar(&c.SkipVerify, "skip-verify", envBool("GANTRY_SNAPSHOTTER_SKIP_VERIFY", false), "skip the read-back check after writing a layer")
	fs.StringVar(&c.MkfsErofs, "mkfs-erofs", envOr("GANTRY_SNAPSHOTTER_MKFS_EROFS", ingest.DefaultBinary), "erofs image builder binary")
	fs.StringVar(&mountOptions, "mount-options", envOr("GANTRY_SNAPSHOTTER_MOUNT_OPTIONS", ""), "comma-separated extra overlayfs options")
	fs.StringVar(&c.MapRoot, "map-root", envOr("GANTRY_SNAPSHOTTER_MAP_ROOT", blockmap.DefaultRoot), "directory layer mounts are placed under")
	fs.DurationVar(&c.CleanupInterval, "cleanup-interval", envDuration("GANTRY_SNAPSHOTTER_CLEANUP_INTERVAL", DefaultCleanup), "how often to sweep orphan directories and stale mappings")
	fs.DurationVar(&c.CatalogSync, "catalog-sync", envDuration("GANTRY_SNAPSHOTTER_CATALOG_SYNC", DefaultCatalogSync), "background catalog poll interval")
	fs.DurationVar(&c.HoleGrace, "hole-grace", envDuration("GANTRY_SNAPSHOTTER_HOLE_GRACE", catalog.DefaultHoleGrace), "how long an unwritten catalog record slot persists before it is retired")
	fs.DurationVar(&c.ShutdownGrace, "shutdown-grace", envDuration("GANTRY_SNAPSHOTTER_SHUTDOWN_GRACE", DefaultShutdownGrace), "graceful shutdown budget")
	fs.StringVar(&c.LogLevel, "log-level", envOr("GANTRY_SNAPSHOTTER_LOG_LEVEL", "info"), "debug, info, warn or error")
	fs.StringVar(&c.LogFormat, "log-format", envOr("GANTRY_SNAPSHOTTER_LOG_FORMAT", "text"), "text or json")
	fs.StringVar(&c.MetricsAddr, "metrics-addr", envOr("GANTRY_SNAPSHOTTER_METRICS_ADDR", ""), "address to serve metrics and the liveness endpoint on; empty disables it")
	fs.BoolVar(&c.EnablePprof, "pprof", envBool("GANTRY_SNAPSHOTTER_PPROF", false), "expose net/http/pprof on the metrics listener")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	mode, err := strconv.ParseUint(socketMode, 8, 32)
	if err != nil {
		return nil, fmt.Errorf("socket-mode %q: %w", socketMode, err)
	}

	if segmentBlocks < 1 || segmentBlocks > 4096 {
		return nil, fmt.Errorf("segment-blocks %d: want 1..4096", segmentBlocks)
	}

	c.SegmentBlocks = uint32(segmentBlocks)
	c.SocketMode = os.FileMode(mode)
	c.MountOptions = splitOptions(mountOptions)

	if err := c.validate(); err != nil {
		return nil, err
	}

	return c, nil
}

// validate rejects a configuration that cannot work, rather than one that is
// merely unusual. In particular an empty node name is allowed: it only means
// the ingest election has no opinion and every node is eager, which is correct
// on a single node.
func (c *Config) validate() error {
	switch {
	case c.Socket == "":
		return errors.New("socket required")
	case c.Root == "":
		return errors.New("root required")
	case c.WorkDir == "":
		return errors.New("work-dir required")
	case c.Devices == "":
		return errors.New("devices required")
	case c.MapRoot == "":
		return errors.New("map-root required")
	case c.IngestWorkers < 1:
		return errors.New("ingest-workers must be at least 1")
	case c.IngestDepth < 1:
		return errors.New("ingest-depth must be at least 1")
	case c.MembersSelector != "" && c.NodeName == "":
		return errors.New("node-name required when members-selector is set")
	}

	if _, err := parseLevel(c.LogLevel); err != nil {
		return err
	}

	switch c.LogFormat {
	case "text", "json":
	default:
		return fmt.Errorf("log-format %q: want text or json", c.LogFormat)
	}

	return nil
}

func splitOptions(s string) []string {
	var out []string

	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}

	return out
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}

	return fallback
}

func envBool(key string, fallback bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}

	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}

	return b
}

func envInt(key string, fallback int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}

	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}

	return n
}

func envUint64(key string, fallback uint64) uint64 {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}

	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return fallback
	}

	return n
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}

	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}

	return d
}
