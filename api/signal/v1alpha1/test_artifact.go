// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

func FindTestArtifact(spec CertificationTestSpec, name string) (TestArtifact, bool) {
	for index := range spec.Artifacts {
		if spec.Artifacts[index].Name == name {
			return spec.Artifacts[index], true
		}
	}

	return TestArtifact{}, false
}

func ValidateTestArtifacts(spec CertificationTestSpec) error {
	seen := make(map[string]struct{}, len(spec.Artifacts))
	totalBytes := 0

	for index := range spec.Artifacts {
		artifact := spec.Artifacts[index]
		if _, exists := seen[artifact.Name]; exists {
			return fmt.Errorf("duplicate test artifact %q", artifact.Name)
		}

		seen[artifact.Name] = struct{}{}
		if len([]byte(artifact.Content)) > MaxTestArtifactBytes {
			return fmt.Errorf(
				"test artifact %q exceeds the %d-byte limit",
				artifact.Name,
				MaxTestArtifactBytes,
			)
		}

		totalBytes += len([]byte(artifact.Content))
		if totalBytes > MaxTestArtifactsBytes {
			return fmt.Errorf(
				"test artifacts exceed the %d-byte aggregate limit",
				MaxTestArtifactsBytes,
			)
		}

		sum := sha256.Sum256([]byte(artifact.Content))

		actual := "sha256:" + hex.EncodeToString(sum[:])
		if artifact.Digest != actual {
			return fmt.Errorf("test artifact %q digest does not match content", artifact.Name)
		}
	}

	switch spec.Driver.Type {
	case TestDriverTypeScript:
		if spec.Driver.Script == nil {
			return errors.New("script driver is missing")
		}

		if _, found := FindTestArtifact(spec, spec.Driver.Script.Artifact); !found {
			return fmt.Errorf("script artifact %q is missing", spec.Driver.Script.Artifact)
		}
	case TestDriverTypeWorkflowTemplate:
		if spec.Driver.WorkflowTemplate == nil {
			return errors.New("WorkflowTemplate driver is missing")
		}

		name := spec.Driver.WorkflowTemplate.Artifact
		if name != "" {
			if _, found := FindTestArtifact(spec, name); !found {
				return fmt.Errorf("WorkflowTemplate artifact %q is missing", name)
			}
		}
	}

	return nil
}
