// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signalmanifest_test

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestUpstreamManifestsContainOnlyControllerSurface(t *testing.T) {
	t.Parallel()

	templates, err := filepath.Glob("*.yaml.tmpl")
	if err != nil {
		t.Fatal(err)
	}

	sort.Strings(templates)

	want := []string{
		"00-namespace.yaml.tmpl",
		"01-serviceaccounts.yaml.tmpl",
		"02-rbac.yaml.tmpl",
		"06-deployment.yaml.tmpl",
	}
	if !reflect.DeepEqual(templates, want) {
		t.Fatalf("manifest templates = %v, want %v", templates, want)
	}

	for _, path := range []string{"01-serviceaccounts.yaml.tmpl", "02-rbac.yaml.tmpl"} {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}

		manifest := string(content)
		if strings.Contains(manifest, "project-signal-runner") ||
			strings.Contains(manifest, "kubeflow.org") {
			t.Fatalf("%s contains bundled runner surface", path)
		}
	}
}

func TestUpstreamDoesNotPublishCertificationPackage(t *testing.T) {
	t.Parallel()

	for _, path := range []string{
		"../../project-signal-certification",
		"../../project-signal-certification-runner",
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s must not be published upstream: %v", path, err)
		}
	}
}
