// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package storagesupervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"

	storageconfig "github.com/Azure/unbounded/api/unbounded-storage"
)

// RenderConfig reads the startup-tuning ConfigMap projected one file per
// dotted key under sourceDir and renders it into the daemon's binary
// protobuf config wire format.
//
// Only startup-fixed settings (version + the startup.{memory,fabric,topology,
// metrics} sections) are sourced here; per-node sections are injected
// elsewhere. A missing key file leaves its proto3 zero value in place, which
// the daemon promotes to the documented default. A present file with an
// unparseable value is a hard error so an operator typo fails loudly rather
// than silently reverting a field to its default.
func RenderConfig(sourceDir string) ([]byte, error) {
	cfg := &storageconfig.Config{
		Startup: &storageconfig.StartupCfg{
			Memory:   &storageconfig.MemoryCfg{},
			Fabric:   &storageconfig.FabricCfg{},
			Topology: &storageconfig.TopologyCfg{},
			Metrics:  &storageconfig.MetricsCfg{},
		},
	}

	r := sourceReader{dir: sourceDir}

	r.u64("version", &cfg.Version)

	r.boolean("startup.memory.no_hugepages", &cfg.Startup.Memory.NoHugepages)
	r.u64("startup.memory.memory_total_bytes", &cfg.Startup.Memory.MemoryTotalBytes)

	r.str("startup.fabric.listen_addr", &cfg.Startup.Fabric.ListenAddr)
	r.u32("startup.fabric.progress_threads", &cfg.Startup.Fabric.ProgressThreads)
	r.u32("startup.fabric.progress_poll_us", &cfg.Startup.Fabric.ProgressPollUs)
	r.u32("startup.fabric.rpc_worker_threads", &cfg.Startup.Fabric.RpcWorkerThreads)
	r.u32("startup.fabric.max_inflight", &cfg.Startup.Fabric.MaxInflight)

	r.boolean("startup.topology.use_smt_siblings", &cfg.Startup.Topology.UseSmtSiblings)
	r.boolean("startup.topology.ignore_isolated", &cfg.Startup.Topology.IgnoreIsolated)
	r.boolean("startup.topology.include_node_cpu0", &cfg.Startup.Topology.IncludeNodeCpu0)
	r.boolean("startup.topology.allow_inactive_port", &cfg.Startup.Topology.AllowInactivePort)
	r.boolean("startup.topology.disable_rdma", &cfg.Startup.Topology.DisableRdma)
	r.u64("startup.topology.serving_cores", &cfg.Startup.Topology.ServingCores)
	r.u64("startup.topology.nic_workers", &cfg.Startup.Topology.NicWorkers)

	r.str("startup.metrics.bind", &cfg.Startup.Metrics.Bind)

	if r.err != nil {
		return nil, r.err
	}

	out, err := proto.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal config protobuf: %w", err)
	}

	return out, nil
}

// sourceReader reads typed values from per-key files under dir, accumulating
// the first error so a chain of reads can be expressed without a per-call
// error check. A key whose file is absent is left at its zero value.
type sourceReader struct {
	dir string
	err error
}

// read returns the trimmed contents of the key file, ok=false when the file
// is absent. A missing key is not an error: the proto3 zero value stands and
// the daemon applies the documented default.
func (r *sourceReader) read(key string) (string, bool) {
	if r.err != nil {
		return "", false
	}

	b, err := os.ReadFile(filepath.Join(r.dir, key))
	if err != nil {
		if os.IsNotExist(err) {
			return "", false
		}

		r.err = fmt.Errorf("read config key %q: %w", key, err)

		return "", false
	}

	return strings.TrimSpace(string(b)), true
}

func (r *sourceReader) str(key string, dst *string) {
	if v, ok := r.read(key); ok {
		*dst = v
	}
}

func (r *sourceReader) boolean(key string, dst *bool) {
	v, ok := r.read(key)
	if !ok {
		return
	}

	parsed, err := strconv.ParseBool(v)
	if err != nil {
		r.err = fmt.Errorf("config key %q: invalid bool %q", key, v)

		return
	}

	*dst = parsed
}

func (r *sourceReader) u32(key string, dst *uint32) {
	v, ok := r.read(key)
	if !ok {
		return
	}

	parsed, err := strconv.ParseUint(v, 10, 32)
	if err != nil {
		r.err = fmt.Errorf("config key %q: invalid uint32 %q", key, v)

		return
	}

	*dst = uint32(parsed)
}

func (r *sourceReader) u64(key string, dst *uint64) {
	v, ok := r.read(key)
	if !ok {
		return
	}

	parsed, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		r.err = fmt.Errorf("config key %q: invalid uint64 %q", key, v)

		return
	}

	*dst = parsed
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
