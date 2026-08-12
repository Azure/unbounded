// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package gantrystandalone embeds and renders the manifests owned by gantryctl.
package gantrystandalone

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"text/template"
)

// Values are the runtime substitutions supported by standalone manifests.
type Values struct {
	Namespace string
	Image     string
}

// Manifest is one rendered Kubernetes manifest.
type Manifest struct {
	Name string
	Data []byte
}

//go:embed *.yaml.tmpl
var templates embed.FS

// Render renders every standalone manifest in stable filename order.
func Render(values Values) ([]Manifest, error) {
	if values.Namespace == "" {
		return nil, fmt.Errorf("standalone manifest namespace is required")
	}

	if values.Image == "" {
		return nil, fmt.Errorf("standalone Gantry image is required")
	}

	paths, err := fs.Glob(templates, "*.yaml.tmpl")
	if err != nil {
		return nil, fmt.Errorf("list standalone manifests: %w", err)
	}

	sort.Strings(paths)

	manifests := make([]Manifest, 0, len(paths))
	for _, path := range paths {
		source, err := templates.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read standalone manifest %s: %w", path, err)
		}

		parsed, err := template.New(path).Option("missingkey=error").Parse(string(source))
		if err != nil {
			return nil, fmt.Errorf("parse standalone manifest %s: %w", path, err)
		}

		var rendered bytes.Buffer
		if err := parsed.Execute(&rendered, values); err != nil {
			return nil, fmt.Errorf("render standalone manifest %s: %w", path, err)
		}

		manifests = append(manifests, Manifest{Name: path, Data: rendered.Bytes()})
	}

	return manifests, nil
}
