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
	bindingsKind    = "CertificationPlanBindings"
	maxPlanFileSize = 1024 * 1024
)

var (
	artifactNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	suiteNamePattern    = regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)
)

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
	Suite      *CertificationSuiteSource `json:"suite,omitempty"`
}

type CertificationSuiteSource struct {
	Name        string                     `json:"name"`
	Labels      map[string]string          `json:"labels,omitempty"`
	Annotations map[string]string          `json:"annotations,omitempty"`
	Phases      []CertificationPhaseSource `json:"phases"`
}

type CertificationPhaseSource struct {
	Name        string                        `json:"name"`
	DisplayName string                        `json:"displayName,omitempty"`
	Role        signalv1alpha1.CheckStageRole `json:"role"`
	Stages      []CertificationStageSource    `json:"stages"`
}

type CertificationStageSource struct {
	Name                      string                                       `json:"name"`
	DisplayName               string                                       `json:"displayName,omitempty"`
	Test                      string                                       `json:"test"`
	Parameters                []signalv1alpha1.StageParameterBinding       `json:"parameters,omitempty"`
	ParticipantRoles          []signalv1alpha1.StageParticipantRoleBinding `json:"participantRoles,omitempty"`
	Schedule                  signalv1alpha1.StageSchedule                 `json:"schedule"`
	Retry                     *signalv1alpha1.StageRetryPolicy             `json:"retry,omitempty"`
	DependsOn                 []signalv1alpha1.StageDependency             `json:"dependsOn,omitempty"`
	Optional                  bool                                         `json:"optional,omitempty"`
	Assertions                []signalv1alpha1.TestAssertion               `json:"assertions,omitempty"`
	PerDeviceCoverageInterval *metav1.Duration                             `json:"perDeviceCoverageInterval,omitempty"`
}

type CertificationPlanBindings struct {
	APIVersion string                            `json:"apiVersion"`
	Kind       string                            `json:"kind"`
	Metadata   CertificationPlanBindingsMetadata `json:"metadata"`
	Spec       CertificationPlanBindingsSpec     `json:"spec"`
}

type CertificationPlanBindingsMetadata struct {
	Name string `json:"name"`
}

type CertificationPlanBindingsSpec struct {
	Tests map[string]signalv1alpha1.CertificationTestReference `json:"tests"`
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
	return compileFile(filename, "", false)
}

func CompileTestsFile(filename string) ([]byte, error) {
	return compileFile(filename, "", true)
}

func CompileFileWithBindings(filename, bindingsFilename string) ([]byte, error) {
	return compileFile(filename, bindingsFilename, false)
}

func compileFile(filename, bindingsFilename string, testsOnly bool) ([]byte, error) {
	plan, err := readPlan(filename)
	if err != nil {
		return nil, err
	}

	tests, err := compilePlan(filename, plan)
	if err != nil {
		return nil, err
	}

	resources := make([]any, 0, len(tests)+1)
	for index := range tests {
		resources = append(resources, &tests[index])
	}

	if plan.Spec.Suite != nil && !testsOnly {
		if bindingsFilename == "" {
			return nil, errors.New(
				"spec.suite requires --bindings after the generated CertificationTests are approved; " +
					"use --tests-only to bootstrap them",
			)
		}

		bindings, err := readBindings(bindingsFilename)
		if err != nil {
			return nil, err
		}

		suite, err := compileSuite(plan, bindings)
		if err != nil {
			return nil, err
		}

		resources = append(resources, &suite)
	} else if bindingsFilename != "" {
		return nil, errors.New("--bindings requires spec.suite")
	}

	var rendered bytes.Buffer

	for index := range resources {
		if index > 0 {
			rendered.WriteString("---\n")
		}

		document, err := yaml.Marshal(resources[index])
		if err != nil {
			return nil, fmt.Errorf("encode compiled resource: %w", err)
		}

		rendered.Write(document)
	}

	return rendered.Bytes(), nil
}

func BindingTargets(filename string) (string, []string, error) {
	plan, err := readPlan(filename)
	if err != nil {
		return "", nil, err
	}

	if plan.Spec.Suite == nil {
		return "", nil, errors.New("spec.suite is required to resolve bindings")
	}

	tests, err := compilePlan(filename, plan)
	if err != nil {
		return "", nil, err
	}

	referenced := make(map[string]struct{})

	for phaseIndex := range plan.Spec.Suite.Phases {
		for stageIndex := range plan.Spec.Suite.Phases[phaseIndex].Stages {
			referenced[plan.Spec.Suite.Phases[phaseIndex].Stages[stageIndex].Test] = struct{}{}
		}
	}

	names := make([]string, 0, len(referenced))
	for index := range tests {
		if _, exists := referenced[tests[index].Name]; exists {
			names = append(names, tests[index].Name)
			delete(referenced, tests[index].Name)
		}
	}

	for name := range referenced {
		return "", nil, fmt.Errorf("spec.suite references undefined test %q", name)
	}

	return plan.Metadata.Name, names, nil
}

