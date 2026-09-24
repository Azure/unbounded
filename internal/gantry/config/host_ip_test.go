// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadNodeLocalUpstream(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")

	data := []byte("upstream_registries:\n  - name: fixture.test\n    endpoint: http://${GANTRY_HOST_IP}:18081\n  - name: other.test\n    endpoint: https://other.test\n    credentials_path: /etc/${GANTRY_HOST_IP}/creds\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, host := range []string{"10.1.2.3", "2001:db8::1", "", "node.example", "10.1.2.3/path"} {
		t.Run(host, func(t *testing.T) {
			c, _, err := Load(nil, func(key string) string {
				if key == "GANTRY_HOST_IP" {
					return host
				}

				return ""
			}, path)
			want := ""

			switch host {
			case "10.1.2.3":
				want = "http://10.1.2.3:18081"
			case "2001:db8::1":
				want = "http://[2001:db8::1]:18081"
			}

			if want == "" {
				if err == nil {
					t.Fatal("missing or invalid host IP accepted")
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			if c.UpstreamRegistries[0].Name != "fixture.test" || c.UpstreamRegistries[0].Endpoint != want || c.UpstreamRegistries[1].Endpoint != "https://other.test" || c.UpstreamRegistries[1].CredentialsPath != "/etc/${GANTRY_HOST_IP}/creds" {
				t.Fatalf("unexpected expansion: %+v", c.UpstreamRegistries)
			}
		})
	}
}
