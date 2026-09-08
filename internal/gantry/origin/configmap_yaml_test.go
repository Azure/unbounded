// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package origin_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"

	"github.com/Masterminds/sprig/v3"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/origin"
)

// TestDefaultConfigMap_StartsCleanWithoutSecret is the regression
// test for 's main finding:
//
//	The shipped deploy/gantry/configmap.yaml declared
//	`credentials_path: "/etc/gantry/registry/registry.example.com"`
//	on its sole upstream-registry entry, while the matching
//	registry-creds volume in deploy/gantry/daemonset.yaml is `optional: true`
//	and the harness does NOT apply deploy/gantry/examples/registry-secret.example.yaml.
//	Kubernetes therefore happily starts the pod with no Secret
//	mounted, but origin.New eagerly opens credentials_path at
//	startup, returns a hard error, and the agent crashloops before
//	/readyz can ever turn green. The e2e smoke test could never
//	reach rollout.
//
// This test guards the post-fix invariant: loading the shipped
// default ConfigMap into a Config and constructing an origin.Client
// from it succeeds WITHOUT any credentials file being present.
//
// If a future revision re-introduces a credentials_path on an
// uncommented entry, origin.New(cfg) will hit
// `read credentials %q: no such file or directory` and the test
// trips - exactly the failure mode the reviewer caught in
// production manifests.
//
// Why this lives in internal/origin: origin.New is the chokepoint
// that crashloops the pod. Putting the test next to it keeps the
// failure mode and its regression guard adjacent. It also runs in
// the default `go test ./...` suite (no `e2e` build tag) so every
// validation gate enforces it.
func TestDefaultConfigMap_StartsCleanWithoutSecret(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}

	yamlPath := filepath.Join(repoRoot, "deploy", "gantry", "configmap.yaml.tmpl")

	raw, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatalf("read %s: %v", yamlPath, err)
	}

	tmpl, err := template.New(filepath.Base(yamlPath)).
		Funcs(sprig.TxtFuncMap()).
		Option("missingkey=zero").
		Parse(string(raw))
	if err != nil {
		t.Fatalf("parse %s: %v", yamlPath, err)
	}

	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, map[string]any{}); err != nil {
		t.Fatalf("render %s: %v", yamlPath, err)
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
	cfgYAML := extractInlineConfig(t, rendered.String())

	cfg := config.NewDefault()
	if err := cfg.LoadYAML(strings.NewReader(cfgYAML)); err != nil {
		t.Fatalf("LoadYAML on default ConfigMap: %v", err)
	}

	// The actual regression check: every UpstreamRegistry whose
	// credentials_path is non-empty must reference a file that
	// either (a) exists or (b) is explicitly created by the apply
	// flow. The simplest defensible invariant - and the one the
	// a prior review demands - is that the SHIPPED default has
	// no required credentials_path, so the pod starts cleanly on
	// any cluster regardless of whether deploy/gantry/examples/registry-secret.example.yaml
	// has been applied.
	for i, ur := range cfg.UpstreamRegistries {
		if ur.CredentialsPath != "" {
			t.Errorf("deploy/gantry/configmap.yaml UpstreamRegistries[%d] (%q) has credentials_path=%q on an active entry; the shipped default must be credentials-free so the agent starts without deploy/gantry/examples/registry-secret.example.yaml being applied. Comment the credentials_path line out (operators uncomment when they bring real credentials).", i, ur.Name, ur.CredentialsPath)
		}
	}

	// And the load-bearing assertion: origin.New must succeed on
	// the default ConfigMap. This catches the original bug exactly
	// - even with no credentials_path set today, a future entry
	// that references an absent file would trip this.
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
