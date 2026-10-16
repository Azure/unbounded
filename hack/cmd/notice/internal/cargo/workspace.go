// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cargo

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// sharedCratesPath is the repository directory holding first-party Rust
// library crates that the dataplane may consume as path dependencies.
const sharedCratesPath = "crates"

// Workspace members use the root lock. Only explicitly declared relative members
// are read; missing declared inputs fail closed. Path dependencies that leave the
// workspace are accepted only when they resolve to a package under the
// repository crates/ directory. Such packages are first-party: they get no
// NOTICE entry of their own, but their registry dependencies ship in the
// dataplane binary, so they are resolved against the dataplane Cargo.lock the
// same way as workspace members.
func workspaceVersions(root string) (map[string]string, error) {
	base := filepath.Join(root, cratePath)

	manifest, err := os.ReadFile(filepath.Join(base, "Cargo.toml"))
	if err != nil {
		return nil, err
	}

	var workspace struct {
		Workspace struct {
			Members []string
			Exclude []string
		}
	}
	if err := toml.Unmarshal(manifest, &workspace); err != nil {
		return nil, fmt.Errorf("parsing root Cargo.toml: %w", err)
	}

	if len(workspace.Workspace.Exclude) != 0 {
		return nil, fmt.Errorf("workspace exclude is not supported; list explicit members")
	}

	lock, err := os.ReadFile(filepath.Join(base, "Cargo.lock"))
	if err != nil {
		return nil, err
	}

	versions := map[string]string{}

	declared := map[string]bool{".": true}
	for _, member := range workspace.Workspace.Members {
		declared[filepath.Clean(member)] = true
	}

	for _, member := range workspace.Workspace.Members {
		if member == "" || !filepath.IsLocal(member) || strings.ContainsAny(member, "*?[\\") {
			return nil, fmt.Errorf("workspace member %q must be an explicit local directory", member)
		}
	}

	shared := filepath.Join(root, sharedCratesPath)
	seen := map[string]bool{}
	queue := append([]string{"."}, workspace.Workspace.Members...)

	for len(queue) > 0 {
		member := filepath.Clean(queue[0])
		queue = queue[1:]

		if seen[member] {
			continue
		}

		seen[member] = true
		external := !filepath.IsLocal(member)
		path := filepath.Join(base, member, "Cargo.toml")

		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, fmt.Errorf("resolving %s: %w", path, err)
		}

		resolvedBase, err := filepath.EvalSymlinks(base)
		if err != nil {
			return nil, err
		}

		if external {
			resolvedShared, err := filepath.EvalSymlinks(shared)
			if err != nil {
				return nil, fmt.Errorf("resolving %s: %w", shared, err)
			}

			relative, err := filepath.Rel(resolvedShared, resolved)
			if err != nil || !filepath.IsLocal(relative) {
				return nil, fmt.Errorf("path dependency %s escapes %s", path, sharedCratesPath)
			}
		} else {
			relative, err := filepath.Rel(resolvedBase, resolved)
			if err != nil || !filepath.IsLocal(relative) {
				return nil, fmt.Errorf("workspace member %s escapes workspace", path)
			}
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading workspace member %s: %w", path, err)
		}

		var manifest struct {
			Package struct{ Name string }
		}
		if err := toml.Unmarshal(data, &manifest); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}

		name := manifest.Package.Name
		if name == "" {
			if member != "." {
				return nil, fmt.Errorf("missing package name in %s", path)
			}
			// Retain the original root collector's dependency-only fixture contract.
			name = crateName
		}

		direct, err := directDependencies(string(data))
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}

		// Includes development path edges for membership validation only;
		// lockedPackageVersions excludes all local paths from registry notices.
		for _, dep := range direct {
			if dep.localPath == "" {
				continue
			}

			if filepath.IsAbs(dep.localPath) {
				return nil, fmt.Errorf("%s: local dependency %q must be relative", path, dep.localPath)
			}

			local := filepath.Clean(filepath.Join(member, dep.localPath))
			if declared[local] {
				continue
			}

			if filepath.IsLocal(local) {
				return nil, fmt.Errorf("%s: local dependency %q must be a declared workspace member", path, dep.localPath)
			}

			target := filepath.Join(base, local)
			if rel, err := filepath.Rel(shared, target); err != nil || rel == "." || !filepath.IsLocal(rel) {
				return nil, fmt.Errorf("%s: local dependency %q must be a declared workspace member or a package under %s", path, dep.localPath, sharedCratesPath)
			}

			queue = append(queue, local)
		}

		locked, err := lockedPackageVersions(string(lock), name, direct)
		if err != nil {
			return nil, fmt.Errorf("resolving %s in root Cargo.lock: %w", path, err)
		}

		for dependency, version := range locked {
			if previous := versions[dependency]; previous != "" && previous != version {
				return nil, fmt.Errorf("crate %s has conflicting direct versions %s and %s", dependency, previous, version)
			}

			versions[dependency] = version
		}
	}

	return versions, nil
}
