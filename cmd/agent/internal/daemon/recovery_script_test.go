// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"text/template"

	"github.com/stretchr/testify/require"
)

// Execute the actual recovery asset with controlled service-manager responses.
// No root privileges, host systemd calls, or real cooldowns are needed.
func TestRecoveryScript(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		interval string
		mode     string
		wantWait string
		wantErr  string
	}{
		{name: "immediate restart despite denied reset", mode: "immediate"},
		{name: "immediate restart after successful reset", mode: "reset-allowed"},
		{name: "exhausted budget", interval: "1min", wantWait: "65"},
		{name: "compound timespan rounded up", interval: "1min 500ms", wantWait: "66"},
		{name: "maximum interval", interval: "5min", wantWait: "305"},
		{name: "infinite interval", interval: "infinity", wantErr: "unsupported"},
		{name: "disabled limit", interval: "0", wantErr: "unsupported"},
		{name: "zero duration", interval: "0s", wantErr: "no finite positive"},
		{name: "excessive interval", interval: "1h", wantErr: "5min maximum"},
		{name: "unrecognized interval", interval: "garbage", wantErr: "unsupported"},
		{name: "no interval", wantErr: "no finite positive"},
		{name: "second start fails", interval: "1min", mode: "start-fails", wantWait: "65", wantErr: "retrying"},
		{name: "daemon dies after activation", mode: "inactive", wantErr: "could not reset"},
		{name: "wrong executable", mode: "wrong-pid", wantErr: "expected last-known-good"},
		{name: "different running executable", mode: "wrong-executable", wantErr: "expected last-known-good"},
		{name: "selection changes during cooldown", interval: "1min", mode: "selection-changed", wantWait: "65", wantErr: "selection changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			current := filepath.Join(dir, "current")
			lastGood := filepath.Join(dir, "last-good")
			executable, err := os.Executable()
			require.NoError(t, err)
			require.NoError(t, os.Symlink(executable, lastGood))

			pid := os.Getpid()

			if tc.mode == "wrong-executable" {
				other := exec.CommandContext(t.Context(), "sleep", "60")
				require.NoError(t, other.Start())
				t.Cleanup(func() { _ = other.Process.Kill(); _ = other.Wait() })

				pid = other.Process.Pid
			}

			var script bytes.Buffer

			tmpl, err := template.New("recovery").Parse(string(daemonRecoveryScriptContent))
			require.NoError(t, err)
			require.NoError(t, tmpl.Execute(&script, map[string]string{
				"DaemonBinaryCurrentPath":      current,
				"DaemonBinaryLastGoodPath":     lastGood,
				"DaemonAgentUpgradeSignalPath": filepath.Join(dir, "signal"),
				"DaemonUnit":                   "test-agent.service",
				// No mode other than nspawn on this host: the check is
				// skipped whatever the last-good binary is.
				"IsolationModeConfigs":     "none=" + filepath.Join(dir, "absent-none-applied-config.json"),
				"SupportsIsolationCommand": SupportsIsolationCommand,
			}))

			stub := `#!/bin/bash
set -eu
echo "$*" >> "$CALLS"
case "$1" in
 reset-failed) test "$MODE" = reset-allowed ;;
 restart) case "$MODE" in immediate|reset-allowed|inactive|wrong-pid|wrong-executable) exit 0 ;; *) exit 1 ;; esac ;;
 start) test "$MODE" != start-fails ;;
 is-active) test "$MODE" != inactive ;;
 show)
   case "$3" in
     --property=StartLimitIntervalUSec) echo "$INTERVAL" ;;
     --property=MainPID) if test "$MODE" = wrong-pid; then echo 0; else echo "$TEST_PID"; fi ;;
     *) exit 99 ;;
   esac ;;
 *) exit 99 ;;
esac
`
			require.NoError(t, os.WriteFile(filepath.Join(dir, "systemctl"), []byte(stub), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "sleep"), []byte(`#!/bin/bash
echo "sleep $*" >> "$CALLS"
if test "$MODE" = selection-changed; then ln -sfn /bin/false "$CURRENT"; fi
`), 0o755))
			calls := filepath.Join(dir, "calls")
			cmd := exec.CommandContext(t.Context(), "bash", "-s")
			cmd.Stdin = &script

			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"),
				"CALLS="+calls, "MODE="+tc.mode, "INTERVAL="+tc.interval,
				fmt.Sprintf("TEST_PID=%d", pid), "CURRENT="+current)

			output, err := cmd.CombinedOutput()
			if tc.wantErr == "" {
				require.NoError(t, err, "%s", output)
			} else {
				require.Error(t, err, "%s", output)
				require.Contains(t, string(output), tc.wantErr)
			}

			data, err := os.ReadFile(calls)
			require.NoError(t, err)

			commands := string(data)
			require.Equal(t, 1, strings.Count(commands, "restart test-agent.service\n"))

			if tc.wantWait != "" {
				require.Contains(t, commands, "sleep "+tc.wantWait+"\n")

				if tc.mode != "selection-changed" {
					require.Equal(t, 1, strings.Count("\n"+commands, "\nstart test-agent.service\n"))
				} else {
					require.NotContains(t, "\n"+commands, "\nstart test-agent.service\n")
				}
			} else {
				require.NotContains(t, "\n"+commands, "\nstart test-agent.service\n")
			}
		})
	}
}

