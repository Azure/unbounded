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

type renderState struct {
	ring        ringState
	annotations map[string]string
}

// RenderConfig reads the daemon Config expressed as YAML from the projected
// ConfigMap (a single sourceConfigFile under sourceDir), unmarshals it into the
// protobuf Config message, overlays the per-node render state, and returns the
// daemon's binary protobuf config wire format.
//
// The YAML is the full Config schema (api/unbounded-storage/config.proto) with
// snake_case field names. Unknown fields are rejected so an operator typo fails
// loudly rather than silently dropping a setting. Any field left unset keeps its
// proto3 zero value, which the daemon promotes to the documented default.
//
// The per-node mesh fields (self and discovered peer set) come from state,
// computed from the Kubernetes node watch. When the ring is active, this node's
// peer name is injected and discovered peers are merged with any peers declared
// in the YAML (discovered peers win on name collision). TCP rings also override
// startup.fabric.tcp.addr with the node's own routable bind. The default disk
// pool is populated from the self node's storage disk annotations, or from a
// default file-backed disk when no valid annotation disks are present.
func RenderConfig(sourceDir string, state renderState) ([]byte, error) {
	cfg, err := loadSourceConfig(sourceDir)
	if err != nil {
		return nil, err
	}

	if state.ring.active {
		applyRing(cfg, state.ring)
	}

	if err := applyDiskOverlay(cfg, state.annotations); err != nil {
		return nil, err
	}

	out, err := proto.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal config protobuf: %w", err)
	}

	return out, nil
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
// ConfigMap YAML. It injects this node's self peer name, merges the discovered
// peer set with any declared peers, and rebinds the TCP fabric address to the
// node's own routable address when the ring includes a TCP selfListenAddr.
func applyRing(cfg *storageconfig.Config, ring ringState) {
	// Preserve YAML-declared routing knobs (fingers_per_node, routing_plan);
	// only stamp in the locally computed self name and peer roster.
	cfg.Self = ring.selfName
	cfg.Peers = mergePeers(cfg.Peers, ring.peers)

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
// the ConfigMap YAML, deduplicated by peer name. Discovered peers are
// authoritative: a YAML peer whose name collides with a discovered peer is
// dropped. The result is sorted by name so an unchanged input renders
// byte-for-byte identically and never triggers a spurious config swap.
func mergePeers(declared, discovered []*storageconfig.PeerSpec) []*storageconfig.PeerSpec {
	byName := make(map[string]*storageconfig.PeerSpec, len(declared)+len(discovered))

	add := func(peers []*storageconfig.PeerSpec, fromYAML bool) {
		for _, p := range peers {
			if p == nil {
				continue
			}

			name := p.GetName()
			if name == "" {
				slog.Warn("dropping storage peer with empty name")

				continue
			}

			if _, ok := byName[name]; ok {
				if fromYAML {
					slog.Warn("dropping declared peer; name already discovered", "name", name)
				}

				continue
			}

			byName[name] = p
		}
	}

	// Discovered first so they win name collisions against declared peers.
	add(discovered, false)
	add(declared, true)

	merged := make([]*storageconfig.PeerSpec, 0, len(byName))
	for _, p := range byName {
		merged = append(merged, p)
	}

	sort.Slice(merged, func(i, j int) bool { return merged[i].GetName() < merged[j].GetName() })

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
