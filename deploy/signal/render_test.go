// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"

	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
)

const (
	testControllerImage = "example.invalid/controller@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testAdapterImage    = "example.invalid/adapter@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testRunnerImage     = "example.invalid/runner@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	testDigest          = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	qualificationGuard  = "node-authorizer-node-restriction-rbac-verified"
)

func TestRenderedExecutionProvenanceIsPinnedAndDeterministic(t *testing.T) {
	t.Parallel()

	firstOutput := renderSignal(t, signalRenderData())
	firstWorkflow := readRenderedFile(t, firstOutput, "04-adapter-workflowtemplate.yaml")
	sum := sha256.Sum256(firstWorkflow)
	workflowDigest := fmt.Sprintf("sha256:%x", sum)

	data := signalRenderData()
	data["WorkflowTemplateDigest"] = workflowDigest
	secondOutput := renderSignal(t, data)
	secondWorkflow := readRenderedFile(t, secondOutput, "04-adapter-workflowtemplate.yaml")

	if !bytes.Equal(firstWorkflow, secondWorkflow) {
		t.Fatal("WorkflowTemplate bytes changed when only its recorded digest changed")
	}

	changedData := signalRenderData()
	changedData["AdapterImage"] = "example.invalid/adapter@sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

	changedWorkflow := readRenderedFile(t, renderSignal(t, changedData), "04-adapter-workflowtemplate.yaml")
	if sha256.Sum256(changedWorkflow) == sum {
		t.Fatal("WorkflowTemplate digest did not change with adapter image semantics")
	}

	var workflow struct {
		Spec struct {
			Templates []struct {
				Name      string `json:"name"`
				Container struct {
					Image string `json:"image"`
				} `json:"container"`
			} `json:"templates"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(secondWorkflow, &workflow); err != nil {
		t.Fatalf("decode WorkflowTemplate: %v", err)
	}

	if len(workflow.Spec.Templates) != 1 ||
		workflow.Spec.Templates[0].Name != "certification" ||
		workflow.Spec.Templates[0].Container.Image != testAdapterImage {
		t.Fatalf("WorkflowTemplate adapter identity = %#v", workflow.Spec.Templates)
	}

	var deployment appsv1.Deployment
	if err := yaml.Unmarshal(readRenderedFile(t, secondOutput, "06-deployment.yaml"), &deployment); err != nil {
		t.Fatalf("decode Deployment: %v", err)
	}

	if len(deployment.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("controller containers = %d, want 1", len(deployment.Spec.Template.Spec.Containers))
	}

	controller := deployment.Spec.Template.Spec.Containers[0]
	if controller.Image != testControllerImage {
		t.Fatalf("controller image = %q, want %q", controller.Image, testControllerImage)
	}

	for _, argument := range []string{
		"--script-namespace=script-ns",
		"--runner-image=" + testRunnerImage,
		"--controller-image=" + testControllerImage,
		"--adapter-image=" + testAdapterImage,
		"--workflow-template-digest=" + workflowDigest,
		"--qualification-gate-protected=false",
	} {
		if !slices.Contains(controller.Args, argument) {
			t.Fatalf("controller args do not contain %q: %v", argument, controller.Args)
		}
	}
}

func TestRenderRejectsMutableExecutionProvenance(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name  string
		key   string
		value string
		error string
	}{
		{
			name:  "controller image",
			key:   "ControllerImage",
			value: "example.invalid/controller:latest",
			error: "ControllerImage must be an immutable sha256 digest reference",
		},
		{
			name:  "adapter image",
			key:   "AdapterImage",
			value: "example.invalid/adapter:latest",
			error: "AdapterImage must be an immutable sha256 digest reference",
		},
		{
			name:  "runner image",
			key:   "RunnerImage",
			value: "example.invalid/runner:latest",
			error: "RunnerImage must be an immutable sha256 digest reference",
		},
		{
			name:  "workflow digest",
			key:   "WorkflowTemplateDigest",
			value: "sha256:not-a-digest",
			error: "WorkflowTemplateDigest must be an algorithm-qualified sha256 digest",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			data := signalRenderData()
			data[test.key] = test.value

			err := render.Render(".", t.TempDir(), data)
			if err == nil || !strings.Contains(err.Error(), test.error) {
				t.Fatalf("render error = %v, want %q", err, test.error)
			}
		})
	}
}

func TestQualificationGateRequiresExplicitGuard(t *testing.T) {
	t.Parallel()

	data := signalRenderData()
	data["QualificationGateProtected"] = "true"

	if err := render.Render(".", t.TempDir(), data); err == nil ||
		!strings.Contains(err.Error(), "explicit Node authorizer") {
		t.Fatalf("unguarded protected gate error = %v", err)
	}

	data["QualificationGateGuard"] = qualificationGuard
	output := renderSignal(t, data)

	var deployment appsv1.Deployment
	if err := yaml.Unmarshal(readRenderedFile(t, output, "06-deployment.yaml"), &deployment); err != nil {
		t.Fatalf("decode guarded Deployment: %v", err)
	}

	if !slices.Contains(
		deployment.Spec.Template.Spec.Containers[0].Args,
		"--qualification-gate-protected=true",
	) {
		t.Fatalf("guarded Deployment does not enable qualification projection")
	}

	data = signalRenderData()
	data["QualificationGateProtected"] = "enabled"

	if err := render.Render(".", t.TempDir(), data); err == nil ||
		!strings.Contains(err.Error(), "must be true or false") {
		t.Fatalf("invalid qualification gate error = %v", err)
	}
}

func TestRenderedRBACCoversQualificationAndEvidence(t *testing.T) {
	t.Parallel()

	output := renderSignal(t, signalRenderData())
	controllerDocuments := splitYAMLDocuments(string(readRenderedFile(t, output, "02-rbac.yaml")))

	assertRule(
		t,
		controllerDocuments,
		"ClusterRole",
		"",
		"project-signal-controller",
		"",
		"nodes",
		[]string{"get", "list", "watch", "update", "patch"},
	)
	assertRule(
		t,
		controllerDocuments,
		"ClusterRole",
		"",
		"project-signal-controller",
		"signal.unbounded-cloud.io",
		"certificationpolicies",
		[]string{"get", "list", "watch", "update", "patch"},
	)
	assertRule(
		t,
		controllerDocuments,
		"Role",
		"signal-ns",
		"project-signal-controller",
		"signal.unbounded-cloud.io",
		"certificationruns",
		[]string{"get", "list", "watch", "create", "update", "patch", "delete"},
	)
	assertRule(
		t,
		controllerDocuments,
		"Role",
		"signal-ns",
		"project-signal-controller",
		"signal.unbounded-cloud.io",
		"nodecertifications",
		[]string{"get", "list", "watch", "create", "update", "patch", "delete"},
	)

	adapterDocuments := splitYAMLDocuments(string(readRenderedFile(
		t,
		output,
		"03-adapter-serviceaccount-rbac.yaml",
	)))
	assertRule(
		t,
		adapterDocuments,
		"Role",
		"signal-ns",
		"project-signal-adapter",
		"argoproj.io",
		"workflowtaskresults",
		[]string{"create", "patch"},
	)
	assertRule(
		t,
		adapterDocuments,
		"ClusterRole",
		"",
		"project-signal-adapter-node-reader",
		"",
		"nodes",
		[]string{"get"},
	)
}

func TestGeneratedCRDSetContainsAllCertificationResources(t *testing.T) {
	t.Parallel()

	want := []string{
		"signal.unbounded-cloud.io_certificationpolicies.yaml",
		"signal.unbounded-cloud.io_certificationprofiles.yaml",
		"signal.unbounded-cloud.io_certificationruns.yaml",
		"signal.unbounded-cloud.io_certificationsuites.yaml",
		"signal.unbounded-cloud.io_nodecertifications.yaml",
	}

	entries, err := os.ReadDir("crd")
	if err != nil {
		t.Fatalf("read CRD directory: %v", err)
	}

	var got []string

	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".yaml") {
			got = append(got, entry.Name())
		}
	}

	slices.Sort(got)

	if !slices.Equal(got, want) {
		t.Fatalf("generated CRDs = %v, want %v", got, want)
	}

	for _, name := range got {
		content, err := os.ReadFile(filepath.Join("crd", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}

		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.Unmarshal(content, &crd); err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}

		if crd.Spec.PreserveUnknownFields ||
			len(crd.Spec.Versions) != 1 ||
			crd.Spec.Versions[0].Schema == nil ||
			crd.Spec.Versions[0].Schema.OpenAPIV3Schema == nil {
			t.Fatalf("%s is not a structural v1 CRD: %#v", name, crd.Spec)
		}

		switch name {
		case "signal.unbounded-cloud.io_certificationruns.yaml",
			"signal.unbounded-cloud.io_certificationsuites.yaml":
			assertContainsAll(
				t,
				string(content),
				name,
				"minDuration",
				"continuity",
				"maxSampleGap must not exceed backendActiveDeadline",
				"maxSampleGap must not exceed minDuration",
			)
		case "signal.unbounded-cloud.io_nodecertifications.yaml":
			assertContainsAll(
				t,
				string(content),
				name,
				"latestNonPass",
				"latestNonPass must advance by finishedAt and UID",
			)
		}
	}
}

func signalRenderData() map[string]string {
	return map[string]string{
		"Namespace":                  "signal-ns",
		"ScriptNamespace":            "script-ns",
		"ControllerImage":            testControllerImage,
		"AdapterImage":               testAdapterImage,
		"RunnerImage":                testRunnerImage,
		"WorkflowTemplateDigest":     testDigest,
		"QualificationGateProtected": "false",
	}
}

func renderSignal(t *testing.T, data map[string]string) string {
	t.Helper()

	output := t.TempDir()
	if err := render.Render(".", output, data); err != nil {
		t.Fatalf("render Signal manifests: %v", err)
	}

	return output
}

func readRenderedFile(t *testing.T, output, name string) []byte {
	t.Helper()

	content, err := os.ReadFile(filepath.Join(output, name))
	if err != nil {
		t.Fatalf("read rendered %s: %v", name, err)
	}

	return content
}

func splitYAMLDocuments(content string) []string {
	var documents []string

	for _, document := range strings.Split(content, "\n---") {
		if strings.TrimSpace(document) != "" {
			documents = append(documents, document)
		}
	}

	return documents
}

func assertRule(
	t *testing.T,
	documents []string,
	kind string,
	namespace string,
	name string,
	group string,
	resource string,
	verbs []string,
) {
	t.Helper()

	for _, document := range documents {
		var object struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Rules []rbacv1.PolicyRule `json:"rules"`
		}
		if err := yaml.Unmarshal([]byte(document), &object); err != nil {
			t.Fatalf("decode RBAC document: %v", err)
		}

		if object.Kind != kind || object.Metadata.Name != name || object.Metadata.Namespace != namespace {
			continue
		}

		for _, rule := range object.Rules {
			if slices.Equal(rule.APIGroups, []string{group}) &&
				slices.Equal(rule.Resources, []string{resource}) &&
				slices.Equal(rule.Verbs, verbs) {
				return
			}
		}

		t.Fatalf("%s/%s has no %s/%s rule with verbs %v: %#v", kind, name, group, resource, verbs, object.Rules)
	}

	t.Fatalf("%s/%s in namespace %q not found", kind, name, namespace)
}

func assertContainsAll(t *testing.T, content, name string, values ...string) {
	t.Helper()

	for _, value := range values {
		if !strings.Contains(content, value) {
			t.Fatalf("%s does not contain %q", name, value)
		}
	}
}
