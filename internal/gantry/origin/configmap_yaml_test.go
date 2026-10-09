// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package origin_test

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/origin"
)

// TestDefaultConfigMap_StartsCleanWithoutSecret verifies that the chart's
// default configuration constructs an origin client without shared credentials.
func TestDefaultConfigMap_StartsCleanWithoutSecret(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}

	helm := filepath.Join(repoRoot, "bin", "helm")
	if _, err := exec.LookPath(helm); err != nil {
		helm, err = exec.LookPath("helm")
		if err != nil {
			t.Fatal("helm not found; run make install-helm")
		}
	}

	chartPath := filepath.Join(repoRoot, "deploy", "gantry", "chart")
	cmd := exec.Command(helm, "template", "gantry", chartPath, "--show-only", "templates/configmap.yaml")

	rendered, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("render %s: %v\n%s", chartPath, err, rendered)
	}

	// deploy/gantry/configmap.yaml is a Kubernetes ConfigMap whose
	// data.config.yaml field carries the actual agent config. We
	// extract that inline YAML by string-trimming around the
	// well-known marker block - kubectl-style apply doesn't require
	// pulling in a kubernetes client just to read a single inlined
	// document. The marker is deliberately chosen to be the literal
	// first line of the inline config; if the operator reformats
	// the ConfigMap heavily this test fails loud (good - that means
	// the test needs reanchoring before shipping).
	cfgYAML := extractInlineConfig(t, string(rendered))

	cfg := config.NewDefault()
	if err := cfg.LoadYAML(strings.NewReader(cfgYAML)); err != nil {
		t.Fatalf("LoadYAML on default ConfigMap: %v", err)
	}

	// Shared identity file configuration must not return through chart rendering.
	if strings.Contains(cfgYAML, "credentials_path") {
		t.Error("removed credentials-file configuration rendered")
	}

	// Construction does not depend on mounted registry secrets.
	c, err := origin.New(cfg)
	if err != nil {
		t.Fatalf("origin.New on default ConfigMap: %v (the shipped default ConfigMap must start without any Secret being applied; see deploy/gantry/configmap.yaml and deploy/gantry/README.md 'Apply order')", err)
	}

	if c == nil {
		t.Fatal("origin.New returned nil client on default ConfigMap")
	}
}

// extractInlineConfig pulls the value of `data.config.yaml` out of
// the deploy ConfigMap document. The ConfigMap uses YAML block
// scalar style:
//
//	data:
//	 config.yaml: |
//	 <agent config goes here, indented 4 spaces>
//
// We anchor on the `config.yaml: |` line and read until the file
// ends or the indentation drops back to top level. Keeping this
// extractor inline (rather than pulling in k8s.io/api types) keeps
// the test in internal/origin's dependency closure.
func extractInlineConfig(t *testing.T, raw string) string {
	t.Helper()

	const marker = "config.yaml: |"

	idx := strings.Index(raw, marker)
	if idx < 0 {
		t.Fatalf("default ConfigMap does not contain %q marker; the test needs reanchoring against the new ConfigMap layout", marker)
	}

	body := raw[idx+len(marker):]
	// Trim leading newline that follows `|`.
	body = strings.TrimLeft(body, "\n")

	// The block-scalar body is indented 4 spaces (2 for `data:`'s
	// child + 2 more for `config.yaml:`'s value). Strip 4 spaces
	// per line; preserve blank lines verbatim.
	const indent = "    "

	var out strings.Builder

	for _, line := range strings.Split(body, "\n") {
		switch {
		case line == "":
			out.WriteString("\n")
		case strings.HasPrefix(line, indent):
			out.WriteString(strings.TrimPrefix(line, indent))
			out.WriteString("\n")
		default:
			// Indentation dropped - end of block scalar.
			return out.String()
		}
	}

	return out.String()
}
