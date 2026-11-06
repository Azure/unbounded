// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantrynodeconfig_test

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfiguratorAppliesAndRestoresOnChange(t *testing.T) {
	fixture := newConfiguratorFixture(t)
	fixture.run(t, "apply", "changed")
	if calls := readLines(t, fixture.systemctlLog); len(calls) != 2 || calls[0] != "restart containerd" {
		t.Fatalf("apply systemctl calls = %v", calls)
	}
	if calls := readLines(t, fixture.modprobeLog); len(calls) != 1 || calls[0] != "nbd nbds_max=1024" {
		t.Fatalf("modprobe calls = %v", calls)
	}

	fixture.run(t, "restore", "changed")
	if calls := readLines(t, fixture.systemctlLog); len(calls) != 4 || calls[2] != "restart containerd" {
		t.Fatalf("restore systemctl calls = %v", calls)
	}
}

func TestConfiguratorDoesNotRestartForUnchangedConfig(t *testing.T) {
	fixture := newConfiguratorFixture(t)
	fixture.run(t, "apply", "unchanged")
	if calls := readLines(t, fixture.systemctlLog); len(calls) != 1 || calls[0] != "is-active --quiet containerd" {
		t.Fatalf("systemctl calls = %v, want only health verification", calls)
	}
}

type configuratorFixture struct {
	env               []string
	systemctlLog      string
	modprobeLog       string
	listener          net.Listener
	readyMarker       string
	pendingActivation string
}

func newConfiguratorFixture(t *testing.T) configuratorFixture {
	t.Helper()

	root := t.TempDir()
	hostRoot := filepath.Join(root, "host")
	binDir := filepath.Join(root, "bin")
	for _, directory := range []string{binDir, hostRoot, filepath.Join(hostRoot, "etc", "containerd")} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	listener, err := net.Listen("unix", filepath.Join(hostRoot, "s.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	systemctlLog := filepath.Join(root, "systemctl.log")
	modprobeLog := filepath.Join(root, "modprobe.log")
	configToolLog := filepath.Join(root, "config-tool.log")
	writeExecutable(t, binDir, "nsenter", `#!/bin/sh
while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do shift; done
shift
exec "$@"
`)
	writeExecutable(t, binDir, "curl", "#!/bin/sh\nexit 0\n")
	writeExecutable(t, binDir, "modprobe", "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$TEST_MODPROBE_LOG\"\n")
	writeExecutable(t, binDir, "systemctl", `#!/bin/sh
printf '%s\n' "$*" >> "$TEST_SYSTEMCTL_LOG"
[ "$1" != "${TEST_FAIL_SYSTEMCTL_ACTION:-}" ]
`)
	writeExecutable(t, binDir, "sh", `#!/bin/sh
if [ "$1" = -c ]; then echo 64; exit 0; fi
exec /bin/sh "$@"
`)
	configTool := writeExecutable(t, binDir, "gantry-node-config", `#!/bin/sh
printf '%s\n' "$*" >> "$TEST_CONFIG_TOOL_LOG"
[ "${TEST_CONFIG_FAIL:-false}" != true ] || exit 1
printf '%s\n' "$TEST_CONFIG_RESULT"
`)

	return configuratorFixture{
		systemctlLog:      systemctlLog,
		modprobeLog:       modprobeLog,
		listener:          listener,
		readyMarker:       filepath.Join(root, "configured"),
		pendingActivation: filepath.Join(hostRoot, "var/lib/gantry/snapshotter-config/pending-activation"),
		env: append(os.Environ(),
			"HOST_ROOT="+hostRoot,
			"CONFIG_TOOL="+configTool,
			"GANTRY_OVERLAYBD_ONESHOT=true",
			"GANTRY_SNAPSHOTTER_ADDRESS=/s.sock",
			"NSENTER_BIN="+filepath.Join(binDir, "nsenter"),
			"CURL_BIN="+filepath.Join(binDir, "curl"),
			"READY_MARKER="+filepath.Join(root, "configured"),
			"TEST_SYSTEMCTL_LOG="+systemctlLog,
			"TEST_MODPROBE_LOG="+modprobeLog,
			"TEST_CONFIG_TOOL_LOG="+configToolLog,
			"PATH="+binDir+":"+os.Getenv("PATH"),
		),
	}
}

func (f configuratorFixture) run(t *testing.T, action, result string) {
	t.Helper()

	command := exec.Command("/bin/sh", "configure-overlaybd.sh", action)
	command.Env = append(f.env, "TEST_CONFIG_RESULT="+result)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("configure %s: %v\n%s", action, err, output)
	}
}

func writeExecutable(t *testing.T, directory, name, content string) string {
	t.Helper()

	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	return path
}

func readLines(t *testing.T, path string) []string {
	t.Helper()

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil
	}

	return strings.Split(trimmed, "\n")
}
