// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package containerdconfig manages the containerd proxy snapshotter drop-in.
package containerdconfig

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

const importPattern = "/etc/containerd/conf.d/*.toml"

type Result struct {
	Changed   bool
	Preserved bool
}

type Manager struct {
	ConfigPath      string
	DropInPath      string
	StateDir        string
	Address         string
	SnapshotterName string
}

func (m Manager) Apply() (Result, error) {
	if err := m.validate(); err != nil {
		return Result{}, err
	}

	if err := os.MkdirAll(m.StateDir, 0o700); err != nil {
		return Result{}, fmt.Errorf("containerd config: create state directory: %w", err)
	}

	currentConfig, err := os.ReadFile(m.ConfigPath)
	if err != nil {
		return Result{}, fmt.Errorf("containerd config: read main config: %w", err)
	}

	currentDropIn, dropInExists, err := readOptional(m.DropInPath)
	if err != nil {
		return Result{}, err
	}

	_, managedStateExists, err := readOptional(m.statePath("managed"))
	if err != nil {
		return Result{}, err
	}

	managedConfig, managedConfigExists, err := readOptional(m.statePath("managed-config.toml"))
	if err != nil {
		return Result{}, err
	}

	managedDropIn, managedDropInExists, err := readOptional(m.statePath("managed-drop-in.toml"))
	if err != nil {
		return Result{}, err
	}

	if managedStateExists && (!managedConfigExists || !managedDropInExists) {
		return Result{}, errors.New("containerd config: incomplete managed state")
	}

	if managedStateExists && (!bytes.Equal(currentConfig, managedConfig) || !dropInExists || !bytes.Equal(currentDropIn, managedDropIn)) {
		return Result{Preserved: true}, errors.New("containerd config: managed files changed concurrently")
	}

	desiredConfig, err := ensureImport(currentConfig)
	if err != nil {
		return Result{}, err
	}

	desiredDropIn, err := marshalDropIn(currentConfig, m.Address, m.SnapshotterName)
	if err != nil {
		return Result{}, err
	}

	if managedStateExists && bytes.Equal(currentConfig, desiredConfig) && bytes.Equal(currentDropIn, desiredDropIn) {
		return Result{}, nil
	}

	originalConfig, originalConfigExists, err := readOptional(m.statePath("original-config.toml"))
	if err != nil {
		return Result{}, err
	}

	if !originalConfigExists {
		if err := atomicWrite(m.statePath("original-config.toml"), currentConfig, 0o600); err != nil {
			return Result{}, err
		}

		if dropInExists {
			if err := atomicWrite(m.statePath("original-drop-in.toml"), currentDropIn, 0o600); err != nil {
				return Result{}, err
			}
		} else if err := atomicWrite(m.statePath("original-drop-in-absent"), nil, 0o600); err != nil {
			return Result{}, err
		}
	} else if !managedStateExists {
		originalDesired, err := ensureImport(originalConfig)
		if err != nil {
			return Result{}, err
		}

		if !bytes.Equal(currentConfig, originalConfig) && !bytes.Equal(currentConfig, originalDesired) {
			return Result{Preserved: true}, errors.New("containerd config: main config changed during incomplete apply")
		}

		originalDropIn, originalDropInExists, err := readOptional(m.statePath("original-drop-in.toml"))
		if err != nil {
			return Result{}, err
		}

		_, originalAbsentExists, err := readOptional(m.statePath("original-drop-in-absent"))
		if err != nil {
			return Result{}, err
		}

		if !originalDropInExists && !originalAbsentExists {
			if dropInExists {
				if err := atomicWrite(m.statePath("original-drop-in.toml"), currentDropIn, 0o600); err != nil {
					return Result{}, err
				}

				originalDropIn = currentDropIn
				originalDropInExists = true
			} else if err := atomicWrite(m.statePath("original-drop-in-absent"), nil, 0o600); err != nil {
				return Result{}, err
			}
		}

		if dropInExists && !bytes.Equal(currentDropIn, desiredDropIn) && (!originalDropInExists || !bytes.Equal(currentDropIn, originalDropIn)) {
			return Result{Preserved: true}, errors.New("containerd config: drop-in changed during incomplete apply")
		}
	}

	if err := atomicWrite(m.ConfigPath, desiredConfig, 0o644); err != nil {
		return Result{}, fmt.Errorf("containerd config: write main config: %w", err)
	}

	if err := atomicWrite(m.DropInPath, desiredDropIn, 0o644); err != nil {
		return Result{}, fmt.Errorf("containerd config: write drop-in: %w", err)
	}

	if err := atomicWrite(m.statePath("managed-config.toml"), desiredConfig, 0o600); err != nil {
		return Result{}, err
	}

	if err := atomicWrite(m.statePath("managed-drop-in.toml"), desiredDropIn, 0o600); err != nil {
		return Result{}, err
	}

	if err := atomicWrite(m.statePath("managed"), nil, 0o600); err != nil {
		return Result{}, err
	}

	return Result{Changed: !bytes.Equal(currentConfig, desiredConfig) || !dropInExists || !bytes.Equal(currentDropIn, desiredDropIn)}, nil
}

