// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestValidateTestArtifacts(t *testing.T) {
	t.Parallel()

	content := "#!/bin/sh\nexit 0\n"
	sum := sha256.Sum256([]byte(content))
	digest := "sha256:" + hex.EncodeToString(sum[:])
	spec := CertificationTestSpec{
		Artifacts: []TestArtifact{{
			Name:    "check.sh",
			Content: content,
			Digest:  digest,
		}},
		Driver: CertificationTestDriver{
			Type: TestDriverTypeScript,
			Script: &ScriptTestDriver{
				Language:    ScriptLanguageShell,
				Artifact:    "check.sh",
				RunnerImage: "example.invalid/runner@sha256:" + strings.Repeat("a", 64),
			},
		},
	}

	if err := ValidateTestArtifacts(spec); err != nil {
		t.Fatalf("ValidateTestArtifacts() error = %v", err)
	}

	t.Run("tampered content", func(t *testing.T) {
		t.Parallel()

		tampered := spec
		tampered.Artifacts = append([]TestArtifact(nil), spec.Artifacts...)
		tampered.Artifacts[0].Content = content + "echo changed\n"

		if err := ValidateTestArtifacts(tampered); err == nil {
			t.Fatal("ValidateTestArtifacts() accepted content that does not match its digest")
		}
	})

	t.Run("missing driver artifact", func(t *testing.T) {
		t.Parallel()

		missing := spec
		missing.Driver = *spec.Driver.DeepCopy()
		missing.Driver.Script.Artifact = "missing.sh"

		if err := ValidateTestArtifacts(missing); err == nil {
			t.Fatal("ValidateTestArtifacts() accepted a missing driver artifact")
		}
	})

	t.Run("oversized content", func(t *testing.T) {
		t.Parallel()

		oversized := spec
		oversized.Artifacts = append([]TestArtifact(nil), spec.Artifacts...)
		oversized.Artifacts[0].Content = strings.Repeat("x", MaxTestArtifactBytes+1)
		sum := sha256.Sum256([]byte(oversized.Artifacts[0].Content))
		oversized.Artifacts[0].Digest = "sha256:" + hex.EncodeToString(sum[:])

		if err := ValidateTestArtifacts(oversized); err == nil {
			t.Fatal("ValidateTestArtifacts() accepted oversized artifact content")
		}
	})

	t.Run("oversized aggregate content", func(t *testing.T) {
		t.Parallel()

		oversized := spec
		oversized.Driver = *spec.Driver.DeepCopy()
		oversized.Artifacts = nil

		for index := 0; index < 4; index++ {
			content := strings.Repeat(string(rune('a'+index)), MaxTestArtifactBytes)
			sum := sha256.Sum256([]byte(content))
			oversized.Artifacts = append(oversized.Artifacts, TestArtifact{
				Name:    string(rune('a'+index)) + ".sh",
				Content: content,
				Digest:  "sha256:" + hex.EncodeToString(sum[:]),
			})
		}

		oversized.Driver.Script.Artifact = oversized.Artifacts[0].Name

		if err := ValidateTestArtifacts(oversized); err == nil {
			t.Fatal("ValidateTestArtifacts() accepted oversized aggregate artifact content")
		}
	})
}
