// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signalmanifest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"text/template"

	"github.com/Masterminds/sprig/v3"
	"gopkg.in/yaml.v3"
)

const excludedPackagedExample = "gpu-identity-example.sh"

type packagedScriptContract struct {
	Commit  string            `json:"commit"`
	Scripts map[string]string `json:"scripts"`
}

func TestPackagedCertificationSuiteIsExactAndSubstantiveSetExcludesExample(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("../../project-signal-certification-runner/script-contract.json")
	if err != nil {
		t.Fatal(err)
	}

	var contract packagedScriptContract
	if err := json.Unmarshal(raw, &contract); err != nil {
		t.Fatal(err)
	}

	if len(contract.Scripts) != 15 {
		t.Fatalf("packaged contract has %d scripts, want 15", len(contract.Scripts))
	}

	substantive := make([]string, 0, 14)

	for name := range contract.Scripts {
		path := filepath.Join("../../project-signal-certification/scripts", name)

		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}

		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o755 {
			t.Fatalf("%s mode = %s, want regular 0755", name, info.Mode())
		}

		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}

		sum := sha256.Sum256(content)
		if got := hex.EncodeToString(sum[:]); got != contract.Scripts[name] {
			t.Fatalf("%s sha256 = %s, want %s", name, got, contract.Scripts[name])
		}

		cmd := exec.Command("bash", "-n")

		cmd.Stdin = bytes.NewReader(content)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("bash -n %s: %v: %s", name, err, output)
		}

		if name != excludedPackagedExample {
			substantive = append(substantive, name)
		}
	}

	sort.Strings(substantive)

	if len(substantive) != 14 {
		t.Fatalf("substantive script count = %d, want 14", len(substantive))
	}
}

func TestCertificationProfilesCoverEachSubstantiveScriptOnce(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("10-certification-profiles.yaml.tmpl")
	if err != nil {
		t.Fatal(err)
	}

	parsed, err := template.New("profiles").Funcs(sprig.TxtFuncMap()).Parse(string(raw))
	if err != nil {
		t.Fatal(err)
	}

	var rendered bytes.Buffer
	if err := parsed.Execute(&rendered, map[string]string{
		"RunnerImage": "registry.example/runner@sha256:" + strings.Repeat("a", 64),
	}); err != nil {
		t.Fatal(err)
	}

	var manifest struct {
		Kind  string `yaml:"kind"`
		Items []struct {
			Spec struct {
				Script struct {
					Key string `yaml:"key"`
				} `yaml:"script"`
				RunnerImage string            `yaml:"runnerImage"`
				Parameters  map[string]string `yaml:"parameters"`
			} `yaml:"spec"`
		} `yaml:"items"`
	}
	if err := yaml.Unmarshal(rendered.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}

	if manifest.Kind != "CertificationProfileList" || len(manifest.Items) != 14 {
		t.Fatalf("profile list kind/count = %s/%d", manifest.Kind, len(manifest.Items))
	}

	keys := make(map[string]int, len(manifest.Items))
	for _, item := range manifest.Items {
		keys[item.Spec.Script.Key]++
		if !strings.Contains(item.Spec.RunnerImage, "@sha256:") {
			t.Fatalf("runner image is not digest pinned: %q", item.Spec.RunnerImage)
		}

		if item.Spec.Parameters["require-recovery-gate"] != "true" {
			t.Fatalf("%s does not require the recovery gate", item.Spec.Script.Key)
		}
	}

	for name := range readPackagedContract(t).Scripts {
		want := 1
		if name == excludedPackagedExample {
			want = 0
		}

		if keys[name] != want {
			t.Fatalf("profile count for %s = %d, want %d", name, keys[name], want)
		}
	}
}

func TestRenderedScriptConfigMapContainsExactSubstantiveBytes(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("rendered/03-certification-scripts.yaml")
	if err != nil {
		t.Fatal(err)
	}

	var manifest struct {
		Metadata struct {
			Annotations map[string]string `yaml:"annotations"`
		} `yaml:"metadata"`
		Immutable bool              `yaml:"immutable"`
		Data      map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}

	if !manifest.Immutable || len(manifest.Data) != 14 {
		t.Fatalf("rendered script ConfigMap immutable/count = %t/%d", manifest.Immutable, len(manifest.Data))
	}

	contract := readPackagedContract(t)
	if manifest.Metadata.Annotations["signal.unbounded-cloud.io/script-commit"] != contract.Commit {
		t.Fatalf("script commit annotation = %q, want %q", manifest.Metadata.Annotations["signal.unbounded-cloud.io/script-commit"], contract.Commit)
	}

	for name, rendered := range manifest.Data {
		source, err := os.ReadFile(filepath.Join("../../project-signal-certification/scripts", name))
		if err != nil {
			t.Fatal(err)
		}

		if rendered != string(source) {
			sourceSum := sha256.Sum256(source)
			renderedSum := sha256.Sum256([]byte(rendered))
			t.Fatalf(
				"%s bytes differ: source=%s rendered=%s",
				name,
				hex.EncodeToString(sourceSum[:]),
				hex.EncodeToString(renderedSum[:]),
			)
		}

		sum := sha256.Sum256([]byte(rendered))
		if got := hex.EncodeToString(sum[:]); got != contract.Scripts[name] {
			t.Fatalf("%s rendered sha256 = %s, want %s", name, got, contract.Scripts[name])
		}
	}

	if _, exists := manifest.Data[excludedPackagedExample]; exists {
		t.Fatal("example script was included in the substantive ConfigMap")
	}
}

func readPackagedContract(t *testing.T) packagedScriptContract {
	t.Helper()

	raw, err := os.ReadFile("../../project-signal-certification-runner/script-contract.json")
	if err != nil {
		t.Fatal(err)
	}

	var contract packagedScriptContract
	if err := json.Unmarshal(raw, &contract); err != nil {
		t.Fatal(err)
	}

	return contract
}
