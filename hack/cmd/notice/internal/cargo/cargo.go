// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package cargo implements a notice.Collector for direct non-development
// dependencies of cmd/racer-dataplane and its local path dependencies.
package cargo

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/Azure/unbounded/hack/cmd/notice/internal/license"
	"github.com/Azure/unbounded/hack/cmd/notice/internal/notice"
)

const (
	cratePath = "cmd/racer-dataplane"
	crateName = "racer-dataplane"
)

// Collector reads Cargo.toml and Cargo.lock locally and obtains license text
// from Cargo's populated registry source cache.
type Collector struct {
	cargoHome string
}

type dependency struct {
	packageName string
	path        string
}

// New constructs a Collector. An empty cargoHome uses CARGO_HOME or Cargo's
// standard $HOME/.cargo location.
func New(cargoHome ...string) *Collector {
	c := &Collector{}
	if len(cargoHome) != 0 {
		c.cargoHome = cargoHome[0]
	}

	return c
}

// Name implements notice.Collector.
func (c *Collector) Name() string { return "cargo" }

// Precheck implements notice.Collector.
func (c *Collector) Precheck(root string) error {
	present, err := cargoFilesPresent(root)
	if err != nil || !present {
		return err
	}

	lockPath := filepath.Join(root, cratePath, "Cargo.lock")

	lock, err := os.ReadFile(lockPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", lockPath, err)
	}

	versions, err := localRegistryVersions(filepath.Join(root, cratePath), string(lock))
	if err != nil {
		return err
	}

	if len(versions) == 0 {
		return nil
	}

	home, err := c.home()
	if err != nil {
		return err
	}

	if _, err := os.Stat(filepath.Join(home, "registry", "src")); err != nil {
		return fmt.Errorf("cargo registry source cache not found; run 'cargo fetch --manifest-path %s/Cargo.toml --locked' first (%w)", cratePath, err)
	}

	return nil
}

// Collect implements notice.Collector.
func (c *Collector) Collect(root string) ([]notice.Entry, error) {
	present, err := cargoFilesPresent(root)
	if err != nil || !present {
		return nil, err
	}

	lockPath := filepath.Join(root, cratePath, "Cargo.lock")

	lock, err := os.ReadFile(lockPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", lockPath, err)
	}

	versions, err := localRegistryVersions(filepath.Join(root, cratePath), string(lock))
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", lockPath, err)
	}

	entries := make([]notice.Entry, 0, len(versions))
	for name, version := range versions {
		entry, err := c.buildEntry(name, version)
		if err != nil {
			return nil, fmt.Errorf("crate %s@%s: %w", name, version, err)
		}

		entries = append(entries, entry)
	}

	return entries, nil
}

// localRegistryVersions follows local crates, but not registry transitives.
func localRegistryVersions(root, lock string) (map[string]string, error) {
	versions := map[string]string{}
	visited := map[string]bool{}

	var visit func(string, string) error

	visit = func(dir, name string) error {
		manifestPath := filepath.Join(dir, "Cargo.toml")

		canonical, err := filepath.EvalSymlinks(manifestPath)
		if err != nil {
			return fmt.Errorf("reading %s: %w", manifestPath, err)
		}

		if visited[canonical] {
			return nil
		}

		visited[canonical] = true

		manifest, err := os.ReadFile(manifestPath)
		if err != nil {
			return fmt.Errorf("reading %s: %w", manifestPath, err)
		}

		direct, err := directDependencies(string(manifest))
		if err != nil {
			return fmt.Errorf("parsing %s: %w", manifestPath, err)
		}

		locked, err := lockedDirectVersions(lock, direct, name)
		if err != nil {
			return fmt.Errorf("crate %s: %w", name, err)
		}

		for _, dep := range direct {
			if dep.path != "" {
				path := dep.path
				if !filepath.IsAbs(path) {
					path = filepath.Join(dir, path)
				}

				if err := visit(path, dep.packageName); err != nil {
					return err
				}

				continue
			}

			version := locked[dep.packageName]
			if previous := versions[dep.packageName]; previous != "" && previous != version {
				return fmt.Errorf("dependency %s has ambiguous locked versions", dep.packageName)
			}

			versions[dep.packageName] = version
		}

		return nil
	}

	if err := visit(root, crateName); err != nil {
		return nil, err
	}

	return versions, nil
}

