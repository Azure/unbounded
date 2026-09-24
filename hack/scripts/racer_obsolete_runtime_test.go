// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package scripts_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestRacerObsoleteRuntimeInventory(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Fatal("jq is required to test the cutover inventory")
	}

	hex := strings.Repeat("a", 64)
	for _, tc := range []struct {
		name, label, value, owner string
		want                      bool
	}{
		{"racer-v4-topology-" + hex, "rust-state", "pointer", "", true},
		{"racer-v4-storage-" + hex, "rust-state", "pointer", "", true},
		{"racer-v4-chunk-" + hex, "rust-state", "chunk", "", true},
		{"racer-v4-store-gate", "rust-state", "gate", "", true},
		{"racer-pki-" + hex[:16] + "-" + hex, "pki-participants", "v4", "", true},
		{"racer-replica-pod-uid", "", "", "pod-uid", true},
		{"racer-replica-pod-uid", "", "", "other-pod", false},
		{"racer-replica-pod-uid", "", "", "", false},
		{"racer-v4-topology-" + hex, "", "", "", false},
		{"racer-v4-storage-" + hex, "rust-state", "chunk", "", false},
		{"racer-v4-topology-short", "rust-state", "pointer", "", false},
		{"racer-v4-chunk-" + hex + "-backup", "rust-state", "chunk", "", false},
		{"racer-pki-" + hex[:16] + "-" + hex, "pki-participants", "v5", "", false},
		{"racer-runtime-revisions", "rust-state", "pointer", "", false},
		{"racer-trust", "rust-state", "pointer", "", false},
		{"unbounded-component-overrides", "rust-state", "pointer", "", false},
		{"racer-desired-old", "state", "commit", "", false},
	} {
		t.Run(tc.name+"/"+tc.value+"/"+tc.owner, func(t *testing.T) {
			meta := map[string]any{
				"name": tc.name, "uid": "object-uid", "resourceVersion": "42",
				"labels": map[string]string{"racer.unbounded-cloud.io/" + tc.label: tc.value},
			}
			if tc.owner != "" {
				meta["ownerReferences"] = []any{map[string]any{"apiVersion": "v1", "kind": "Pod", "uid": tc.owner, "controller": true}}
			}

			fixture, err := json.Marshal(map[string]any{"items": []any{map[string]any{"metadata": meta}}})
			if err != nil {
				t.Fatal(err)
			}

			output, err := runRacerInventory(string(fixture), "dev-context", "state-ns")
			if err != nil {
				t.Fatalf("inventory: %v: %s", err, output)
			}

			want := ""
			if tc.want {
				want = "configmap/" + tc.name + "\tobject-uid\t42\n"
			}

			if string(output) != want {
				t.Fatalf("inventory = %q, want %q", output, want)
			}
		})
	}
}

func TestRacerObsoleteRuntimeRejectsAmbiguousInvocation(t *testing.T) {
	for _, args := range [][]string{nil, {"dev-context"}, {"", "state-ns"}, {"dev-context", ""}, {"--all", "state-ns"}, {"dev-context", "--all-namespaces"}, {"dev-context", "state-ns", "--delete"}} {
		output, err := runRacerInventory(`{"items":[]}`, args...)
		if err == nil || !strings.Contains(string(output), "Usage:") {
			t.Fatalf("args %q: expected usage failure, got %v: %s", args, err, output)
		}
	}

	output, err := runRacerInventory(`not JSON`, "dev-context", "state-ns")
	if err == nil {
		t.Fatalf("invalid API response accepted: %s", output)
	}
}

func runRacerInventory(fixture string, args ...string) ([]byte, error) {
	// A shell function shadows kubectl, so tests cannot access a real cluster.
	// Any operation beyond one exact namespace-scoped ConfigMap read fails.
	command := `kubectl() {
  if [[ "$*" != '--context=dev-context --namespace=state-ns get configmaps -o json' ]]; then
    return 97
  fi
  printf '%s\n' "$RACER_INVENTORY_FIXTURE"
}
export -f kubectl
bash ./racer-obsolete-runtime.sh "$@"`
	cmd := exec.Command("bash", append([]string{"-c", command, "inventory-test"}, args...)...)
	cmd.Env = append(os.Environ(), "RACER_INVENTORY_FIXTURE="+fixture)

	return cmd.CombinedOutput()
}
