// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package storagesupervisor

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"sigs.k8s.io/yaml"

	storageconfig "github.com/Azure/unbounded/api/unbounded-storage"
)

// RenderConfig reads the daemon Config expressed as YAML from the projected
// ConfigMap (a single sourceConfigFile under sourceDir), unmarshals it into the
// protobuf Config message, overlays the per-node ring state, and returns the
// daemon's binary protobuf config wire format.
//
// The YAML is the full Config schema (api/unbounded-storage/config.proto) with
// snake_case field names. Unknown fields are rejected so an operator typo fails
// loudly rather than silently dropping a setting. Any field left unset keeps its
// proto3 zero value, which the daemon promotes to the documented default.
//
// The per-node sections (neighborhood local_node_id and discovered peer sets)
// come from ring, computed from the Kubernetes node watch. When the ring is
// active, this node's id is injected into every declared neighborhood,
// discovered peers are merged with any neighborhood peers declared in the YAML
// (discovered peers win on id collision), and startup.fabric.tcp.addr is
// overridden with the node's own routable bind. When the ring is inactive (no
// node watch, no ring membership, or no fixed fabric port) the YAML is rendered
// as-is, including any hand-declared neighborhoods/peers.
func RenderConfig(sourceDir string, ring ringState) ([]byte, error) {
	cfg, err := loadSourceConfig(sourceDir)
	if err != nil {
		return nil, err
	}

	if ring.active {
		applyRing(cfg, ring)
	}

	if err := validateConfigGraph(cfg); err != nil {
		return nil, fmt.Errorf("invalid config graph: %w", err)
	}

	out, err := proto.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal config protobuf: %w", err)
	}

	return out, nil
}

// validateConfigGraph mirrors the daemon's binding-graph validation for source
// relationships that can be expressed by the ConfigMap renderer. This catches
// stale keyspace graph configs before the supervisor publishes them.
func validateConfigGraph(cfg *storageconfig.Config) error {
	ids := map[string]string{}
	addID := func(kind, name string) error {
		if name == "" {
			return nil
		}

		if existing, ok := ids[name]; ok {
			return fmt.Errorf("%s %q duplicates %s", kind, name, existing)
		}

		ids[name] = kind

		return nil
	}

	backends := map[string]struct{}{}

	for _, backend := range cfg.GetBackends() {
		if backend == nil {
			continue
		}

		if err := addID("backend", backend.GetName()); err != nil {
			return err
		}

		backends[backend.GetName()] = struct{}{}
	}

	keyspaces := map[string]struct{}{}

	for _, keyspace := range cfg.GetKeyspaces() {
		if keyspace == nil {
			continue
		}

		if err := addID("keyspace", keyspace.GetName()); err != nil {
			return err
		}

		keyspaces[keyspace.GetName()] = struct{}{}
	}

	caches := map[string]struct{}{}

	for _, cache := range cfg.GetCaches() {
		if cache == nil {
			continue
		}

		if err := addID("cache", cache.GetName()); err != nil {
			return err
		}

		caches[cache.GetName()] = struct{}{}
	}

	neighborhoods := map[string]struct{}{}

	for _, neighborhood := range cfg.GetNeighborhoods() {
		if neighborhood == nil {
			continue
		}

		if err := addID("neighborhood", neighborhood.GetName()); err != nil {
			return err
		}

		neighborhoods[neighborhood.GetName()] = struct{}{}
	}

	for _, frontend := range cfg.GetFrontends() {
		if frontend == nil {
			continue
		}

		if err := addID("frontend", frontend.GetName()); err != nil {
			return err
		}
	}

	for _, keyspace := range cfg.GetKeyspaces() {
		if keyspace == nil {
			continue
		}

		if len(keyspace.GetRoutes()) == 0 {
			return fmt.Errorf("keyspace %q: at least one route is required", keyspace.GetName())
		}

		for _, route := range keyspace.GetRoutes() {
			if route == nil {
				continue
			}

			if _, ok := backends[route.GetBackend()]; !ok {
				return fmt.Errorf("keyspace %q route %q backend %q, which is not a backend", keyspace.GetName(), route.GetKeyPrefix(), route.GetBackend())
			}
		}
	}

	for _, cache := range cfg.GetCaches() {
		if cache == nil {
			continue
		}

		_, keyspace := keyspaces[cache.GetSource()]
		_, neighborhood := neighborhoods[cache.GetSource()]

		if !keyspace && !neighborhood {
			return fmt.Errorf("cache %q source %q, which is not a keyspace or neighborhood", cache.GetName(), cache.GetSource())
		}
	}

	for _, neighborhood := range cfg.GetNeighborhoods() {
		if neighborhood == nil {
			continue
		}

		if _, ok := keyspaces[neighborhood.GetSource()]; !ok {
			return fmt.Errorf("neighborhood %q source %q, which is not a keyspace", neighborhood.GetName(), neighborhood.GetSource())
		}
	}

	for _, frontend := range cfg.GetFrontends() {
		if frontend == nil {
			continue
		}

		_, backend := backends[frontend.GetSource()]
		_, keyspace := keyspaces[frontend.GetSource()]
		_, cache := caches[frontend.GetSource()]
		_, neighborhood := neighborhoods[frontend.GetSource()]

		if !backend && !keyspace && !cache && !neighborhood {
			return fmt.Errorf("frontend %q source %q, which is not a backend, keyspace, cache, or neighborhood", frontend.GetName(), frontend.GetSource())
		}
	}

	return nil
}