// cargoFilesPresent permits an inactive scaffold, but rejects incomplete inputs.
func cargoFilesPresent(root string) (bool, error) {
	missing := make([]string, 0, 2)

	for _, name := range []string{"Cargo.toml", "Cargo.lock"} {
		path := filepath.Join(root, cratePath, name)

		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			missing = append(missing, name)
			continue
		}

		if err != nil {
			return false, fmt.Errorf("stat %s: %w", path, err)
		}

		if info.Mode()&os.ModeSymlink != 0 {
			info, err = os.Stat(path)
			if err != nil {
				return false, fmt.Errorf("stat %s: %w", path, err)
			}
		}

		if !info.Mode().IsRegular() {
			return false, fmt.Errorf("%s is not a regular file", path)
		}
	}

	if len(missing) == 2 {
		return false, nil
	}

	if len(missing) != 0 {
		return false, fmt.Errorf("missing %s", filepath.Join(cratePath, missing[0]))
	}

	return true, nil
}

func (c *Collector) buildEntry(name, version string) (notice.Entry, error) {
	home, err := c.home()
	if err != nil {
		return notice.Entry{}, err
	}

	matches, err := filepath.Glob(filepath.Join(home, "registry", "src", "*", name+"-"+version))
	if err != nil {
		return notice.Entry{}, fmt.Errorf("locating registry source: %w", err)
	}

	if len(matches) == 0 {
		return notice.Entry{}, fmt.Errorf("registry source directory not found")
	}

	if len(matches) > 1 {
		return notice.Entry{}, fmt.Errorf("multiple registry source directories found: %v", matches)
	}

	licensePaths, err := crateLicenseFiles(matches[0])
	if err != nil {
		declaredLicense := crateLicense(matches[0])
		if declaredLicense == "" {
			return notice.Entry{}, err
		}

		return notice.Entry{
			Dependency: name,
			Ecosystem:  c.Name(),
			Copyright:  []string{"See crate source"},
			License: declaredLicenses(
				declaredLicense,
				fmt.Sprintf("https://docs.rs/crate/%s/%s/source/Cargo.toml.orig", name, version),
			),
		}, nil
	}

	entry := notice.Entry{
		Dependency: name,
		Ecosystem:  c.Name(),
	}
	seenLicenses := map[string]bool{}
	seenCopyrights := map[string]bool{}

	for _, licensePath := range licensePaths {
		licenseText, readErr := os.ReadFile(licensePath)
		if readErr != nil {
			return notice.Entry{}, fmt.Errorf("reading %s: %w", licensePath, readErr)
		}

		licenseNames, classifyErr := license.Classify(licenseText)
		if classifyErr != nil {
			return notice.Entry{}, fmt.Errorf("classifying %s: %w", licensePath, classifyErr)
		}

		licenseURL := fmt.Sprintf("https://docs.rs/crate/%s/%s/source/%s", name, version, filepath.Base(licensePath))

		for _, licenseName := range licenseNames {
			if !seenLicenses[licenseName] {
				entry.License = append(entry.License, notice.License{Name: licenseName, Link: licenseURL})
				seenLicenses[licenseName] = true
			}
		}

		copyrights, copyrightErr := license.ExtractCopyrightFromDir(matches[0], licenseText)
		if copyrightErr != nil {
			return notice.Entry{}, fmt.Errorf("extracting copyright from %s: %w", licensePath, copyrightErr)
		}

		for _, copyright := range copyrights {
			if !seenCopyrights[copyright] {
				entry.Copyright = append(entry.Copyright, copyright)
				seenCopyrights[copyright] = true
			}
		}
	}

	if len(entry.Copyright) > 1 {
		entry.Copyright = slices.DeleteFunc(entry.Copyright, func(value string) bool {
			return value == "See LICENSE file"
		})
	}

	return entry, nil
}

func declaredLicenses(expression, link string) []notice.License {
	seen := map[string]bool{}

	var licenses []notice.License

	for _, part := range strings.FieldsFunc(expression, func(r rune) bool {
		return r == ' ' || r == '(' || r == ')'
	}) {
		if part == "OR" || part == "AND" || part == "WITH" {
			continue
		}

		name := license.SPDXFriendly(part)
		if !seen[name] {
			licenses = append(licenses, notice.License{Name: name, Link: link})
			seen[name] = true
		}
	}

	return licenses
}

func crateLicenseFiles(dir string) ([]string, error) {
	var paths []string

	for _, pattern := range []string{"LICENSE*", "LICENCE*", "COPYING*"} {
		matches, err := filepath.Glob(filepath.Join(dir, pattern))
		if err != nil {
			return nil, fmt.Errorf("locating license files: %w", err)
		}

		for _, match := range matches {
			info, statErr := os.Stat(match)
			if statErr == nil && !info.IsDir() {
				paths = append(paths, match)
			}
		}
	}

	if len(paths) == 0 {
		return nil, fmt.Errorf("no license file found in %s", dir)
	}

	sort.Strings(paths)

	return paths, nil
}

func (c *Collector) home() (string, error) {
	if c.cargoHome != "" {
		return c.cargoHome, nil
	}

	if home := os.Getenv("CARGO_HOME"); home != "" {
		return home, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home directory: %w", err)
	}

	return filepath.Join(home, ".cargo"), nil
}

