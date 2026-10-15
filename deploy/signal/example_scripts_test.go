// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signalmanifest

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestExampleScriptsAreConfigMapDataAndParseAsBash(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("03-scripts.yaml.tmpl")
	if err != nil {
		t.Fatal(err)
	}

	rendered := strings.ReplaceAll(
		string(raw),
		`{{ default "project-signal" .Namespace }}`,
		"project-signal",
	)

	var manifest struct {
		Kind string            `yaml:"kind"`
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal([]byte(rendered), &manifest); err != nil {
		t.Fatalf("parse script ConfigMap: %v", err)
	}

	if manifest.Kind != "ConfigMap" || len(manifest.Data) == 0 {
		t.Fatalf("expected a ConfigMap containing example scripts, got %#v", manifest)
	}

	for name, script := range manifest.Data {
		name, script := name, script
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cmd := exec.Command("bash", "-n")
			cmd.Stdin = strings.NewReader(script)

			var stderr bytes.Buffer

			cmd.Stderr = &stderr

			if err := cmd.Run(); err != nil {
				t.Fatalf("bash -n: %v: %s", err, stderr.String())
			}
		})
	}
}

func TestNoStandaloneCertificationScripts(t *testing.T) {
	t.Parallel()

	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if !info.IsDir() && strings.HasSuffix(info.Name(), ".sh") {
			t.Errorf("certification scripts must be ConfigMap data, found %s", path)
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