// loadSourceConfig reads the YAML Config document projected from the ConfigMap
// and unmarshals it into a protobuf Config message. An absent or empty file
// yields an empty Config (every field at its proto3 zero value); a present but
// malformed document, or one carrying an unknown field, is a hard error.
func loadSourceConfig(sourceDir string) (*storageconfig.Config, error) {
	path := filepath.Join(sourceDir, sourceConfigFile)

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &storageconfig.Config{}, nil
		}

		return nil, fmt.Errorf("read config source %q: %w", path, err)
	}

	if len(strings.TrimSpace(string(raw))) == 0 {
		return &storageconfig.Config{}, nil
	}

	jsonBytes, err := yaml.YAMLToJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("convert config %q yaml to json: %w", path, err)
	}

	cfg := &storageconfig.Config{}

	// DiscardUnknown defaults to false, so an unknown field (typically an
	// operator typo) is rejected rather than silently ignored.
	if err := protojson.Unmarshal(jsonBytes, cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config %q: %w", path, err)
	}

	return cfg, nil
}

// applyRing overlays the per-node ring state onto a Config parsed from the
// ConfigMap YAML. It injects this node's local id into declared neighborhoods,
// merges the discovered peer set with each neighborhood's declared peers, and
// rebinds the TCP fabric address to the node's own routable address.
func applyRing(cfg *storageconfig.Config, ring ringState) {
	injected := false

	for _, neighborhood := range cfg.Neighborhoods {
		if neighborhood == nil {
			continue
		}

		injected = true

		// Preserve YAML-declared neighborhood scalars (fingers_per_node,
		// local_tags, routing_plan); only stamp in the locally computed node id.
		neighborhood.LocalNodeId = proto.Uint64(ring.localNodeID)
		neighborhood.Peers = mergePeers(neighborhood.Peers, ring.peers, ring.localNodeID)
	}

	if !injected {
		slog.Warn("ring discovery active but config declares no neighborhoods; discovered storage peers were not injected")
	}

	if ring.selfListenAddr != "" {
		if cfg.Startup == nil {
			cfg.Startup = &storageconfig.StartupCfg{}
		}

		if cfg.Startup.Fabric == nil {
			cfg.Startup.Fabric = &storageconfig.FabricCfg{}
		}

		cfg.Startup.Fabric.Binds = &storageconfig.FabricCfg_Tcp{
			Tcp: &storageconfig.TcpFabricBinds{Addr: ring.selfListenAddr},
		}
	}
}

// mergePeers combines the label-discovered peer set with any peers declared in
// the ConfigMap YAML, deduplicated by peer id. Discovered peers are
// authoritative: a YAML peer whose id collides with a discovered peer is
// dropped. Any peer whose id equals the local node id is dropped (the daemon's
// validate() rejects a self-peer, and discovery already excludes self). The
// result is sorted by id so an unchanged input renders byte-for-byte
// identically and never triggers a spurious config swap.
func mergePeers(declared, discovered []*storageconfig.PeerSpec, localNodeID uint64) []*storageconfig.PeerSpec {
	byID := make(map[uint64]*storageconfig.PeerSpec, len(declared)+len(discovered))

	add := func(peers []*storageconfig.PeerSpec, fromYAML bool) {
		for _, p := range peers {
			if p == nil {
				continue
			}

			if p.GetId() == localNodeID {
				slog.Warn("dropping peer whose id equals local node id", "id", p.GetId())

				continue
			}

			if _, ok := byID[p.GetId()]; ok {
				if fromYAML {
					slog.Warn("dropping declared peer; id already discovered", "id", p.GetId())
				}

				continue
			}

			byID[p.GetId()] = p
		}
	}

	// Discovered first so they win id collisions against declared peers.
	add(discovered, false)
	add(declared, true)

	merged := make([]*storageconfig.PeerSpec, 0, len(byID))
	for _, p := range byID {
		merged = append(merged, p)
	}

	sort.Slice(merged, func(i, j int) bool { return merged[i].GetId() < merged[j].GetId() })

	return merged
}

// WriteConfigAtomic writes data to path via a temp file in the same directory
// followed by a rename, so the daemon's parent-directory config watcher only
// ever observes a complete file appearing as an atomic swap. The destination
// directory must already exist (install bootstraps it).
func WriteConfigAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp config in %q: %w", dir, err)
	}

	tmpName := tmp.Name()

	cleanup := func() {
		_ = tmp.Close()        //nolint:errcheck
		_ = os.Remove(tmpName) //nolint:errcheck
	}

	if _, err := tmp.Write(data); err != nil {
		cleanup()

		return fmt.Errorf("write temp config %q: %w", tmpName, err)
	}

	if err := tmp.Sync(); err != nil {
		cleanup()

		return fmt.Errorf("sync temp config %q: %w", tmpName, err)
	}

	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName) //nolint:errcheck

		return fmt.Errorf("close temp config %q: %w", tmpName, err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName) //nolint:errcheck

		return fmt.Errorf("rename %q to %q: %w", tmpName, path, err)
	}

	return nil
}
