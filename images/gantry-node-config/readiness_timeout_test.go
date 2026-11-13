// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantrynodeconfig_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfiguratorReadinessUsesWallClockBudget(t *testing.T) {
	for _, test := range []struct {
		name          string
		budget        int
		missingSocket bool
		wantProbes    []string
	}{
		{name: "slow probes", budget: 5, wantProbes: []string{"2", "2"}},
		{name: "last probe is capped", budget: 4, wantProbes: []string{"2", "1"}},
		{name: "short budget", budget: 1, wantProbes: []string{"1"}},
		{name: "missing socket", budget: 4, missingSocket: true, wantProbes: []string{"2", "2", "2", "1"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newConfiguratorFixture(t)
			root := filepath.Dir(fixture.readyMarker)
			bin := filepath.Join(root, "bin")
			clock := filepath.Join(root, "clock")
			probes := filepath.Join(root, "probes")
			if err := os.WriteFile(clock, []byte("100\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			writeExecutable(t, bin, "date", `#!/bin/sh
read -r now < "$TEST_CLOCK"
printf '%s\n' "$now"
`)
			writeExecutable(t, bin, "sleep", `#!/bin/sh
read -r now < "$TEST_CLOCK"
printf '%s\n' "$((now + $1))" > "$TEST_CLOCK"
`)
			writeExecutable(t, bin, "curl", `#!/bin/sh
while [ "$1" != --max-time ]; do shift; done
shift
printf '%s\n' "$1" >> "$TEST_PROBES"
[ "$TEST_MISSING_SOCKET" != true ] || exit 0
read -r now < "$TEST_CLOCK"
printf '%s\n' "$((now + $1))" > "$TEST_CLOCK"
exit 1
`)
			if test.missingSocket {
				_ = fixture.listener.Close()
			}
			command := exec.Command("/bin/sh", "configure-overlaybd.sh", "apply")
			command.Env = append(fixture.env, "TEST_CONFIG_RESULT=changed", "TEST_CLOCK="+clock, "TEST_PROBES="+probes,
				fmt.Sprintf("TEST_MISSING_SOCKET=%t", test.missingSocket), fmt.Sprintf("GANTRY_READY_TIMEOUT_SECONDS=%d", test.budget))
			if output, err := command.CombinedOutput(); err == nil || !strings.Contains(string(output), "did not become ready") {
				t.Fatalf("timeout error = %v\n%s", err, output)
			}
			data, err := os.ReadFile(clock)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != fmt.Sprintf("%d\n", 100+test.budget) {
				t.Fatalf("clock exceeded startup budget: %s", data)
			}
			if got := readLines(t, probes); strings.Join(got, ",") != strings.Join(test.wantProbes, ",") {
				t.Fatalf("probe budgets = %v, want %v", got, test.wantProbes)
			}
			if _, err := os.Stat(fixture.readyMarker); !os.IsNotExist(err) {
				t.Fatalf("timeout left readiness marker: %v", err)
			}
			if len(readLines(t, fixture.systemctlLog)) != 0 {
				t.Fatal("timeout activated containerd configuration")
			}
		})
	}
}
