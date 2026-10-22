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

func TestUpstreamManifestsContainControllerAndGenericAdapter(t *testing.T) {
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
		"03-adapter-serviceaccount-rbac.yaml.tmpl",
		"04-adapter-workflowtemplate.yaml.tmpl",
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

func TestAdapterManifestIsGenericAndParticipantAware(t *testing.T) {
	t.Parallel()

	content, err := os.ReadFile("04-adapter-workflowtemplate.yaml.tmpl")
	if err != nil {
		t.Fatal(err)
	}

	manifest := string(content)
	for _, required := range []string{
		"participants-json",
		"participant-count",
		"request-node-name",
		"backend-resources",
		"backend-api-version",
		"backend-kind",
		"queue",
		"workload-policy",
	} {
		if !strings.Contains(manifest, required) {
			t.Fatalf("adapter WorkflowTemplate does not contain %q", required)
		}
	}

	for _, forbidden := range []string{
		"project-signal-certification/",
		"REPLACE_WITH_",
		"internal-node",
	} {
		if strings.Contains(manifest, forbidden) {
			t.Fatalf("adapter WorkflowTemplate contains downstream-specific value %q", forbidden)
		}
	}
}

func TestCertificationRunCRDRequiresCanonicalDigests(t *testing.T) {
	t.Parallel()

	content, err := os.ReadFile("crd/signal.unbounded-cloud.io_certificationruns.yaml")
	if err != nil {
		t.Fatal(err)
	}

	const digestPattern = "pattern: ^sha256:[0-9a-f]{64}$"
	if count := strings.Count(string(content), digestPattern); count != 2 {
		t.Fatalf("canonical digest patterns = %d, want 2", count)
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
