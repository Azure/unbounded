// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cargo

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

const performancePath = "cmd/racer-loadgen/performance"

func allVersions(root string) (map[string]string, error) {
	versions := map[string]string{}

	present, err := cargoFilesPresent(root)
	if err != nil {
		return nil, err
	}

	if present {
		versions, err = workspaceVersions(root)
		if err != nil {
			return nil, err
		}
	}

	present, err = crateFilesPresent(root, performancePath)
	if err != nil || !present {
		return versions, err
	}

	manifest, err := os.ReadFile(filepath.Join(root, performancePath, "Cargo.toml"))
	if err != nil {
		return nil, err
	}

	name, err := manifestPackageName(string(manifest))
	if err != nil {
		return nil, err
	}

	direct, err := directDependencies(string(manifest))
	if err != nil {
		return nil, err
	}

	workspaceData, err := os.ReadFile(filepath.Join(root, cratePath, "Cargo.toml"))
	if err != nil {
		return nil, err
	}

	var workspace struct{ Workspace struct{ Members []string } }
	if err := toml.Unmarshal(workspaceData, &workspace); err != nil {
		return nil, err
	}

	declared := map[string]bool{".": true}
	for _, member := range workspace.Workspace.Members {
		declared[filepath.Clean(member)] = true
	}

	// This standalone tool can use first-party packages from the validated
	// dataplane workspace, but must not hide an unrelated local dependency.
	for _, dep := range direct {
		if dep.localPath == "" {
			continue
		}

		path := filepath.Join(root, performancePath, dep.localPath, "Cargo.toml")

		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, err
		}

		base, err := filepath.EvalSymlinks(filepath.Join(root, cratePath))
		if err != nil {
			return nil, err
		}

		relative, err := filepath.Rel(base, resolved)
		if err != nil || !filepath.IsLocal(relative) {
			return nil, fmt.Errorf("performance dependency %q escapes dataplane workspace", dep.localPath)
		}

		if !declared[filepath.Dir(relative)] {
			return nil, fmt.Errorf("performance dependency %q must be a declared workspace member", dep.localPath)
		}
	}

	lock, err := os.ReadFile(filepath.Join(root, performancePath, "Cargo.lock"))
	if err != nil {
		return nil, err
	}

	locked, err := lockedPackageVersions(string(lock), name, direct)
	if err != nil {
		return nil, err
	}

	for name, version := range locked {
		if previous := versions[name]; previous != "" && previous != version {
			return nil, fmt.Errorf("crate %s has conflicting direct versions %s and %s", name, previous, version)
		}

		versions[name] = version
	}

	return versions, nil
}