// On a host whose node runs in a mode other than nspawn, the last-good binary
// must answer the support check for that mode, or recovery refuses to switch to
// it: an agent that predates the mode would operate the node as an nspawn
// machine, or not find it.
func TestRecoveryScriptRefusesALastGoodThatCannotOperateTheMode(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		answer   string
		wantErr  bool
		restarts int
	}{
		{name: "unsupported", answer: "exit 1", wantErr: true},
		{name: "unknown subcommand of an older agent", answer: `echo "unknown command" >&2; exit 1`, wantErr: true},
		// The script then goes on to the normal recovery; the stub systemctl
		// fails the liveness check, which is not what this test is about.
		{name: "supported", answer: "exit 0", restarts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			calls := filepath.Join(dir, "calls")
			appliedNone := filepath.Join(dir, "none-kube1-applied-config.json")
			require.NoError(t, os.WriteFile(appliedNone, []byte("{}"), 0o600))

			lastGood := filepath.Join(dir, "last-good-agent")
			require.NoError(t, os.WriteFile(lastGood, []byte("#!/bin/bash\necho \"agent $*\" >> \"$CALLS\"\n"+tc.answer+"\n"), 0o755))

			lastGoodLink := filepath.Join(dir, "last-good")
			require.NoError(t, os.Symlink(lastGood, lastGoodLink))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "systemctl"), []byte("#!/bin/bash\necho \"$*\" >> \"$CALLS\"\nexit 1\n"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "sleep"), []byte("#!/bin/bash\n"), 0o755))

			var script bytes.Buffer

			tmpl, err := template.New("recovery").Parse(string(daemonRecoveryScriptContent))
			require.NoError(t, err)
			require.NoError(t, tmpl.Execute(&script, map[string]string{
				"DaemonBinaryCurrentPath":      filepath.Join(dir, "current"),
				"DaemonBinaryLastGoodPath":     lastGoodLink,
				"DaemonAgentUpgradeSignalPath": filepath.Join(dir, "signal"),
				"DaemonUnit":                   "test-agent.service",
				"IsolationModeConfigs":         "none=" + appliedNone,
				"SupportsIsolationCommand":     SupportsIsolationCommand,
			}))

			cmd := exec.CommandContext(t.Context(), "bash", "-s")
			cmd.Stdin = &script
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "CALLS="+calls)

			output, err := cmd.CombinedOutput()
			require.Error(t, err, "%s", output)

			data, rerr := os.ReadFile(calls)
			require.NoError(t, rerr)
			require.Contains(t, string(data), "agent supports-isolation none\n")

			if tc.wantErr {
				require.Contains(t, string(output), "cannot operate isolation mode none")
				require.NotContains(t, string(data), "restart", "nothing is switched")

				_, lerr := os.Lstat(filepath.Join(dir, "current"))
				require.ErrorIs(t, lerr, os.ErrNotExist)

				return
			}

			require.Equal(t, tc.restarts, strings.Count(string(data), "restart test-agent.service\n"))
		})
	}
}

func TestIsolationModeConfigsNamesEveryModeButNSpawn(t *testing.T) {
	t.Parallel()

	require.Equal(t, "none=/etc/unbounded/agent/none-kube1-applied-config.json", isolationModeConfigs())
}
