// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantrynodeconfig_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const originalConfig = `{"logConfig":{"logLevel":1},"p2pConfig":{"enable":false,"address":""},"other":{"preserve":true}}`

type configuratorFixture struct {
	hostRoot string
	config   string
	stateDir string
	log      string
	env      []string
}

func newConfiguratorFixture(t *testing.T) configuratorFixture {
	t.Helper()

	root := t.TempDir()
	hostRoot := filepath.Join(root, "host")
	config := filepath.Join(hostRoot, "etc", "overlaybd", "overlaybd.json")
	stateDir := filepath.Join(hostRoot, "var", "lib", "gantry", "overlaybd-config")
	binDir := filepath.Join(root, "bin")
	logPath := filepath.Join(root, "systemctl.log")

	for _, directory := range []string{filepath.Dir(config), binDir} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.WriteFile(config, []byte(originalConfig), 0o644); err != nil {
		t.Fatal(err)
	}

	nsenter := writeExecutable(t, binDir, "nsenter", `#!/bin/sh
while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do shift; done
shift
exec "$@"
`)
	curl := writeExecutable(t, binDir, "curl", "#!/bin/sh\nexit 0\n")
	writeExecutable(t, binDir, "systemctl", `#!/bin/sh
printf '%s\n' "$*" >> "$TEST_SYSTEMCTL_LOG"
exit 0
`)
	configTool := writeExecutable(t, binDir, "config.sh", `#!/bin/sh
key=$1
value=$2
tmp="$TEST_HOST_CONFIG.tmp"
case "$key" in
p2pConfig.enable)
  jq --argjson value "$value" '.p2pConfig.enable=$value' "$TEST_HOST_CONFIG" > "$tmp"
  ;;
p2pConfig.address)
  jq --arg value "$value" '.p2pConfig.address=$value' "$TEST_HOST_CONFIG" > "$tmp"
  ;;
*) exit 2 ;;
esac
mv "$tmp" "$TEST_HOST_CONFIG"
`)

	return configuratorFixture{
		hostRoot: hostRoot,
		config:   config,
		stateDir: stateDir,
		log:      logPath,
		env: append(os.Environ(),
			"HOST_ROOT="+hostRoot,
			"OVERLAYBD_P2P_ADDRESS=http://localhost:5000/blobs",
			"OVERLAYBD_CONFIG_TOOL="+configTool,
			"GANTRY_OVERLAYBD_ONESHOT=true",
			"NSENTER_BIN="+nsenter,
			"CURL_BIN="+curl,
			"TEST_HOST_CONFIG="+config,
			"TEST_SYSTEMCTL_LOG="+logPath,
			"READY_MARKER="+filepath.Join(root, "run", "configured"),
			"PATH="+binDir+":"+os.Getenv("PATH"),
		),
	}
}

func (f configuratorFixture) run(t *testing.T, action string) string {
	t.Helper()

	command := exec.Command("sh", "configure-overlaybd.sh", action)
	command.Env = f.env
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("configure-overlaybd.sh %s: %v\n%s", action, err, output)
	}

	return string(output)
}

func TestConfiguratorApplyIsIdempotentAndRestoreReverts(t *testing.T) {
	fixture := newConfiguratorFixture(t)
	fixture.run(t, "apply")

	assertDesiredConfig(t, fixture.config)
	if got := countLines(t, fixture.log); got != 4 {
		t.Fatalf("systemctl calls after apply = %d, want 4", got)
	}

	fixture.run(t, "apply")
	if got := countLines(t, fixture.log); got != 4 {
		t.Fatalf("systemctl calls after no-op apply = %d, want 4", got)
	}

	fixture.run(t, "restore")
	restored, err := os.ReadFile(fixture.config)
	if err != nil {
		t.Fatal(err)
	}

	if string(restored) != originalConfig {
		t.Fatalf("restored config = %s, want %s", restored, originalConfig)
	}

	if got := countLines(t, fixture.log); got != 8 {
		t.Fatalf("systemctl calls after restore = %d, want 8", got)
	}

	for _, name := range []string{"original.json", "managed.json"} {
		if _, err := os.Stat(filepath.Join(fixture.stateDir, name)); !os.IsNotExist(err) {
			t.Fatalf("state file %s remains after restore; err=%v", name, err)
		}
	}
}

func TestConfiguratorRestorePreservesConcurrentChange(t *testing.T) {
	fixture := newConfiguratorFixture(t)
	fixture.run(t, "apply")

	changedPath := fixture.config + ".changed"
	command := exec.Command("jq", ".other.preserve=false", fixture.config)
	changed, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(changedPath, changed, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(changedPath, fixture.config); err != nil {
		t.Fatal(err)
	}

	output := fixture.run(t, "restore")
	if !strings.Contains(output, "preserving current host config") {
		t.Fatalf("restore output = %q, want preservation warning", output)
	}

	got, err := os.ReadFile(fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(changed) {
		t.Fatalf("concurrent config was overwritten: %s", got)
	}

	if calls := countLines(t, fixture.log); calls != 4 {
		t.Fatalf("systemctl calls after refused restore = %d, want 4", calls)
	}
}

func TestConfiguratorResidentProcessReleasesRestoreLock(t *testing.T) {
	fixture := newConfiguratorFixture(t)
	resident := exec.Command("sh", "configure-overlaybd.sh", "apply")
	resident.Env = replaceEnv(fixture.env, "GANTRY_OVERLAYBD_ONESHOT", "false")
	if err := resident.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = resident.Process.Kill()
		_ = resident.Wait()
	})

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(fixture.stateDir, "managed.json")); err == nil {
			break
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("resident configurator did not finish apply")
		}
		time.Sleep(10 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	restore := exec.CommandContext(ctx, "sh", "configure-overlaybd.sh", "restore")
	restore.Env = fixture.env
	if output, err := restore.CombinedOutput(); err != nil {
		t.Fatalf("restore while resident configurator runs: %v\n%s", err, output)
	}

	restored, err := os.ReadFile(fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != originalConfig {
		t.Fatalf("restored config = %s, want %s", restored, originalConfig)
	}
}

func replaceEnv(environment []string, key, value string) []string {
	prefix := key + "="
	replaced := append([]string(nil), environment...)
	for index, entry := range replaced {
		if strings.HasPrefix(entry, prefix) {
			replaced[index] = prefix + value

			return replaced
		}
	}

	return append(replaced, prefix+value)
}

func writeExecutable(t *testing.T, directory, name, content string) string {
	t.Helper()

	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	return path
}

// assertDesiredConfig also pins that settings Gantry does not own are left
// exactly as the host had them.
func assertDesiredConfig(t *testing.T, path string) {
	t.Helper()

	command := exec.Command("jq", "-e", `.p2pConfig.enable == true and .p2pConfig.address == "http://localhost:5000/blobs" and .logConfig.logLevel == 1 and .other.preserve == true`, path)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("desired config check: %v\n%s", err, output)
	}
}

func countLines(t *testing.T, path string) int {
	t.Helper()

	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}

	trimmed := strings.TrimSpace(string(content))
	if trimmed == "" {
		return 0
	}

	return strings.Count(trimmed, "\n") + 1
}
