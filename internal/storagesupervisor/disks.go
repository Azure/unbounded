// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package storagesupervisor

import (
	"log/slog"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"

	storageconfig "github.com/Azure/unbounded/api/unbounded-storage"
)

const (
	storageDisksAnnotation     = "unbounded-cloud.io/storage-disks"
	storageFileSizeAnnotation  = "unbounded-cloud.io/storage-file-size-bytes"
	defaultStorageFileDiskPath = "/var/lib/unbounded-storage/cache.disk"
	defaultStorageFileDiskDir  = "/var/lib/unbounded-storage"
	defaultStorageFileDiskSize = uint64(2 * 1024 * 1024 * 1024)
	defaultStorageDiskPageSize = 4096
	diskOptionQueueDepth       = "queue_depth"
	diskOptionPageSizeBytes    = "page_size_bytes"
	diskOptionSkipRecoveryScan = "skip_recovery_scan"
	diskOptionNuma             = "numa"
)

// applyDiskOverlay injects per-node storage disks when the config does not
// declare disks explicitly. Explicit config disks are authoritative.
func applyDiskOverlay(cfg *storageconfig.Config, annotations map[string]string) error {
	if len(cfg.GetDisks()) > 0 {
		return nil
	}

	existingPaths := declaredDiskPaths(cfg)

	disks := annotationBlockDisks(annotations[storageDisksAnnotation], existingPaths)
	if len(disks) == 0 {
		fallback := fallbackFileDisk(annotations[storageFileSizeAnnotation])

		path := fallback.GetFile().GetPath()
		if _, exists := existingPaths[path]; exists {
			slog.Warn("skipping fallback storage file disk because path is already declared", "path", path)

			return nil
		}

		disks = []*storageconfig.DiskSpec{fallback}
	}

	cfg.Disks = disks

	return nil
}

func annotationBlockDisks(raw string, existingPaths map[string]struct{}) []*storageconfig.DiskSpec {
	entries := strings.Split(raw, ",")
	disks := make([]*storageconfig.DiskSpec, 0, len(entries))
	seenPaths := make(map[string]struct{}, len(entries))

	for _, entry := range entries {
		disk := annotationBlockDisk(entry)
		if disk == nil {
			continue
		}

		path := disk.GetBlock().GetPath()
		if _, exists := existingPaths[path]; exists {
			slog.Warn("skipping annotated storage disk because path is already declared", "path", path)

			continue
		}

		if _, exists := seenPaths[path]; exists {
			slog.Warn("skipping duplicate annotated storage disk path", "path", path)

			continue
		}

		seenPaths[path] = struct{}{}

		disks = append(disks, disk)
	}

	return disks
}

func annotationBlockDisk(raw string) *storageconfig.DiskSpec {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	parts := strings.Split(raw, ";")

	path := strings.TrimSpace(parts[0])
	if path == "" {
		slog.Warn("skipping annotated storage disk with empty path", "value", raw)

		return nil
	}

	if !strings.HasPrefix(path, "/") {
		slog.Warn("skipping annotated storage disk with non-absolute path", "path", path)

		return nil
	}

	disk := &storageconfig.DiskSpec{
		Config: &storageconfig.DiskSpec_Block{
			Block: &storageconfig.BlockDiskConfig{Path: path},
		},
	}
	seenOptions := make(map[string]struct{}, len(parts)-1)

	for _, part := range parts[1:] {
		applyDiskOption(disk, strings.TrimSpace(part), seenOptions, path)
	}

	return disk
}

