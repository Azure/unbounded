// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"os"
	"slices"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

func TestRedfishPasswordSelectorKeyOrDefault(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  string
		want string
	}{
		{name: "empty key uses default", key: "", want: "password"},
		{name: "explicit key", key: "bmc-01", want: "bmc-01"},
		{name: "explicit ssh-privatekey is kept", key: "ssh-privatekey", want: "ssh-privatekey"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sel := RedfishPasswordSelector{Name: "bmc", Namespace: "unbounded-system", Key: tt.key}
			if got := sel.KeyOrDefault(); got != tt.want {
				t.Fatalf("KeyOrDefault() = %q, want %q", got, tt.want)
			}
		})
	}

	if DefaultRedfishPasswordKey != "password" {
		t.Fatalf("DefaultRedfishPasswordKey = %q, want %q", DefaultRedfishPasswordKey, "password")
	}
}

// TestSecretKeyDefaultsInCRDs checks the key default of every passwordRef and
// privateKeyRef in the generated CRDs. Redfish passwords default to
// "password"; SSH keys keep "ssh-privatekey".
func TestSecretKeyDefaultsInCRDs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		file string
		want map[string]string
	}{
		{
			file: "unbounded-cloud.io_machines.yaml",
			want: map[string]string{
				"spec.pxe.redfish.passwordRef":          "password",
				"spec.host.netboot.redfish.passwordRef": "password",
				"spec.ssh.privateKeyRef":                "ssh-privatekey",
				"spec.ssh.bastion.privateKeyRef":        "ssh-privatekey",
			},
		},
		{
			file: "unbounded-cloud.io_machineoperations.yaml",
			want: map[string]string{
				"status.targets.bootMedia.netboot.redfish.passwordRef": "password",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			t.Parallel()

			data, err := os.ReadFile("../../../deploy/machina/crd/" + tt.file)
			if err != nil {
				t.Fatalf("read CRD: %v", err)
			}

			var crd apiextensionsv1.CustomResourceDefinition
			if err := yaml.Unmarshal(data, &crd); err != nil {
				t.Fatalf("parse CRD: %v", err)
			}

			got := map[string]string{}

			for _, version := range crd.Spec.Versions {
				collectSecretKeyDefaults(t, "", *version.Schema.OpenAPIV3Schema, got)
			}

			for path, want := range tt.want {
				if got[path] != want {
					t.Errorf("%s key default = %q, want %q", path, got[path], want)
				}
			}

			// Every passwordRef must default to "password", including any
			// added later that the table above does not list.
			paths := make([]string, 0, len(got))
			for path := range got {
				paths = append(paths, path)
			}

			slices.Sort(paths)

			for _, path := range paths {
				if strings.HasSuffix(path, ".passwordRef") && got[path] != DefaultRedfishPasswordKey {
					t.Errorf("%s key default = %q, want %q", path, got[path], DefaultRedfishPasswordKey)
				}
			}
		})
	}
}

// collectSecretKeyDefaults records the default of the key property for every
// passwordRef and privateKeyRef schema under schema, keyed by its dotted path.
func collectSecretKeyDefaults(t *testing.T, path string, schema apiextensionsv1.JSONSchemaProps, out map[string]string) {
	t.Helper()

	for name, prop := range schema.Properties {
		child := name
		if path != "" {
			child = path + "." + name
		}

		if name == "passwordRef" || name == "privateKeyRef" {
			key, ok := prop.Properties["key"]
			if !ok {
				t.Fatalf("%s has no key property", child)
			}

			def := ""
			if key.Default != nil {
				if err := yaml.Unmarshal(key.Default.Raw, &def); err != nil {
					t.Fatalf("parse %s.key default: %v", child, err)
				}
			}

			out[child] = def
		}

		collectSecretKeyDefaults(t, child, prop, out)
	}

	if schema.Items != nil && schema.Items.Schema != nil {
		collectSecretKeyDefaults(t, path, *schema.Items.Schema, out)
	}
}
