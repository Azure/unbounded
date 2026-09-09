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

			var script bytes.Buffer

			tmpl, err := template.New("recovery").Parse(string(daemonRecoveryScriptContent))
			require.NoError(t, err)
			require.NoError(t, tmpl.Execute(&script, map[string]string{
				"DaemonBinaryCurrentPath":      current,
				"DaemonBinaryLastGoodPath":     lastGood,
				"DaemonAgentUpgradeSignalPath": filepath.Join(dir, "signal"),
				"DaemonUnit":                   "test-agent.service",
			}))

			stub := `#!/bin/bash
set -eu
echo "$*" >> "$CALLS"
case "$1" in
 reset-failed) exit 1 ;;
 restart) case "$MODE" in immediate|inactive|wrong-pid) exit 0 ;; *) exit 1 ;; esac ;;
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
			cmd := exec.Command("bash", "-s")
			cmd.Stdin = &script

			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"),
				"CALLS="+calls, "MODE="+tc.mode, "INTERVAL="+tc.interval,
				fmt.Sprintf("TEST_PID=%d", os.Getpid()), "CURRENT="+current)

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
				}
			} else {
				require.NotContains(t, "\n"+commands, "\nstart test-agent.service\n")
			}
		})
	}
}