func applyDiskOption(disk *storageconfig.DiskSpec, raw string, seen map[string]struct{}, path string) {
	if raw == "" {
		slog.Warn("ignoring empty storage disk option", "path", path)

		return
	}

	key, value, ok := strings.Cut(raw, "=")
	if !ok {
		slog.Warn("ignoring storage disk option without key=value", "path", path, "option", raw)

		return
	}

	key = strings.TrimSpace(key)

	value = strings.TrimSpace(value)
	if key == "" || value == "" {
		slog.Warn("ignoring storage disk option with empty key or value", "path", path, "option", raw)

		return
	}

	switch key {
	case diskOptionQueueDepth:
		if isDuplicateDiskOption(seen, path, key) {
			return
		}

		v, ok := parsePositiveUint32Option(path, key, value)
		if ok {
			disk.QueueDepth = proto.Uint32(v)
			seen[key] = struct{}{}
		}
	case diskOptionPageSizeBytes:
		if isDuplicateDiskOption(seen, path, key) {
			return
		}

		v, ok := parsePositiveUint64Option(path, key, value)
		if ok {
			disk.PageSizeBytes = proto.Uint64(v)
			seen[key] = struct{}{}
		}
	case diskOptionSkipRecoveryScan:
		if isDuplicateDiskOption(seen, path, key) {
			return
		}

		v, err := strconv.ParseBool(value)
		if err != nil {
			slog.Warn("ignoring invalid storage disk bool option", "path", path, "key", key, "value", value)

			return
		}

		disk.SkipRecoveryScan = v
		seen[key] = struct{}{}
	case diskOptionNuma:
		if isDuplicateDiskOption(seen, path, key) {
			return
		}

		v, ok := parseUint32Option(path, key, value)
		if ok {
			disk.GetBlock().Numa = proto.Uint32(v)
			seen[key] = struct{}{}
		}
	default:
		slog.Warn("ignoring unknown storage disk option", "path", path, "key", key)
	}
}

func isDuplicateDiskOption(seen map[string]struct{}, path, key string) bool {
	if _, exists := seen[key]; exists {
		slog.Warn("ignoring duplicate storage disk option", "path", path, "key", key)

		return true
	}

	return false
}

func parsePositiveUint32Option(path, key, raw string) (uint32, bool) {
	v, ok := parseUint32Option(path, key, raw)
	if !ok {
		return 0, false
	}

	if v == 0 {
		slog.Warn("ignoring zero storage disk option", "path", path, "key", key)

		return 0, false
	}

	return v, true
}

func parseUint32Option(path, key, raw string) (uint32, bool) {
	v, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		slog.Warn("ignoring invalid storage disk uint32 option", "path", path, "key", key, "value", raw)

		return 0, false
	}

	return uint32(v), true
}

func parsePositiveUint64Option(path, key, raw string) (uint64, bool) {
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		slog.Warn("ignoring invalid storage disk uint64 option", "path", path, "key", key, "value", raw)

		return 0, false
	}

	if v == 0 {
		slog.Warn("ignoring zero storage disk option", "path", path, "key", key)

		return 0, false
	}

	return v, true
}

func fallbackFileDisk(rawSize string) *storageconfig.DiskSpec {
	size := defaultStorageFileDiskSize

	if strings.TrimSpace(rawSize) != "" {
		parsed, err := strconv.ParseUint(strings.TrimSpace(rawSize), 10, 64)
		if err != nil || parsed == 0 || parsed%defaultStorageDiskPageSize != 0 {
			slog.Warn("using default storage file disk size because annotation is invalid",
				"annotation", storageFileSizeAnnotation, "value", rawSize, "default", defaultStorageFileDiskSize)
		} else {
			size = parsed
		}
	}

	return &storageconfig.DiskSpec{
		Config: &storageconfig.DiskSpec_File{
			File: &storageconfig.FileDiskConfig{
				Path: defaultStorageFileDiskPath,
				Size: proto.Uint64(size),
			},
		},
	}
}

func declaredDiskPaths(cfg *storageconfig.Config) map[string]struct{} {
	paths := map[string]struct{}{}

	for _, disk := range cfg.GetDisks() {
		path := diskPath(disk)
		if path != "" {
			paths[path] = struct{}{}
		}
	}

	return paths
}

func diskPath(disk *storageconfig.DiskSpec) string {
	if disk == nil {
		return ""
	}

	if block := disk.GetBlock(); block != nil {
		return block.GetPath()
	}

	if file := disk.GetFile(); file != nil {
		return file.GetPath()
	}

	return ""
}
