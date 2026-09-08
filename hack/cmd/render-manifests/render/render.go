// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package render implements the manifest template renderer used by
// the render-manifests CLI. Exposed as a package so tests in other
// packages (e.g. internal/orca/manifests) can render the orca
// templates programmatically without shelling out to `go run`.
package render

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/Masterminds/sprig/v3"
)

// Render walks templatesDir for *.yaml.tmpl files, executes each with
// Go's text/template (plus the sprig function library), and writes
// the rendered output under outputDir mirroring the source tree.
//
// Template data is supplied via the data map. Missing keys evaluate
// to empty strings (text/template's missingkey=zero), which lets
// templates rely on sprig's `default` function for fallbacks.
func Render(templatesDir, outputDir string, data map[string]string) error {
	return filepath.WalkDir(templatesDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		if !strings.HasSuffix(path, ".yaml.tmpl") {
			return nil
		}

		relPath, err := filepath.Rel(templatesDir, path)
		if err != nil {
			return err
		}

		outputRelPath := strings.TrimSuffix(relPath, ".tmpl")
		outputPath := filepath.Join(outputDir, outputRelPath)

		templateBytes, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read template %q: %w", path, err)
		}

		tmpl, err := template.New(relPath).Funcs(sprig.TxtFuncMap()).Option("missingkey=zero").Parse(string(templateBytes))
		if err != nil {
			return fmt.Errorf("parse template %q: %w", path, err)
		}

		if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
			return fmt.Errorf("create output dir for %q: %w", outputPath, err)
		}

		var rendered bytes.Buffer
		if err := tmpl.Execute(&rendered, data); err != nil {
			return fmt.Errorf("execute template %q: %w", path, err)
		}

		if err := os.WriteFile(outputPath, rendered.Bytes(), 0o644); err != nil {
			return fmt.Errorf("write rendered manifest %q: %w", outputPath, err)
		}

		return nil
	})
}
