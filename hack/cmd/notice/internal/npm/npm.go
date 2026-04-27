// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package npm implements a notice.Collector for direct dependencies declared
// in frontend/package.json. devDependencies are excluded.
package npm

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/Azure/unbounded/hack/cmd/notice/internal/notice"
)

// FileSystem is the small slice of os we depend on. Tests inject a fake to
// avoid materializing real node_modules trees on disk where possible
// (though most fixtures still use t.TempDir() since pkg directories are
// looked up by os.ReadFile).
type FileSystem interface {
	ReadFile(path string) ([]byte, error)
	Stat(path string) (os.FileInfo, error)
}

// Collector enumerates the `dependencies` listed in frontend/package.json
// and builds a NOTICE entry for each by reading the matching directory
// inside frontend/node_modules. devDependencies are ignored.
type Collector struct {
	fs FileSystem
}

// Option configures a Collector.
type Option func(*Collector)

// WithFS injects a FileSystem. Defaults to the host filesystem.
func WithFS(f FileSystem) Option { return func(c *Collector) { c.fs = f } }

// New constructs a Collector with production defaults.
func New(opts ...Option) *Collector {
	c := &Collector{fs: osFS{}}
	for _, o := range opts {
		o(c)
	}

	return c
}

// Name implements notice.Collector.
func (c *Collector) Name() string { return "npm" }

// Precheck implements notice.Collector. The collector requires
// frontend/node_modules to be populated; we do not auto-install.
func (c *Collector) Precheck(root string) error {
	frontendDir := filepath.Join(root, "frontend")

	if _, err := c.fs.Stat(filepath.Join(frontendDir, "package.json")); err != nil {
		// No frontend at all is a soft success: ecosystems that aren't
		// present in the project simply contribute zero entries. Surface
		// "no node_modules" only when there IS a frontend declared.
		if os.IsNotExist(err) {
			return nil
		}

		return fmt.Errorf("stat package.json: %w", err)
	}

	if _, err := c.fs.Stat(filepath.Join(frontendDir, "node_modules")); err != nil {
		return fmt.Errorf("frontend/node_modules not found; run 'npm ci' in frontend/ first (%w)", err)
	}

	return nil
}

// Collect implements notice.Collector.
func (c *Collector) Collect(root string) ([]notice.Entry, error) {
	frontendDir := filepath.Join(root, "frontend")
	pkgPath := filepath.Join(frontendDir, "package.json")

	if _, err := c.fs.Stat(pkgPath); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("stat %s: %w", pkgPath, err)
	}

	data, err := c.fs.ReadFile(pkgPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", pkgPath, err)
	}

	var pkg packageJSON
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", pkgPath, err)
	}

	if len(pkg.Deps) == 0 {
		return nil, nil
	}

	nodeModules := filepath.Join(frontendDir, "node_modules")

	names := make([]string, 0, len(pkg.Deps))
	for name := range pkg.Deps {
		names = append(names, name)
	}

	sort.Strings(names)

	out := make([]notice.Entry, 0, len(names))

	for _, name := range names {
		e, err := c.buildEntry(nodeModules, name)
		if err != nil {
			return nil, fmt.Errorf("npm package %s: %w", name, err)
		}

		out = append(out, e)
	}

	return out, nil
}

func (c *Collector) buildEntry(nodeModules, name string) (notice.Entry, error) {
	pkgDir := filepath.Join(nodeModules, name) // handles scoped @x/y naturally

	pkgJSONPath := filepath.Join(pkgDir, "package.json")

	data, err := c.fs.ReadFile(pkgJSONPath)
	if err != nil {
		return notice.Entry{}, fmt.Errorf("reading %s: %w", pkgJSONPath, err)
	}

	var pkg packageJSON
	if err := json.Unmarshal(data, &pkg); err != nil {
		return notice.Entry{}, fmt.Errorf("parsing %s: %w", pkgJSONPath, err)
	}

	repoBase, ref := repoBase(pkg, name)
	declared := licenseString(pkg)

	return notice.AssembleEntry(name, c.Name(), pkgDir, repoBase, ref, declared)
}

// osFS is the production FileSystem.
type osFS struct{}

func (osFS) ReadFile(path string) ([]byte, error)  { return os.ReadFile(path) }
func (osFS) Stat(path string) (os.FileInfo, error) { return os.Stat(path) }
