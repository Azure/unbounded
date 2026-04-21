// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// render-manifests is a generic Go template renderer. It walks --templates-dir
// for *.yaml.tmpl files, executes each with Go's text/template (plus the sprig
// function library), and writes the rendered output under --output-dir
// mirroring the source tree structure.
//
// Template data is supplied via repeatable --set key=value flags. Missing keys
// evaluate to empty strings (text/template's missingkey=zero behaviour for map
// data), which lets templates rely on sprig's `default` function to supply
// documented fallbacks.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	"github.com/Masterminds/sprig/v3"
)

// setFlags implements flag.Value for repeatable --set key=value arguments.
type setFlags map[string]string

func (s setFlags) String() string {
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, k+"="+s[k])
	}

	return strings.Join(pairs, ",")
}

func (s setFlags) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return fmt.Errorf("--set value must be key=value, got %q", v)
	}

	s[k] = val

	return nil
}

func main() {
	var (
		templatesDir string
		outputDir    string
		data         = setFlags{}
	)

	flag.StringVar(&templatesDir, "templates-dir", "", "Directory containing *.yaml.tmpl manifest templates")
	flag.StringVar(&outputDir, "output-dir", "", "Directory where rendered manifests are written")
	flag.Var(data, "set", "Template variable as key=value (repeatable)")
	flag.Parse()

	if templatesDir == "" {
		exitWithError("--templates-dir is required")
	}

	if outputDir == "" {
		exitWithError("--output-dir is required")
	}

	if err := renderTemplates(templatesDir, outputDir, data); err != nil {
		exitWithError(err.Error())
	}
}

func renderTemplates(templatesDir, outputDir string, data setFlags) error {
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
		if err := tmpl.Execute(&rendered, map[string]string(data)); err != nil {
			return fmt.Errorf("execute template %q: %w", path, err)
		}

		if err := os.WriteFile(outputPath, rendered.Bytes(), 0o644); err != nil {
			return fmt.Errorf("write rendered manifest %q: %w", outputPath, err)
		}

		return nil
	})
}

func exitWithError(message string) {
	fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}
