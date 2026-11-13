// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signalplan

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"

	signalv1alpha1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

const (
	planAPIVersion  = "signal.unbounded-cloud.io/v1alpha1"
	planKind        = "CertificationPlan"
	maxPlanFileSize = 1024 * 1024
)

var artifactNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

type CertificationPlan struct {
	APIVersion string                    `json:"apiVersion"`
	Kind       string                    `json:"kind"`
	Metadata   CertificationPlanMetadata `json:"metadata"`
	Spec       CertificationPlanSpec     `json:"spec"`
}

type CertificationPlanMetadata struct {
	Name        string            `json:"name"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type CertificationPlanSpec struct {
	SourceRoot string                    `json:"sourceRoot,omitempty"`
	Owner      string                    `json:"owner"`
	Version    string                    `json:"version"`
	Runtimes   map[string]string         `json:"runtimes,omitempty"`
	Tests      []CertificationTestSource `json:"tests"`
}

type CertificationTestSource struct {
	Name        string                                   `json:"name"`
	DisplayName string                                   `json:"displayName"`
	Description string                                   `json:"description,omitempty"`
	Labels      map[string]string                        `json:"labels,omitempty"`
	Annotations map[string]string                        `json:"annotations,omitempty"`
	Tags        []string                                 `json:"tags,omitempty"`
	Parameters  []signalv1alpha1.TestParameterDefinition `json:"parameters,omitempty"`
	Metrics     []signalv1alpha1.TestMetricDefinition    `json:"metrics,omitempty"`
	Topology    signalv1alpha1.TestTopology              `json:"topology,omitempty"`
	Artifact    *ArtifactSource                          `json:"artifact,omitempty"`
	Driver      CertificationTestDriverSource            `json:"driver"`
	Security    signalv1alpha1.TestSecurityRequirements  `json:"security,omitempty"`
}

type ArtifactSource struct {
	Name string `json:"name,omitempty"`
	Path string `json:"path"`
}

type CertificationTestDriverSource struct {
	Type             signalv1alpha1.TestDriverType `json:"type"`
	Script           *ScriptDriverSource           `json:"script,omitempty"`
	Container        *ContainerDriverSource        `json:"container,omitempty"`
	WorkflowTemplate *WorkflowTemplateDriverSource `json:"workflowTemplate,omitempty"`
}

type ScriptDriverSource struct {
	Language  signalv1alpha1.ScriptLanguage `json:"language"`
	Runtime   string                        `json:"runtime"`
	Resources corev1.ResourceRequirements   `json:"resources,omitempty"`
}

type ContainerDriverSource struct {
	Runtime   string                      `json:"runtime"`
	Command   []string                    `json:"command,omitempty"`
	Args      []string                    `json:"args,omitempty"`
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

type WorkflowTemplateDriverSource struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

func CompileFile(filename string) ([]byte, error) {
	raw, err := readFileBounded(filename, maxPlanFileSize)
	if err != nil {
		return nil, fmt.Errorf("read CertificationPlan: %w", err)
	}

	var plan CertificationPlan
	if err := yaml.UnmarshalStrict(raw, &plan); err != nil {
		return nil, fmt.Errorf("decode CertificationPlan: %w", err)
	}

	tests, err := compilePlan(filename, plan)
	if err != nil {
		return nil, err
	}

	var rendered bytes.Buffer

	for index := range tests {
		if index > 0 {
			rendered.WriteString("---\n")
		}

		document, err := yaml.Marshal(&tests[index])
		if err != nil {
			return nil, fmt.Errorf("encode CertificationTest %q: %w", tests[index].Name, err)
		}

		rendered.Write(document)
	}

	return rendered.Bytes(), nil
}

func readFileBounded(path string, limit int64) (content []byte, err error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := file.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
	}()

	info, err := file.Stat()
	if err != nil {
		return nil, err
	}

	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%q is not a regular file", path)
	}

	content, err = io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}

	if int64(len(content)) > limit {
		return nil, fmt.Errorf("%q exceeds the %d-byte limit", path, limit)
	}

	return content, nil
}

func compilePlan(filename string, plan CertificationPlan) ([]signalv1alpha1.CertificationTest, error) {
	if plan.APIVersion != planAPIVersion {
		return nil, fmt.Errorf("apiVersion must be %q", planAPIVersion)
	}

	if plan.Kind != planKind {
		return nil, fmt.Errorf("kind must be %q", planKind)
	}

	if errors := validation.IsDNS1123Subdomain(plan.Metadata.Name); len(errors) > 0 {
		return nil, fmt.Errorf("metadata.name is invalid: %s", strings.Join(errors, ", "))
	}

	if plan.Spec.Owner == "" {
		return nil, errors.New("spec.owner is required")
	}

	if plan.Spec.Version == "" {
		return nil, errors.New("spec.version is required")
	}

	if len(plan.Spec.Tests) == 0 {
		return nil, errors.New("spec.tests must contain at least one test")
	}

	sourceRoot, err := resolveSourceRoot(filename, plan.Spec.SourceRoot)
	if err != nil {
		return nil, err
	}

	if err := validateRuntimes(plan.Spec.Runtimes); err != nil {
		return nil, err
	}

	seen := make(map[string]struct{}, len(plan.Spec.Tests))

	tests := make([]signalv1alpha1.CertificationTest, 0, len(plan.Spec.Tests))
	for index := range plan.Spec.Tests {
		source := plan.Spec.Tests[index]
		if _, exists := seen[source.Name]; exists {
			return nil, fmt.Errorf("spec.tests[%d].name %q is duplicated", index, source.Name)
		}

		seen[source.Name] = struct{}{}

		test, err := compileTest(plan, sourceRoot, source)
		if err != nil {
			return nil, fmt.Errorf("compile test %q: %w", source.Name, err)
		}

		tests = append(tests, test)
	}

	return tests, nil
}

func resolveSourceRoot(filename, configured string) (string, error) {
	if configured == "" {
		configured = "."
	}

	if filepath.IsAbs(configured) {
		return "", errors.New("spec.sourceRoot must be relative to the plan file")
	}

	filename, err := filepath.Abs(filename)
	if err != nil {
		return "", fmt.Errorf("resolve CertificationPlan path: %w", err)
	}

	root := filepath.Join(filepath.Dir(filename), filepath.Clean(configured))

	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve spec.sourceRoot: %w", err)
	}

	return root, nil
}

func validateRuntimes(runtimes map[string]string) error {
	for name, image := range runtimes {
		if name == "" {
			return errors.New("spec.runtimes contains an empty name")
		}

		if !signalv1alpha1.IsDigestPinnedImage(image) {
			return fmt.Errorf("spec.runtimes[%q] must be a digest-pinned image", name)
		}
	}

	return nil
}

func compileTest(
	plan CertificationPlan,
	sourceRoot string,
	source CertificationTestSource,
) (signalv1alpha1.CertificationTest, error) {
	if errors := validation.IsDNS1123Subdomain(source.Name); len(errors) > 0 {
		return signalv1alpha1.CertificationTest{}, fmt.Errorf("name is invalid: %s", strings.Join(errors, ", "))
	}

	if source.DisplayName == "" {
		return signalv1alpha1.CertificationTest{}, errors.New("displayName is required")
	}

	if (source.Topology.MinParticipants == 0) != (source.Topology.MaxParticipants == 0) {
		return signalv1alpha1.CertificationTest{}, errors.New(
			"topology.minParticipants and topology.maxParticipants must be set together",
		)
	}

	if source.Topology.MinParticipants > source.Topology.MaxParticipants {
		return signalv1alpha1.CertificationTest{}, errors.New(
			"topology.minParticipants must not exceed topology.maxParticipants",
		)
	}

	artifacts, artifactName, err := compileArtifact(sourceRoot, source.Artifact)
	if err != nil {
		return signalv1alpha1.CertificationTest{}, err
	}

	driver, err := compileDriver(plan.Spec.Runtimes, source.Driver, artifactName)
	if err != nil {
		return signalv1alpha1.CertificationTest{}, err
	}

	metrics := source.Metrics
	if len(metrics) == 0 {
		metrics = []signalv1alpha1.TestMetricDefinition{{
			Name:        "scriptResult",
			DisplayName: "Canonical script result",
			Type:        signalv1alpha1.MetricValueTypeStatus,
			Required:    true,
		}}
	}

	topology := source.Topology
	if topology.MinParticipants == 0 && topology.MaxParticipants == 0 {
		topology.MinParticipants = 1
		topology.MaxParticipants = 1
	}

	labels := mergeStringMaps(plan.Metadata.Labels, source.Labels)
	annotations := mergeStringMaps(plan.Metadata.Annotations, source.Annotations)
	annotations["signal.unbounded-cloud.io/source-plan"] = plan.Metadata.Name

	test := signalv1alpha1.CertificationTest{
		TypeMeta: metav1.TypeMeta{
			APIVersion: signalv1alpha1.GroupVersion.String(),
			Kind:       "CertificationTest",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        source.Name,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: signalv1alpha1.CertificationTestSpec{
			DisplayName: source.DisplayName,
			Description: source.Description,
			Owner:       plan.Spec.Owner,
			Version:     plan.Spec.Version,
			Tags:        source.Tags,
			Parameters:  source.Parameters,
			Artifacts:   artifacts,
			Metrics:     metrics,
			Topology:    topology,
			Driver:      driver,
			Security:    source.Security,
		},
	}
	if err := signalv1alpha1.ValidateTestArtifacts(test.Spec); err != nil {
		return signalv1alpha1.CertificationTest{}, err
	}

	return test, nil
}

func compileArtifact(sourceRoot string, source *ArtifactSource) ([]signalv1alpha1.TestArtifact, string, error) {
	if source == nil {
		return nil, "", nil
	}

	if source.Path == "" {
		return nil, "", errors.New("artifact.path is required")
	}

	if filepath.IsAbs(source.Path) {
		return nil, "", errors.New("artifact.path must be relative to spec.sourceRoot")
	}

	path, err := filepath.EvalSymlinks(filepath.Join(sourceRoot, filepath.Clean(source.Path)))
	if err != nil {
		return nil, "", fmt.Errorf("resolve artifact.path %q: %w", source.Path, err)
	}

	relative, err := filepath.Rel(sourceRoot, path)
	if err != nil {
		return nil, "", fmt.Errorf("compare artifact.path with spec.sourceRoot: %w", err)
	}

	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, "", fmt.Errorf("artifact.path %q escapes spec.sourceRoot", source.Path)
	}

	content, err := readFileBounded(path, signalv1alpha1.MaxTestArtifactBytes)
	if err != nil {
		return nil, "", fmt.Errorf("read artifact.path %q: %w", source.Path, err)
	}

	if len(content) == 0 {
		return nil, "", fmt.Errorf("artifact.path %q is empty", source.Path)
	}

	if !utf8.Valid(content) {
		return nil, "", fmt.Errorf("artifact.path %q is not valid UTF-8", source.Path)
	}

	name := source.Name
	if name == "" {
		name = filepath.Base(source.Path)
	}

	if !artifactNamePattern.MatchString(name) {
		return nil, "", fmt.Errorf("artifact.name %q must match %s", name, artifactNamePattern)
	}

	sum := sha256.Sum256(content)

	return []signalv1alpha1.TestArtifact{{
		Name:    name,
		Content: string(content),
		Digest:  "sha256:" + hex.EncodeToString(sum[:]),
	}}, name, nil
}

func compileDriver(
	runtimes map[string]string,
	source CertificationTestDriverSource,
	artifactName string,
) (signalv1alpha1.CertificationTestDriver, error) {
	switch source.Type {
	case signalv1alpha1.TestDriverTypeScript:
		if source.Script == nil || source.Container != nil || source.WorkflowTemplate != nil {
			return signalv1alpha1.CertificationTestDriver{}, errors.New(
				"script driver requires only driver.script",
			)
		}

		if artifactName == "" {
			return signalv1alpha1.CertificationTestDriver{}, errors.New(
				"script driver requires artifact",
			)
		}

		if source.Script.Language != signalv1alpha1.ScriptLanguageShell &&
			source.Script.Language != signalv1alpha1.ScriptLanguagePython {
			return signalv1alpha1.CertificationTestDriver{}, errors.New(
				"driver.script.language must be Shell or Python",
			)
		}

		image, err := resolveRuntime(runtimes, source.Script.Runtime)
		if err != nil {
			return signalv1alpha1.CertificationTestDriver{}, err
		}

		return signalv1alpha1.CertificationTestDriver{
			Type: signalv1alpha1.TestDriverTypeScript,
			Script: &signalv1alpha1.ScriptTestDriver{
				Language:    source.Script.Language,
				Artifact:    artifactName,
				RunnerImage: image,
				Resources:   source.Script.Resources,
			},
		}, nil
	case signalv1alpha1.TestDriverTypeContainer:
		if source.Container == nil || source.Script != nil || source.WorkflowTemplate != nil {
			return signalv1alpha1.CertificationTestDriver{}, errors.New(
				"container driver requires only driver.container",
			)
		}

		if artifactName != "" {
			return signalv1alpha1.CertificationTestDriver{}, errors.New(
				"container driver must not declare artifact",
			)
		}

		image, err := resolveRuntime(runtimes, source.Container.Runtime)
		if err != nil {
			return signalv1alpha1.CertificationTestDriver{}, err
		}

		return signalv1alpha1.CertificationTestDriver{
			Type: signalv1alpha1.TestDriverTypeContainer,
			Container: &signalv1alpha1.ContainerTestDriver{
				Image:     image,
				Command:   source.Container.Command,
				Args:      source.Container.Args,
				Resources: source.Container.Resources,
			},
		}, nil
	case signalv1alpha1.TestDriverTypeWorkflowTemplate:
		if source.WorkflowTemplate == nil || source.Script != nil || source.Container != nil {
			return signalv1alpha1.CertificationTestDriver{}, errors.New(
				"WorkflowTemplate driver requires only driver.workflowTemplate",
			)
		}

		if !signalv1alpha1.IsSHA256Digest(source.WorkflowTemplate.Digest) {
			return signalv1alpha1.CertificationTestDriver{}, errors.New(
				"driver.workflowTemplate.digest must be a SHA-256 digest",
			)
		}

		if source.WorkflowTemplate.Name == "" {
			return signalv1alpha1.CertificationTestDriver{}, errors.New(
				"driver.workflowTemplate.name is required",
			)
		}

		return signalv1alpha1.CertificationTestDriver{
			Type: signalv1alpha1.TestDriverTypeWorkflowTemplate,
			WorkflowTemplate: &signalv1alpha1.WorkflowTemplateTestDriver{
				Name:     source.WorkflowTemplate.Name,
				Digest:   source.WorkflowTemplate.Digest,
				Artifact: artifactName,
			},
		}, nil
	default:
		return signalv1alpha1.CertificationTestDriver{}, fmt.Errorf(
			"unsupported driver.type %q",
			source.Type,
		)
	}
}

func resolveRuntime(runtimes map[string]string, name string) (string, error) {
	if name == "" {
		return "", errors.New("driver runtime is required")
	}

	image, found := runtimes[name]
	if !found {
		return "", fmt.Errorf("driver runtime %q is not defined in spec.runtimes", name)
	}

	return image, nil
}

func mergeStringMaps(base, override map[string]string) map[string]string {
	merged := make(map[string]string, len(base)+len(override)+1)
	for key, value := range base {
		merged[key] = value
	}

	for key, value := range override {
		merged[key] = value
	}

	return merged
}