func RenderBindings(
	planName string,
	references map[string]signalv1alpha1.CertificationTestReference,
) ([]byte, error) {
	bindings := CertificationPlanBindings{
		APIVersion: planAPIVersion,
		Kind:       bindingsKind,
		Metadata: CertificationPlanBindingsMetadata{
			Name: planName,
		},
		Spec: CertificationPlanBindingsSpec{
			Tests: references,
		},
	}

	rendered, err := yaml.Marshal(&bindings)
	if err != nil {
		return nil, fmt.Errorf("encode CertificationPlanBindings: %w", err)
	}

	return rendered, nil
}

func readPlan(filename string) (CertificationPlan, error) {
	raw, err := readFileBounded(filename, maxPlanFileSize)
	if err != nil {
		return CertificationPlan{}, fmt.Errorf("read CertificationPlan: %w", err)
	}

	var plan CertificationPlan
	if err := yaml.UnmarshalStrict(raw, &plan); err != nil {
		return CertificationPlan{}, fmt.Errorf("decode CertificationPlan: %w", err)
	}

	return plan, nil
}

func readBindings(filename string) (CertificationPlanBindings, error) {
	raw, err := readFileBounded(filename, maxPlanFileSize)
	if err != nil {
		return CertificationPlanBindings{}, fmt.Errorf("read CertificationPlanBindings: %w", err)
	}

	var bindings CertificationPlanBindings
	if err := yaml.UnmarshalStrict(raw, &bindings); err != nil {
		return CertificationPlanBindings{}, fmt.Errorf("decode CertificationPlanBindings: %w", err)
	}

	if bindings.APIVersion != planAPIVersion {
		return CertificationPlanBindings{}, fmt.Errorf(
			"bindings apiVersion must be %q",
			planAPIVersion,
		)
	}

	if bindings.Kind != bindingsKind {
		return CertificationPlanBindings{}, errors.New(
			`bindings kind must be "CertificationPlanBindings"`,
		)
	}

	return bindings, nil
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

func compileSuite(
	plan CertificationPlan,
	bindings CertificationPlanBindings,
) (signalv1alpha1.CertificationSuite, error) {
	source := plan.Spec.Suite
	if source == nil {
		return signalv1alpha1.CertificationSuite{}, errors.New("spec.suite is required")
	}

	if bindings.Metadata.Name != plan.Metadata.Name {
		return signalv1alpha1.CertificationSuite{}, fmt.Errorf(
			"bindings metadata.name %q must match plan metadata.name %q",
			bindings.Metadata.Name,
			plan.Metadata.Name,
		)
	}

	if errors := validation.IsDNS1123Subdomain(source.Name); len(errors) > 0 {
		return signalv1alpha1.CertificationSuite{}, fmt.Errorf(
			"spec.suite.name is invalid: %s",
			strings.Join(errors, ", "),
		)
	}

	if len(source.Phases) == 0 {
		return signalv1alpha1.CertificationSuite{}, errors.New(
			"spec.suite.phases must contain at least one phase",
		)
	}

	if len(source.Phases) > 16 {
		return signalv1alpha1.CertificationSuite{}, errors.New(
			"spec.suite.phases must not contain more than 16 phases",
		)
	}

	testNames := make(map[string]struct{}, len(plan.Spec.Tests))
	for index := range plan.Spec.Tests {
		testNames[plan.Spec.Tests[index].Name] = struct{}{}
	}

	for name := range bindings.Spec.Tests {
		if _, exists := testNames[name]; !exists {
			return signalv1alpha1.CertificationSuite{}, fmt.Errorf(
				"bindings spec.tests contains undefined test %q",
				name,
			)
		}
	}

	phases := make([]signalv1alpha1.CertificationPhase, len(source.Phases))
	stageLocations := make(map[string][2]int)
	dependencies := make(map[string][]string)
	phaseNames := make(map[string]struct{}, len(source.Phases))
	startupCount := 0
	terminalCount := 0
	stageCount := 0

	for phaseIndex := range source.Phases {
		sourcePhase := source.Phases[phaseIndex]
		if len(sourcePhase.Name) > 63 || !suiteNamePattern.MatchString(sourcePhase.Name) {
			return signalv1alpha1.CertificationSuite{}, fmt.Errorf(
				"spec.suite phase name %q is invalid",
				sourcePhase.Name,
			)
		}

		if _, exists := phaseNames[sourcePhase.Name]; exists {
			return signalv1alpha1.CertificationSuite{}, fmt.Errorf(
				"spec.suite phase name %q is duplicated",
				sourcePhase.Name,
			)
		}

		phaseNames[sourcePhase.Name] = struct{}{}
		if len(sourcePhase.Stages) == 0 {
			return signalv1alpha1.CertificationSuite{}, fmt.Errorf(
				"spec.suite phase %q has no stages",
				sourcePhase.Name,
			)
		}

		switch sourcePhase.Role {
		case signalv1alpha1.CheckStageRoleStartup:
			startupCount++

			if phaseIndex != 0 {
				return signalv1alpha1.CertificationSuite{}, errors.New(
					"spec.suite Startup phase must be first",
				)
			}
		case signalv1alpha1.CheckStageRoleValidation:
		case signalv1alpha1.CheckStageRoleTerminal:
			terminalCount++

			if phaseIndex != len(source.Phases)-1 {
				return signalv1alpha1.CertificationSuite{}, errors.New(
					"spec.suite Terminal phase must be last",
				)
			}
		default:
			return signalv1alpha1.CertificationSuite{}, fmt.Errorf(
				"spec.suite phase %q has invalid role %q",
				sourcePhase.Name,
				sourcePhase.Role,
			)
		}

		phases[phaseIndex] = signalv1alpha1.CertificationPhase{
			Name:        sourcePhase.Name,
			DisplayName: sourcePhase.DisplayName,
			Role:        sourcePhase.Role,
			Stages:      make([]signalv1alpha1.CertificationStage, len(sourcePhase.Stages)),
		}
		stageCount += len(sourcePhase.Stages)

		for stageIndex := range sourcePhase.Stages {
			sourceStage := sourcePhase.Stages[stageIndex]
			if len(sourceStage.Name) > 63 || !suiteNamePattern.MatchString(sourceStage.Name) {
				return signalv1alpha1.CertificationSuite{}, fmt.Errorf(
					"spec.suite stage name %q is invalid",
					sourceStage.Name,
				)
			}

			if location, exists := stageLocations[sourceStage.Name]; exists {
				return signalv1alpha1.CertificationSuite{}, fmt.Errorf(
					"spec.suite stage name %q is duplicated in phases %q and %q",
					sourceStage.Name,
					source.Phases[location[0]].Name,
					sourcePhase.Name,
				)
			}

			if _, exists := testNames[sourceStage.Test]; !exists {
				return signalv1alpha1.CertificationSuite{}, fmt.Errorf(
					"spec.suite stage %q references undefined test %q",
					sourceStage.Name,
					sourceStage.Test,
				)
			}

			binding, exists := bindings.Spec.Tests[sourceStage.Test]
			if !exists {
				return signalv1alpha1.CertificationSuite{}, fmt.Errorf(
					"bindings spec.tests[%q] is required by stage %q",
					sourceStage.Test,
					sourceStage.Name,
				)
			}

			if err := validateTestBinding(sourceStage.Test, binding); err != nil {
				return signalv1alpha1.CertificationSuite{}, err
			}

			if err := signalv1alpha1.ValidateStageSchedule(sourceStage.Schedule); err != nil {
				return signalv1alpha1.CertificationSuite{}, fmt.Errorf(
					"spec.suite phase %q stage %q schedule: %w",
					sourcePhase.Name,
					sourceStage.Name,
					err,
				)
			}

			if sourcePhase.Role != signalv1alpha1.CheckStageRoleValidation &&
				sourceStage.Schedule.Mode != signalv1alpha1.CheckExecutionModeRunOnce {
				return signalv1alpha1.CertificationSuite{}, fmt.Errorf(
					"spec.suite phase %q non-validation stage %q must use RunOnce",
					sourcePhase.Name,
					sourceStage.Name,
				)
			}

			stageLocations[sourceStage.Name] = [2]int{phaseIndex, stageIndex}
			phases[phaseIndex].Stages[stageIndex] = signalv1alpha1.CertificationStage{
				Name:                      sourceStage.Name,
				DisplayName:               sourceStage.DisplayName,
				Test:                      binding,
				Parameters:                sourceStage.Parameters,
				ParticipantRoles:          sourceStage.ParticipantRoles,
				Schedule:                  sourceStage.Schedule,
				Retry:                     sourceStage.Retry,
				DependsOn:                 sourceStage.DependsOn,
				Optional:                  sourceStage.Optional,
				Assertions:                sourceStage.Assertions,
				PerDeviceCoverageInterval: sourceStage.PerDeviceCoverageInterval,
			}
		}
	}

	if startupCount > 1 || terminalCount > 1 {
		return signalv1alpha1.CertificationSuite{}, errors.New(
			"spec.suite allows at most one Startup and one Terminal phase",
		)
	}

	if stageCount > signalv1alpha1.MaxCertificationStages {
		return signalv1alpha1.CertificationSuite{}, fmt.Errorf(
			"spec.suite stage count %d exceeds maximum %d",
			stageCount,
			signalv1alpha1.MaxCertificationStages,
		)
	}

	for phaseIndex := range phases {
		for stageIndex := range phases[phaseIndex].Stages {
			stage := phases[phaseIndex].Stages[stageIndex]
			seen := make(map[string]struct{}, len(stage.DependsOn))

			if len(stage.DependsOn) == 0 && phaseIndex > 0 {
				for previousIndex := range phases[phaseIndex-1].Stages {
					dependencies[stage.Name] = append(
						dependencies[stage.Name],
						phases[phaseIndex-1].Stages[previousIndex].Name,
					)
				}

				continue
			}

			for dependencyIndex := range stage.DependsOn {
				dependency := stage.DependsOn[dependencyIndex]
				if dependency.Stage == stage.Name {
					return signalv1alpha1.CertificationSuite{}, fmt.Errorf(
						"spec.suite stage %q cannot depend on itself",
						stage.Name,
					)
				}

				if _, exists := seen[dependency.Stage]; exists {
					return signalv1alpha1.CertificationSuite{}, fmt.Errorf(
						"spec.suite stage %q dependency %q is duplicated",
						stage.Name,
						dependency.Stage,
					)
				}

				seen[dependency.Stage] = struct{}{}
				if _, exists := stageLocations[dependency.Stage]; !exists {
					return signalv1alpha1.CertificationSuite{}, fmt.Errorf(
						"spec.suite stage %q dependency %q does not exist",
						stage.Name,
						dependency.Stage,
					)
				}

				switch dependency.Condition {
				case "",
					signalv1alpha1.StageDependencyConditionSucceeded,
					signalv1alpha1.StageDependencyConditionCompleted:
				default:
					return signalv1alpha1.CertificationSuite{}, fmt.Errorf(
						"spec.suite stage %q dependency %q has invalid condition %q",
						stage.Name,
						dependency.Stage,
						dependency.Condition,
					)
				}

				dependencies[stage.Name] = append(dependencies[stage.Name], dependency.Stage)
			}
		}
	}

	if err := validateStageDependencyGraph(stageLocations, dependencies); err != nil {
		return signalv1alpha1.CertificationSuite{}, err
	}

	labels := mergeStringMaps(plan.Metadata.Labels, source.Labels)
	annotations := mergeStringMaps(plan.Metadata.Annotations, source.Annotations)
	annotations["signal.unbounded-cloud.io/source-plan"] = plan.Metadata.Name

	return signalv1alpha1.CertificationSuite{
		TypeMeta: metav1.TypeMeta{
			APIVersion: signalv1alpha1.GroupVersion.String(),
			Kind:       "CertificationSuite",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        source.Name,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: signalv1alpha1.CertificationSuiteSpec{
			Phases: phases,
		},
	}, nil
}

func validateTestBinding(
	sourceName string,
	binding signalv1alpha1.CertificationTestReference,
) error {
	if binding.Name != sourceName {
		return fmt.Errorf(
			"bindings spec.tests[%q].name %q must match the source test name",
			sourceName,
			binding.Name,
		)
	}

	if binding.UID == "" {
		return fmt.Errorf("bindings spec.tests[%q].uid is required", sourceName)
	}

	if binding.Generation < 1 {
		return fmt.Errorf(
			"bindings spec.tests[%q].generation must be at least 1",
			sourceName,
		)
	}

	if !signalv1alpha1.IsSHA256Digest(binding.ContentDigest) {
		return fmt.Errorf(
			"bindings spec.tests[%q].contentDigest must be a SHA-256 digest",
			sourceName,
		)
	}

	return nil
}

func validateStageDependencyGraph(
	stageLocations map[string][2]int,
	dependencies map[string][]string,
) error {
	const (
		unvisited = iota
		visiting
		visited
	)

	states := make(map[string]int, len(stageLocations))

	var visit func(string) error

	visit = func(stage string) error {
		switch states[stage] {
		case visiting:
			return fmt.Errorf("spec.suite stage dependency graph contains a cycle at %q", stage)
		case visited:
			return nil
		}

		states[stage] = visiting
		for _, dependency := range dependencies[stage] {
			if err := visit(dependency); err != nil {
				return err
			}
		}

		states[stage] = visited

		return nil
	}

	for stage := range stageLocations {
		if err := visit(stage); err != nil {
			return err
		}
	}

	return nil
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
