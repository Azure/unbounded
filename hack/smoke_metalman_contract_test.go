// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package hack

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMetalmanSmokeSuitesUseSplitRuntime(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"smoke-metalman.py", "smoke-metalman-http.py"} {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			contents, err := os.ReadFile(filepath.Join(".", name))
			if err != nil {
				t.Fatal(err)
			}

			text := string(contents)
			for _, stale := range []string{"serve-pxe", `"bootProtocol"`} {
				if strings.Contains(text, stale) {
					t.Errorf("%s still contains legacy %q wiring", name, stale)
				}
			}

			for _, required := range []string{"deploy_split_metalman", "bootstrap-netboot"} {
				if !strings.Contains(text, required) {
					t.Errorf("%s does not exercise %q", name, required)
				}
			}
		})
	}
}

func TestTraditionalMetalmanSmokeDisruptsServerDuringProvisioning(t *testing.T) {
	t.Parallel()

	contents, err := os.ReadFile(filepath.Join(".", "smoke-metalman.py"))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(contents), "delete_metalman_server_during_provisioning") {
		t.Fatal("traditional smoke does not disrupt a server pod during provisioning")
	}
}

func TestHTTPFixtureFetchesCapabilityEntrypoint(t *testing.T) {
	t.Parallel()

	contents, err := os.ReadFile(filepath.Join(".", "metalman-redfish-fixture.py"))
	if err != nil {
		t.Fatal(err)
	}

	text := string(contents)
	if strings.Contains(text, "cache_dir.glob") {
		t.Fatal("HTTP fixture still reads a process-local Metalman cache")
	}

	if !strings.Contains(text, `("http-entrypoint.efi", boot_url)`) {
		t.Fatal("HTTP fixture does not fetch the capability-scoped firmware URL")
	}
}