func directDependencies(data string) (map[string]dependency, error) {
	var manifest cargoManifest
	if err := toml.Unmarshal([]byte(data), &manifest); err != nil {
		return nil, fmt.Errorf("invalid dependency line or manifest: %w", err)
	}

	return manifest.directDependencies()
}

type dependencyTables struct {
	Dependencies      map[string]any `toml:"dependencies"`
	BuildDependencies map[string]any `toml:"build-dependencies"`
}

type cargoManifest struct {
	dependencyTables
	Target map[string]dependencyTables `toml:"target"`
}

func (m cargoManifest) directDependencies() (map[string]dependency, error) {
	direct := map[string]dependency{}

	tables := []dependencyTables{m.dependencyTables}
	for _, target := range m.Target {
		tables = append(tables, target)
	}

	for _, table := range tables {
		for _, deps := range []map[string]any{table.Dependencies, table.BuildDependencies} {
			for alias, value := range deps {
				dep := dependency{packageName: alias}

				switch value := value.(type) {
				case string:
				case map[string]any:
					for _, field := range []string{"package", "path"} {
						if raw, exists := value[field]; exists {
							text, ok := raw.(string)
							if !ok || text == "" {
								return nil, fmt.Errorf("dependency %s: invalid %s", alias, field)
							}

							if field == "package" {
								dep.packageName = text
							} else {
								dep.path = text
							}
						}
					}
				default:
					return nil, fmt.Errorf("dependency %s: expected version or table", alias)
				}

				if previous, exists := direct[alias]; exists && previous != dep {
					return nil, fmt.Errorf("dependency %s has conflicting declarations", alias)
				}

				direct[alias] = dep
			}
		}
	}

	return direct, nil
}

func lockedDirectVersions(data string, direct map[string]dependency, owner string) (map[string]string, error) {
	type pkg struct {
		name, version string
		dependencies  []string
	}

	var (
		packages []pkg
		current  *pkg
	)

	inDependencies := false

	scanner := bufio.NewScanner(strings.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "[[package]]" {
			packages = append(packages, pkg{})
			current = &packages[len(packages)-1]
			inDependencies = false

			continue
		}

		if current == nil {
			continue
		}

		if inDependencies {
			if line == "]" {
				inDependencies = false
				continue
			}

			if dep := quotedValue(strings.TrimSuffix(line, ",")); dep != "" {
				current.dependencies = append(current.dependencies, dep)
			}

			continue
		}

		switch {
		case strings.HasPrefix(line, "name ="):
			current.name = quotedValue(strings.TrimSpace(strings.TrimPrefix(line, "name =")))
		case strings.HasPrefix(line, "version ="):
			current.version = quotedValue(strings.TrimSpace(strings.TrimPrefix(line, "version =")))
		case line == "dependencies = [":
			inDependencies = true
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	var root *pkg

	for i := range packages {
		if packages[i].name == owner {
			root = &packages[i]
			break
		}
	}

	if root == nil {
		return nil, fmt.Errorf("%s package not found", owner)
	}

	versions := map[string]string{}

	for _, dependency := range root.dependencies {
		name, version := lockDependency(dependency)
		if !containsPackage(direct, name) {
			continue
		}

		if version == "" {
			for _, candidate := range packages {
				if candidate.name == name {
					if version != "" {
						return nil, fmt.Errorf("dependency %s has ambiguous locked versions", name)
					}

					version = candidate.version
				}
			}
		}

		if version == "" {
			return nil, fmt.Errorf("dependency %s has no locked version", name)
		}

		versions[name] = version
	}

	for alias, dep := range direct {
		if versions[dep.packageName] == "" {
			return nil, fmt.Errorf("direct dependency %s not found in root lock entry", alias)
		}
	}

	return versions, nil
}

func containsPackage(direct map[string]dependency, name string) bool {
	for _, dep := range direct {
		if dep.packageName == name {
			return true
		}
	}

	return false
}

func lockDependency(value string) (string, string) {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return "", ""
	}

	if len(fields) > 1 && fields[1][0] >= '0' && fields[1][0] <= '9' {
		return fields[0], fields[1]
	}

	return fields[0], ""
}

func quotedValue(value string) string {
	if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
		return ""
	}

	return value[1 : len(value)-1]
}

func crateLicense(dir string) string {
	for _, name := range []string{"Cargo.toml.orig", "Cargo.toml"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}

		section := ""

		scanner := bufio.NewScanner(strings.NewReader(string(data)))
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if strings.HasPrefix(line, "[") {
				section = strings.Trim(line, "[]")
				continue
			}

			if section == "package" && strings.HasPrefix(line, "license =") {
				return quotedValue(strings.TrimSpace(strings.TrimPrefix(line, "license =")))
			}
		}
	}

	return ""
}
