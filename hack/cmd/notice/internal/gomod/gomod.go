// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package gomod implements a notice.Collector for direct dependencies
// declared in go.mod.
package gomod

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/mod/modfile"

	"github.com/Azure/unbounded/hack/cmd/notice/internal/notice"
)

// CmdRunner runs an external command and returns stdout. Tests inject a fake
// to keep `go mod download` out of the loop.
type CmdRunner interface {
	Run(name string, args ...string) ([]byte, error)
}

// FileSystem is the small slice of os we depend on. Tests inject a fake to
// keep the host filesystem out of the loop where it would otherwise consult
// the real Go module cache.
type FileSystem interface {
	ReadFile(path string) ([]byte, error)
}

// Collector enumerates direct (non-indirect) dependencies in go.mod and
// builds a NOTICE entry for each by consulting the module cache via
// `go mod download -json`.
type Collector struct {
	runner CmdRunner
	fs     FileSystem
}

// Option configures a Collector.
type Option func(*Collector)

// WithRunner injects a CmdRunner. Defaults to invoking the real `go` binary.
func WithRunner(r CmdRunner) Option { return func(c *Collector) { c.runner = r } }

// WithFS injects a FileSystem. Defaults to the host filesystem.
func WithFS(f FileSystem) Option { return func(c *Collector) { c.fs = f } }

// New constructs a Collector with production defaults.
func New(opts ...Option) *Collector {
	c := &Collector{
		runner: realRunner{},
		fs:     osFS{},
	}
	for _, o := range opts {
		o(c)
	}

	return c
}

// Name implements notice.Collector.
func (c *Collector) Name() string { return "go" }

// Precheck implements notice.Collector. The Go toolchain handles its own
// caching; we only verify that go.mod exists.
func (c *Collector) Precheck(root string) error {
	if _, err := c.fs.ReadFile(filepath.Join(root, "go.mod")); err != nil {
		return fmt.Errorf("reading go.mod: %w", err)
	}

	return nil
}

// Collect implements notice.Collector.
func (c *Collector) Collect(root string) ([]notice.Entry, error) {
	goModPath := filepath.Join(root, "go.mod")

	data, err := c.fs.ReadFile(goModPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", goModPath, err)
	}

	mf, err := modfile.Parse(goModPath, data, nil)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", goModPath, err)
	}

	type modVer struct{ Path, Version string }

	var direct []modVer

	seen := map[string]bool{}

	for _, r := range mf.Require {
		if r.Indirect {
			continue
		}

		if seen[r.Mod.Path] {
			continue
		}

		seen[r.Mod.Path] = true

		direct = append(direct, modVer{Path: r.Mod.Path, Version: r.Mod.Version})
	}

	out := make([]notice.Entry, 0, len(direct))

	for _, m := range direct {
		e, err := c.buildEntry(m.Path, m.Version)
		if err != nil {
			return nil, fmt.Errorf("%s@%s: %w", m.Path, m.Version, err)
		}

		out = append(out, e)
	}

	return out, nil
}

type goModDownload struct {
	Path    string
	Version string
	Dir     string
	Origin  *struct {
		VCS string
		URL string
		Ref string
	} `json:"Origin,omitempty"`
}

func (c *Collector) buildEntry(modPath, version string) (notice.Entry, error) {
	info, err := c.goModDownloadJSON(modPath, version)
	if err != nil {
		return notice.Entry{}, err
	}

	if info.Dir == "" {
		return notice.Entry{}, fmt.Errorf("module dir not found in cache")
	}

	repoBase, ref := goRepoBase(modPath, version, info)

	return notice.AssembleEntry(modPath, c.Name(), info.Dir, repoBase, ref, "")
}

func (c *Collector) goModDownloadJSON(modPath, version string) (*goModDownload, error) {
	out, err := c.runner.Run("go", "mod", "download", "-json", "-x", modPath+"@"+version)
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("go mod download: %s", strings.TrimSpace(string(ee.Stderr)))
		}

		return nil, fmt.Errorf("go mod download: %w", err)
	}

	var info goModDownload
	if err := json.Unmarshal(out, &info); err != nil {
		return nil, fmt.Errorf("parsing go mod download output: %w", err)
	}

	return &info, nil
}

// realRunner is the production CmdRunner; it shells out via os/exec.
type realRunner struct{}

func (realRunner) Run(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Stderr = nil

	return cmd.Output()
}

// osFS is the production FileSystem.
type osFS struct{}

func (osFS) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }
