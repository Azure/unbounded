// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package cargo implements a notice.Collector for direct non-development
// dependencies of cmd/unbounded-storage.
package cargo

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/Azure/unbounded/hack/cmd/notice/internal/license"
	"github.com/Azure/unbounded/hack/cmd/notice/internal/notice"
)

const cratePath = "cmd/unbounded-storage"

// Collector reads Cargo.toml and Cargo.lock locally and obtains license text
// from Cargo's populated registry source cache.
type Collector struct {
	cargoHome string
}

type dependency struct {
	packageName string
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
	for _, name := range []string{"Cargo.toml", "Cargo.lock"} {
		if _, err := os.Stat(filepath.Join(root, cratePath, name)); err != nil {
			return fmt.Errorf("stat %s: %w", filepath.Join(cratePath, name), err)
		}
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
	manifestPath := filepath.Join(root, cratePath, "Cargo.toml")

	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", manifestPath, err)
	}

	direct, err := directDependencies(string(manifest))
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", manifestPath, err)
	}

	lockPath := filepath.Join(root, cratePath, "Cargo.lock")

	lock, err := os.ReadFile(lockPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", lockPath, err)
	}

	versions, err := lockedDirectVersions(string(lock), direct)
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
	direct := map[string]dependency{}
	section := ""

	scanner := bufio.NewScanner(strings.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(strings.SplitN(scanner.Text(), "#", 2)[0])
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			continue
		}

		if !dependencySection(section) || line == "" {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("invalid dependency line %q", line)
		}

		name := strings.Trim(strings.TrimSpace(key), `"'`)

		packageName := name
		if parsed := inlinePackageName(value); parsed != "" {
			packageName = parsed
		}

		direct[name] = dependency{packageName: packageName}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return direct, nil
}

func inlinePackageName(value string) string {
	for _, field := range strings.Split(strings.Trim(value, " {}"), ",") {
		key, fieldValue, found := strings.Cut(field, "=")
		if found && strings.TrimSpace(key) == "package" {
			return quotedValue(strings.TrimSpace(fieldValue))
		}
	}

	return ""
}

func dependencySection(section string) bool {
	return section == "dependencies" || section == "build-dependencies" ||
		(strings.HasPrefix(section, "target.") && (strings.HasSuffix(section, ".dependencies") || strings.HasSuffix(section, ".build-dependencies")))
}

func lockedDirectVersions(data string, direct map[string]dependency) (map[string]string, error) {
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
		if packages[i].name == "unbounded-storage" {
			root = &packages[i]
			break
		}
	}

	if root == nil {
		return nil, fmt.Errorf("unbounded-storage package not found")
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
