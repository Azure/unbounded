// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cargo

import (
	"fmt"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

type lockedPackage struct {
	Name         string   `toml:"name"`
	Version      string   `toml:"version"`
	Source       string   `toml:"source"`
	Dependencies []string `toml:"dependencies"`
}

func lockPackages(data string) ([]lockedPackage, error) {
	var lock struct {
		Packages []lockedPackage `toml:"package"`
	}
	if err := toml.Unmarshal([]byte(data), &lock); err != nil {
		return nil, fmt.Errorf("invalid Cargo.lock: %w", err)
	}

	return lock.Packages, nil
}

// resolveLocked rejects ambiguous names and sources instead of choosing an
// arbitrary version or registry cache directory.
func resolveLocked(packages []lockedPackage, edge string) (lockedPackage, error) {
	name, version := lockDependency(edge)

	_, source, qualified := strings.Cut(edge, " (")
	if qualified {
		source = strings.TrimSuffix(source, ")")
	}

	var matches []lockedPackage

	for _, pkg := range packages {
		if pkg.Name == name && (version == "" || pkg.Version == version) {
			matches = append(matches, pkg)
		}
	}

	if len(matches) == 0 {
		return lockedPackage{}, fmt.Errorf("dependency %s has no locked version", name)
	}

	if len(matches) != 1 {
		return lockedPackage{}, fmt.Errorf("dependency %s has ambiguous locked versions or sources", name)
	}

	if qualified && source != matches[0].Source {
		return lockedPackage{}, fmt.Errorf("dependency %s has mismatched locked source", name)
	}

	return matches[0], nil
}

// Local packages are filtered by their manifests before seeding this closure.
// Registry lock edges contain normal/build dependencies, not registry dev deps.
// Never start from every lock package: that would include workspace dev deps.
func registryClosure(data string, seeds map[string]string) (map[string]string, error) {
	packages, err := lockPackages(data)
	if err != nil {
		return nil, err
	}

	versions := map[string]string{}

	var visit func(string) error

	visit = func(edge string) error {
		pkg, err := resolveLocked(packages, edge)
		if err != nil {
			return err
		}
		// Non-registry edges require local manifest filtering, which cannot be
		// inferred safely from the untyped lock edges alone.
		if !strings.HasPrefix(pkg.Source, "registry+") {
			return fmt.Errorf("dependency %s has unsupported non-registry source %q", pkg.Name, pkg.Source)
		}

		if previous := versions[pkg.Name]; previous != "" {
			if previous != pkg.Version {
				return fmt.Errorf("dependency %s has ambiguous locked versions", pkg.Name)
			}

			return nil
		}

		versions[pkg.Name] = pkg.Version
		for _, dep := range pkg.Dependencies {
			if err := visit(dep); err != nil {
				return err
			}
		}

		return nil
	}
	for name, version := range seeds {
		if err := visit(name + " " + version); err != nil {
			return nil, err
		}
	}

	return versions, nil
}
