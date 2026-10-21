// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package ipam

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/containernetworking/plugins/plugins/ipam/host-local/backend/disk"

	ipamproto "github.com/Azure/unbounded/internal/net/ipam/proto"
)

func TestCNIHelperProcess(t *testing.T) {
	if os.Getenv("UNBOUNDED_IPAM_TEST_HELPER") != "1" {
		return
	}

	plugin := Plugin{DataDir: os.Getenv("UNBOUNDED_IPAM_TEST_DIR")}
	skel.PluginMainFuncs(plugin.Functions(), version.All, "unbounded-ipam-test")
	os.Exit(0)
}

func runCNI(t *testing.T, command, path, directory, id string) ([]byte, error) {
	t.Helper()

	return runCNIInput(t, command, testArgs(t, path, "net", id).StdinData, directory, id)
}

func runCNIInput(t *testing.T, command string, input []byte, directory, id string) ([]byte, error) {
	t.Helper()

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestCNIHelperProcess$")

	cmd.Env = append(os.Environ(),
		"UNBOUNDED_IPAM_TEST_HELPER=1", "UNBOUNDED_IPAM_TEST_DIR="+directory,
		"CNI_COMMAND="+command, "CNI_CONTAINERID="+id,
		"CNI_NETNS=/proc/self/ns/net", "CNI_IFNAME=eth0", "CNI_PATH="+directory, "CNI_ARGS=",
		"CNI_NETNS_OVERRIDE=1",
	)
	cmd.Stdin = bytes.NewReader(input)

	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
	if err != nil {
		t.Logf("%s stderr: %s", command, stderr.String())
	}

	return stdout.Bytes(), err
}

func TestCNISubprocessVersionsAndUnsupportedCommands(t *testing.T) {
	conf := testConfig(t, "10.244.2.0/24")
	path := startTestServer(t, func() (*ipamproto.Config, error) { return conf, nil })

	for _, specVersion := range []string{"0.1.0", "0.2.0", "0.3.0", "0.3.1", "0.4.0", "1.0.0", "1.1.0"} {
		t.Run(specVersion, func(t *testing.T) {
			directory := t.TempDir()
			input := map[string]any{
				"name": "net", "cniVersion": specVersion, "type": "bridge",
				"ipam": map[string]string{"type": "unbounded-ipam", "socketPath": path},
			}

			data, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}

			output, err := runCNIInput(t, "ADD", data, directory, "pod")
			if err != nil || !json.Valid(output) {
				t.Fatalf("ADD result is not pure JSON: %s, %v", output, err)
			}

			var result struct {
				CNIVersion string `json:"cniVersion"`
			}
			if err := json.Unmarshal(output, &result); err != nil || result.CNIVersion != specVersion {
				t.Fatalf("incorrect result version: %s, %v", output, err)
			}

			if _, err := runCNIInput(t, "DEL", data, directory, "pod"); err != nil {
				t.Fatal(err)
			}
		})
	}

	for _, command := range []string{"GC", "STATUS"} {
		t.Run(command, func(t *testing.T) {
			input := []byte(`{"name":"net","cniVersion":"1.1.0","ipam":{"type":"unbounded-ipam"}}`)

			output, err := runCNIInput(t, command, input, t.TempDir(), "pod")
			if err == nil || !json.Valid(output) || !strings.Contains(string(output), "does not support "+command) {
				t.Fatalf("unsupported command succeeded or lost its CNI error: %s, %v", output, err)
			}
		})
	}
}

func TestCNISubprocessRequestedIPs(t *testing.T) {
	conf := testConfig(t, "10.244.2.0/24")
	path := startTestServer(t, func() (*ipamproto.Config, error) { return conf, nil })

	for _, field := range []string{"runtimeConfig", "RuntimeConfig", "args", "ARGS"} {
		for _, value := range []string{`null`, `"10.244.2.9"`} {
			t.Run(field+"/"+value, func(t *testing.T) {
				args := testArgs(t, path, "net", "pod")

				var input map[string]json.RawMessage
				if err := json.Unmarshal(args.StdinData, &input); err != nil {
					t.Fatal(err)
				}

				options := `{"IPS":[` + value + `]}`
				if strings.EqualFold(field, "args") {
					options = `{"CNI":` + options + `}`
				}

				input[field] = json.RawMessage(options)

				data, err := json.Marshal(input)
				if err != nil {
					t.Fatal(err)
				}

				directory := t.TempDir()

				output, err := runCNIInput(t, "ADD", data, directory, "pod")
				if value != "null" {
					if err != nil || !json.Valid(output) || !strings.Contains(string(output), "10.244.2.9/24") {
						t.Fatalf("valid requested IP failed: %s, %v", output, err)
					}

					return
				}

				if err == nil || !json.Valid(output) || !strings.Contains(string(output), "requested IP list must not contain null") {
					t.Fatalf("null requested IP did not return a structured CNI error: %s, %v", output, err)
				}

				entries, err := os.ReadDir(directory)
				if err != nil || len(entries) != 0 {
					t.Fatalf("null requested IP created reservations: %v, %v", entries, err)
				}
			})
		}
	}
}

func TestCNISubprocessRebootRace(t *testing.T) {
	dir := t.TempDir()

	store, err := disk.New("net", dir)
	if err != nil {
		t.Fatal(err)
	}

	if err := store.Lock(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "net", "10.244.1.2"), []byte("legacy-pod\r\neth0"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := store.Unlock(); err != nil {
		t.Fatal(err)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var (
		mu       sync.Mutex
		verified *ipamproto.Config
	)

	path := startTestServer(t, func() (*ipamproto.Config, error) {
		mu.Lock()
		defer mu.Unlock()

		if verified == nil {
			return nil, errors.New("waiting for Kubernetes startup verification")
		}

		return verified, nil
	})
	for _, socket := range []string{"/missing/ipam.sock", path} {
		output, err := runCNI(t, "ADD", socket, dir, "new-pod")
		if err == nil || !json.Valid(output) {
			t.Fatalf("unverified ADD did not return a structured CNI error: %s, %v", output, err)
		}
	}

	if _, err := os.Stat(filepath.Join(dir, "net", "10.244.2.2")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed ADD reserved an IP: %v", err)
	}

	mu.Lock()
	verified = testConfig(t, "10.244.2.0/24", "fd00:2::/64")
	mu.Unlock()

	output, err := runCNI(t, "ADD", path, dir, "new-pod")
	if err != nil || !json.Valid(output) || !strings.Contains(string(output), "10.244.2.2/24") || !strings.Contains(string(output), "fd00:2::2/64") {
		t.Fatalf("new assignment allocation: %s, %v", output, err)
	}

	mu.Lock()
	verified = nil
	mu.Unlock()

	for _, command := range []string{"CHECK", "VERSION", "DEL", "DEL"} {
		output, err := runCNI(t, command, "/missing/socket", dir, "new-pod")
		if err != nil {
			t.Fatalf("offline %s: %s, %v", command, output, err)
		}

		if command == "DEL" && len(output) != 0 {
			t.Fatalf("DEL wrote stdout: %s", output)
		}
	}

	if _, err := runCNI(t, "DEL", "/missing/socket", dir, "legacy-pod"); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(dir, "net", "10.244.1.2")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old CIDR reservation survived DEL: %v", err)
	}
}
