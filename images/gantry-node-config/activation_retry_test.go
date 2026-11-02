// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantrynodeconfig_test

import (
	"os"
	"os/exec"
	"testing"
)

func TestConfiguratorRetriesPendingActivation(t *testing.T) {
	for _, action := range []string{"apply", "restore"} {
		for _, failure := range []string{"restart", "is-active", "config-tool"} {
			t.Run(action+"/"+failure, func(t *testing.T) {
				fixture := newConfiguratorFixture(t)
				fixture.run(t, "apply", "changed")
				if err := os.Remove(fixture.systemctlLog); err != nil {
					t.Fatal(err)
				}
				command := exec.Command("/bin/sh", "configure-overlaybd.sh", action)
				command.Env = append(fixture.env, "TEST_CONFIG_RESULT=changed", "TEST_FAIL_SYSTEMCTL_ACTION="+failure)
				if failure == "config-tool" {
					command.Env = append(command.Env, "TEST_CONFIG_FAIL=true")
				}
				if output, err := command.CombinedOutput(); err == nil {
					t.Fatalf("failed activation succeeded: %s", output)
				}
				if _, err := os.Stat(fixture.pendingActivation); err != nil {
					t.Fatalf("pending activation was forgotten: %v", err)
				}
				if _, err := os.Stat(fixture.readyMarker); !os.IsNotExist(err) {
					t.Fatalf("failed activation left readiness marker: %v", err)
				}
				before := len(readLines(t, fixture.systemctlLog))
				fixture.run(t, action, "unchanged")
				calls := readLines(t, fixture.systemctlLog)[before:]
				if len(calls) != 2 || calls[0] != "restart containerd" || calls[1] != "is-active --quiet containerd" {
					t.Fatalf("retry calls = %v, want restart and health verification", calls)
				}
				if _, err := os.Stat(fixture.pendingActivation); !os.IsNotExist(err) {
					t.Fatalf("successful retry retained pending activation: %v", err)
				}
				_, err := os.Stat(fixture.readyMarker)
				if (action == "apply" && err != nil) || (action == "restore" && !os.IsNotExist(err)) {
					t.Fatalf("readiness after %s: %v", action, err)
				}
			})
		}
	}
}
