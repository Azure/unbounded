// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

func TestRacerEnvtestCIContract(t *testing.T) {
	makefile, err := os.ReadFile("../../Makefile")
	require.NoError(t, err)

	target := func(name string) string {
		t.Helper()

		_, body, found := strings.Cut(string(makefile), "\n"+name+":")
		require.True(t, found, "missing target %s", name)

		body, _, _ = strings.Cut(body, "\n\n")

		return body
	}

	run := target("racer-envtest")
	for _, pkg := range []string{"./internal/racer", "./internal/racer/authority"} {
		require.Contains(t, strings.Fields(run), pkg)
	}

	// A prefix includes new envtest cases without an allowlist.
	require.Contains(t, run, "-run '^TestEnvtest'")
	require.Contains(t, run, "$(GOTEST) -race")
	require.Contains(t, run, "-count=1")
	require.Contains(t, run, "-timeout=5m")
	require.Contains(t, run, "timeout --signal=TERM --kill-after=10s 300s")
	require.Contains(t, run, `test -n "$(KUBEBUILDER_ASSETS)" ||`)
	require.Contains(t, run, `KUBEBUILDER_ASSETS="$(KUBEBUILDER_ASSETS)"`)

	provision := target("racer-envtest-ci")
	require.Contains(t, provision, "$(SETUP_ENVTEST)")
	require.Contains(t, provision, "use $(ENVTEST_K8S_VERSION)")
	require.Contains(t, provision, `$(MAKE) racer-envtest KUBEBUILDER_ASSETS="$$assets"`)

	workflow, err := os.ReadFile("../../.github/workflows/ci.yaml")
	require.NoError(t, err)

	var ci struct {
		Jobs map[string]struct {
			Steps []struct {
				Run string `json:"run"`
			} `json:"steps"`
		} `json:"jobs"`
	}

	require.NoError(t, yaml.Unmarshal(workflow, &ci))

	var commands []string
	for _, step := range ci.Jobs["racer-envtest"].Steps {
		commands = append(commands, step.Run)
	}

	require.Contains(t, commands, "timeout --signal=TERM --kill-after=10s 300s make racer-envtest-ci")
}
