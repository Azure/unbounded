// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package racer embeds the controller-only Racer templates applied by the
// operator. Manifests are rendered in memory with template defaults.
package racer

import (
	"bytes"
	"embed"
	"io/fs"
	"strings"
	"text/template"

	"github.com/Masterminds/sprig/v3"

	"github.com/Azure/unbounded/internal/version"
)

// Dataplane workloads and their bootstrap inputs are not operator-managed, so
// their templates are deliberately excluded.
//
//go:embed config.yaml.tmpl controller.yaml.tmpl controller-pdb.yaml.tmpl node-restriction.yaml.tmpl rbac.yaml.tmpl
var sources embed.FS

// Manifests exposes the embedded templates as rendered flat *.yaml names. It
// contains no dataplane workloads or generated CRDs. The controller image
// defaults to the binary version; the operator replaces it with its configured
// registry and tag before applying resources.
var Manifests fs.FS = manifestFS{}

type manifestFS struct{}

func (manifestFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) || strings.HasSuffix(name, ".tmpl") {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}

	if strings.Contains(name, "/") || !strings.HasSuffix(name, ".yaml") {
		if name == "." {
			file, err := sources.Open(name)
			if err != nil {
				return nil, err
			}

			directory, ok := file.(fs.ReadDirFile)
			if !ok {
				return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
			}

			return manifestDir{ReadDirFile: directory}, nil
		}

		return sources.Open(name)
	}

	data, err := sources.ReadFile(name + ".tmpl")
	if err != nil {
		return nil, err
	}

	tmpl, err := template.New(name).Funcs(sprig.TxtFuncMap()).Option("missingkey=zero").Parse(string(data))
	if err != nil {
		return nil, err
	}

	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, map[string]string{
		"ControllerImage": "ghcr.io/azure/racer-controller:" + version.Version,
	}); err != nil {
		return nil, err
	}

	info, err := fs.Stat(sources, name+".tmpl")
	if err != nil {
		return nil, err
	}

	return &manifestFile{Reader: bytes.NewReader(rendered.Bytes()), info: manifestInfo{FileInfo: info, size: int64(rendered.Len())}}, nil
}

func (manifestFS) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := sources.ReadDir(name)
	if err != nil {
		return nil, err
	}

	for i, entry := range entries {
		entries[i] = manifestEntry{DirEntry: entry}
	}

	return entries, nil
}

type manifestEntry struct{ fs.DirEntry }

func (e manifestEntry) Name() string { return strings.TrimSuffix(e.DirEntry.Name(), ".tmpl") }
func (e manifestEntry) Info() (fs.FileInfo, error) {
	if strings.HasSuffix(e.DirEntry.Name(), ".tmpl") {
		return fs.Stat(Manifests, e.Name())
	}

	return e.DirEntry.Info()
}

type manifestDir struct{ fs.ReadDirFile }

func (d manifestDir) ReadDir(n int) ([]fs.DirEntry, error) {
	entries, err := d.ReadDirFile.ReadDir(n)
	for i, entry := range entries {
		entries[i] = manifestEntry{DirEntry: entry}
	}

	return entries, err
}

type manifestInfo struct {
	fs.FileInfo
	size int64
}

func (i manifestInfo) Name() string { return strings.TrimSuffix(i.FileInfo.Name(), ".tmpl") }
func (i manifestInfo) Size() int64  { return i.size }

type manifestFile struct {
	*bytes.Reader
	info manifestInfo
}

func (f *manifestFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (*manifestFile) Close() error                 { return nil }
