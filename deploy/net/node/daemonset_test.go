// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package node

import (
	"os"
	"strings"
	"testing"
)

func TestDaemonSetExcludesExternalSyntheticNodes(t *testing.T) {
	contents, err := os.ReadFile("03-daemonset.yaml.tmpl")
	if err != nil {
		t.Fatalf("read DaemonSet template: %v", err)
	}

	template := string(contents)
	for _, expected := range []string{
		"nodeAffinity:",
		"requiredDuringSchedulingIgnoredDuringExecution:",
		"net.unbounded-cloud.io/external-node",
		"operator: DoesNotExist",
	} {
		if !strings.Contains(template, expected) {
			t.Fatalf("DaemonSet template missing %q", expected)
		}
	}
}
