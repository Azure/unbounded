// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package storagesupervisor

import (
	"log/slog"
	"math"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"

	storageconfig "github.com/Azure/unbounded/api/unbounded-storage"
)

const (
	storageLoadgenAnnotation = "unbounded-cloud.io/storage-loadgen"

	loadgenOptionSource          = "source"
	loadgenOptionWorkers         = "workers"
	loadgenOptionSeed            = "seed"
	loadgenOptionKeyspaceObjects = "keyspace_objects"
	loadgenOptionObjectSizeBytes = "object_size_bytes"
	loadgenOptionReadBytes       = "read_bytes"
	loadgenOptionZipfExponent    = "zipf_exponent"
	loadgenOptionVerify          = "verify"
	loadgenOptionRemoteOnly      = "remote_only"
	loadgenOptionFabricOnly      = "fabric_only"
	loadgenOptionLocalOnly       = "local_only"
	loadgenOptionSkipLocalDisk   = "skip_local_disk"
)

func applyLoadgenOverlay(cfg *storageconfig.Config, annotations map[string]string) {
	frontends := annotationLoadgenFrontends(annotations[storageLoadgenAnnotation], declaredFrontendNames(cfg))
	if len(frontends) == 0 {
		return
	}

	cfg.Frontends = append(cfg.Frontends, frontends...)
}

func annotationLoadgenFrontends(raw string, existingNames map[string]struct{}) []*storageconfig.FrontendSpec {
	entries := strings.Split(raw, ",")
	frontends := make([]*storageconfig.FrontendSpec, 0, len(entries))
	seenNames := make(map[string]struct{}, len(entries))

	for _, entry := range entries {
		frontend := annotationLoadgenFrontend(entry)
		if frontend == nil {
			continue
		}

		name := frontend.GetName()
		if _, exists := existingNames[name]; exists {
			slog.Warn("skipping annotated storage loadgen because frontend name is already declared", "name", name)

			continue
		}

		if _, exists := seenNames[name]; exists {
			slog.Warn("skipping duplicate annotated storage loadgen frontend", "name", name)

			continue
		}

		seenNames[name] = struct{}{}

		frontends = append(frontends, frontend)
	}

	return frontends
}

func annotationLoadgenFrontend(raw string) *storageconfig.FrontendSpec {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	parts := strings.Split(raw, ";")

	name := strings.TrimSpace(parts[0])
	if name == "" {
		slog.Warn("skipping annotated storage loadgen with empty name", "value", raw)

		return nil
	}

	frontend := &storageconfig.FrontendSpec{
		Name: name,
		Config: &storageconfig.FrontendSpec_Loadgen{
			Loadgen: &storageconfig.LoadgenFrontendConfig{},
		},
	}
	seenOptions := make(map[string]struct{}, len(parts)-1)

	for _, part := range parts[1:] {
		applyLoadgenOption(frontend, strings.TrimSpace(part), seenOptions, name)
	}

	if frontend.GetSource() == "" {
		slog.Warn("skipping annotated storage loadgen without source", "name", name)

		return nil
	}

	return frontend
}

func applyLoadgenOption(frontend *storageconfig.FrontendSpec, raw string, seen map[string]struct{}, name string) {
	if raw == "" {
		slog.Warn("ignoring empty storage loadgen option", "name", name)

		return
	}

	key, value, ok := strings.Cut(raw, "=")
	if !ok {
		slog.Warn("ignoring storage loadgen option without key=value", "name", name, "option", raw)

		return
	}

	key = strings.TrimSpace(key)

	value = strings.TrimSpace(value)
	if key == "" || value == "" {
		slog.Warn("ignoring storage loadgen option with empty key or value", "name", name, "option", raw)

		return
	}

	if _, exists := seen[key]; exists {
		slog.Warn("ignoring duplicate storage loadgen option", "name", name, "key", key)

		return
	}

	loadgen := frontend.GetLoadgen()

	switch key {
	case loadgenOptionSource:
		frontend.Source = value
	case loadgenOptionWorkers:
		v, ok := parseLoadgenUint32Option(name, key, value)
		if !ok {
			return
		}

		loadgen.Workers = proto.Uint32(v)
	case loadgenOptionSeed:
		v, ok := parseLoadgenUint64Option(name, key, value)
		if !ok {
			return
		}

		loadgen.Seed = proto.Uint64(v)
	case loadgenOptionKeyspaceObjects:
		v, ok := parseLoadgenUint64Option(name, key, value)
		if !ok {
			return
		}

		loadgen.KeyspaceObjects = proto.Uint64(v)
	case loadgenOptionObjectSizeBytes:
		v, ok := parseLoadgenUint64Option(name, key, value)
		if !ok {
			return
		}

		loadgen.ObjectSizeBytes = proto.Uint64(v)
	case loadgenOptionReadBytes:
		v, ok := parseLoadgenUint64Option(name, key, value)
		if !ok {
			return
		}

		loadgen.ReadBytes = proto.Uint64(v)
	case loadgenOptionZipfExponent:
		v, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
			slog.Warn("ignoring invalid storage loadgen float option", "name", name, "key", key, "value", value)

			return
		}

		loadgen.ZipfExponent = proto.Float64(v)
	case loadgenOptionVerify:
		v, err := strconv.ParseBool(value)
		if err != nil {
			slog.Warn("ignoring invalid storage loadgen bool option", "name", name, "key", key, "value", value)

			return
		}

		loadgen.Verify = v
	case loadgenOptionRemoteOnly:
		v, err := strconv.ParseBool(value)
		if err != nil {
			slog.Warn("ignoring invalid storage loadgen bool option", "name", name, "key", key, "value", value)

			return
		}

		loadgen.RemoteOnly = v
	case loadgenOptionFabricOnly:
		v, err := strconv.ParseBool(value)
		if err != nil {
			slog.Warn("ignoring invalid storage loadgen bool option", "name", name, "key", key, "value", value)

			return
		}

		loadgen.FabricOnly = v
	case loadgenOptionLocalOnly:
		v, err := strconv.ParseBool(value)
		if err != nil {
			slog.Warn("ignoring invalid storage loadgen bool option", "name", name, "key", key, "value", value)

			return
		}

		loadgen.LocalOnly = v
	case loadgenOptionSkipLocalDisk:
		v, err := strconv.ParseBool(value)
		if err != nil {
			slog.Warn("ignoring invalid storage loadgen bool option", "name", name, "key", key, "value", value)

			return
		}

		loadgen.SkipLocalDisk = v
	default:
		slog.Warn("ignoring unknown storage loadgen option", "name", name, "key", key)

		return
	}

	seen[key] = struct{}{}
}

func parseLoadgenUint32Option(name, key, raw string) (uint32, bool) {
	v, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		slog.Warn("ignoring invalid storage loadgen uint32 option", "name", name, "key", key, "value", raw)

		return 0, false
	}

	return uint32(v), true
}

func parseLoadgenUint64Option(name, key, raw string) (uint64, bool) {
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		slog.Warn("ignoring invalid storage loadgen uint64 option", "name", name, "key", key, "value", raw)

		return 0, false
	}

	return v, true
}

func declaredFrontendNames(cfg *storageconfig.Config) map[string]struct{} {
	names := map[string]struct{}{}

	for _, frontend := range cfg.GetFrontends() {
		if frontend.GetName() != "" {
			names[frontend.GetName()] = struct{}{}
		}
	}

	return names
}