func (m Manager) Restore() (Result, error) {
	if err := m.validate(); err != nil {
		return Result{}, err
	}

	_, managedStateExists, err := readOptional(m.statePath("managed"))
	if err != nil {
		return Result{}, err
	}

	if !managedStateExists {
		if _, originalExists, originalErr := readOptional(m.statePath("original-config.toml")); originalErr != nil {
			return Result{}, originalErr
		} else if originalExists {
			return Result{}, errors.New("containerd config: apply did not complete")
		}

		return Result{}, nil
	}

	managedConfig, managedConfigExists, err := readOptional(m.statePath("managed-config.toml"))
	if err != nil || !managedConfigExists {
		return Result{}, errors.New("containerd config: incomplete managed state")
	}

	managedDropIn, managedDropInExists, err := readOptional(m.statePath("managed-drop-in.toml"))
	if err != nil {
		return Result{}, err
	}

	if !managedDropInExists {
		return Result{}, errors.New("containerd config: incomplete managed state")
	}

	currentConfig, err := os.ReadFile(m.ConfigPath)
	if err != nil {
		return Result{}, err
	}

	currentDropIn, dropInExists, err := readOptional(m.DropInPath)
	if err != nil {
		return Result{}, err
	}

	if !bytes.Equal(currentConfig, managedConfig) || !dropInExists || !bytes.Equal(currentDropIn, managedDropIn) {
		return Result{Preserved: true}, errors.New("containerd config: managed files changed concurrently")
	}

	originalConfig, err := os.ReadFile(m.statePath("original-config.toml"))
	if err != nil {
		return Result{}, fmt.Errorf("containerd config: read original config: %w", err)
	}

	if err := atomicWrite(m.ConfigPath, originalConfig, 0o644); err != nil {
		return Result{}, err
	}

	if originalDropIn, exists, err := readOptional(m.statePath("original-drop-in.toml")); err != nil {
		return Result{}, err
	} else if exists {
		if err := atomicWrite(m.DropInPath, originalDropIn, 0o644); err != nil {
			return Result{}, err
		}
	} else if _, absentErr := os.Stat(m.statePath("original-drop-in-absent")); absentErr == nil {
		if err := os.Remove(m.DropInPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return Result{}, err
		}
	} else {
		return Result{}, errors.New("containerd config: original drop-in state is missing")
	}

	for _, name := range []string{
		"original-config.toml",
		"original-drop-in.toml",
		"original-drop-in-absent",
		"managed-config.toml",
		"managed-drop-in.toml",
		"managed",
	} {
		if err := os.Remove(m.statePath(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return Result{}, err
		}
	}

	return Result{Changed: true}, nil
}

func (m Manager) validate() error {
	if m.ConfigPath == "" || m.DropInPath == "" || m.StateDir == "" || !filepath.IsAbs(m.Address) || m.SnapshotterName == "" {
		return errors.New("containerd config: config, drop-in, state, snapshotter name, and absolute socket address are required")
	}

	return nil
}

func (m Manager) statePath(name string) string { return filepath.Join(m.StateDir, name) }

func ensureImport(data []byte) ([]byte, error) {
	var document map[string]any
	if err := toml.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("containerd config: parse main config: %w", err)
	}

	imports, err := stringSlice(document["imports"])
	if err != nil {
		return nil, err
	}

	for _, current := range imports {
		if current == importPattern {
			return data, nil
		}
	}

	document["imports"] = append(imports, importPattern)

	updated, err := toml.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("containerd config: encode main config: %w", err)
	}

	return updated, nil
}

func stringSlice(value any) ([]string, error) {
	if value == nil {
		return nil, nil
	}

	switch typed := value.(type) {
	case []string:
		return append([]string(nil), typed...), nil
	case []any:
		result := make([]string, 0, len(typed))
		for _, item := range typed {
			text, ok := item.(string)
			if !ok {
				return nil, errors.New("containerd config: imports contains a non-string value")
			}

			result = append(result, text)
		}

		return result, nil
	default:
		return nil, errors.New("containerd config: imports is not an array")
	}
}

func marshalDropIn(config []byte, address, snapshotterName string) ([]byte, error) {
	var source struct {
		Version int            `toml:"version"`
		Plugins map[string]any `toml:"plugins"`
	}
	if err := toml.Unmarshal(config, &source); err != nil {
		return nil, fmt.Errorf("containerd config: parse version: %w", err)
	}

	plugins := make(map[string]any, 1)

	_, hasCRIImagesPlugin := source.Plugins["io.containerd.cri.v1.images"]
	if hasCRIImagesPlugin || source.Version >= 3 {
		plugins["io.containerd.cri.v1.images"] = map[string]any{
			"snapshotter": snapshotterName,
		}
	} else {
		plugins["io.containerd.grpc.v1.cri"] = map[string]any{
			"containerd": map[string]any{
				"snapshotter": snapshotterName,
			},
		}
	}

	document := map[string]any{
		"version": source.Version,
		"proxy_plugins": map[string]any{
			snapshotterName: map[string]any{
				"type":    "snapshot",
				"address": address,
			},
		},
		"plugins": plugins,
	}

	data, err := toml.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("containerd config: encode drop-in: %w", err)
	}

	return data, nil
}

func readOptional(path string) ([]byte, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}

	if err != nil {
		return nil, false, fmt.Errorf("containerd config: read %s: %w", path, err)
	}

	return data, true, nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	temporary, err := os.CreateTemp(filepath.Dir(path), ".gantry-config-*")
	if err != nil {
		return err
	}

	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }() //nolint:errcheck // rename removes the path on success

	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close() //nolint:errcheck // chmod error is the actionable failure
		return err
	}

	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close() //nolint:errcheck // write error is the actionable failure
		return err
	}

	if err := temporary.Sync(); err != nil {
		_ = temporary.Close() //nolint:errcheck // sync error is the actionable failure
		return err
	}

	if err := temporary.Close(); err != nil {
		return err
	}

	return os.Rename(temporaryPath, path)
}
