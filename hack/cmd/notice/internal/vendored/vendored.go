// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package vendored implements a notice.Collector for third-party assets that
// are vendored directly into the repository (for example minified web assets
// like Bootstrap and htmx embedded into a Go binary) rather than pulled through
// a package manager.
//
// Unlike the go.mod and npm collectors, there is no manifest a package manager
// maintains for these files, so attribution is declared explicitly in a
// checked-in manifest at hack/cmd/notice/vendored-assets.yaml. Keeping the data
// there (instead of hand-editing NOTICE) preserves the "NOTICE is generated and
// verifiable" workflow: `make notice` renders it and `make notice-check`
// enforces it.
package vendored

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Azure/unbounded/hack/cmd/notice/internal/notice"
)

// ManifestPath is the repo-root-relative location of the vendored-asset
// attribution manifest.
var ManifestPath = filepath.Join("hack", "cmd", "notice", "vendored-assets.yaml")

// manifest is the on-disk schema of vendored-assets.yaml.
type manifest struct {
	Assets []asset `yaml:"assets"`
}

// asset is one vendored third-party component's attribution.
type asset struct {
	Dependency string           `yaml:"dependency"`
	Copyright  []string         `yaml:"copyright"`
	License    []notice.License `yaml:"license"`
}

// Collector emits one NOTICE entry per asset declared in the manifest.
type Collector struct{}

// New constructs a Collector.
func New() *Collector { return &Collector{} }

// Name implements notice.Collector.
func (c *Collector) Name() string { return "vendored" }

// Precheck implements notice.Collector. A missing manifest is a soft success
// (zero entries), matching how the other collectors treat absent ecosystems.
func (c *Collector) Precheck(string) error { return nil }

// Collect implements notice.Collector.
func (c *Collector) Collect(root string) ([]notice.Entry, error) {
	path := filepath.Join(root, ManifestPath)

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("reading vendored manifest %s: %w", path, err)
	}

	var m manifest

	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)

	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("parsing vendored manifest %s: %w", path, err)
	}

	entries := make([]notice.Entry, 0, len(m.Assets))

	for i, a := range m.Assets {
		if err := validate(a, i); err != nil {
			return nil, fmt.Errorf("%s: %w", ManifestPath, err)
		}

		entries = append(entries, notice.Entry{
			Dependency: a.Dependency,
			Ecosystem:  c.Name(),
			Copyright:  a.Copyright,
			License:    a.License,
		})
	}

	return entries, nil
}

func validate(a asset, idx int) error {
	if strings.TrimSpace(a.Dependency) == "" {
		return fmt.Errorf("assets[%d]: dependency is required", idx)
	}

	if len(a.License) == 0 {
		return fmt.Errorf("asset %q: at least one license is required", a.Dependency)
	}

	for _, l := range a.License {
		if strings.TrimSpace(l.Name) == "" || strings.TrimSpace(l.Link) == "" {
			return fmt.Errorf("asset %q: each license needs a name and link", a.Dependency)
		}
	}

	return nil
}
