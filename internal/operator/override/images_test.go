// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package override

import (
	"encoding/json"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func imagesDocument(image string) string {
	return "apiVersion: " + APIVersion + "\nimages:\n  kubeProxy:\n    image: " + image + "\n"
}

// netControllerDeployment is shaped like the net controller Deployment the
// operator renders: the container images.kubeProxy passes its flag to, with
// arguments of its own.
func netControllerDeployment(container string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": "unbounded-net-controller", "namespace": "unbounded-system"},
		"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]any{"app.kubernetes.io/name": "unbounded-net-controller"}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app.kubernetes.io/name": "unbounded-net-controller"}},
				"spec": map[string]any{
					"containers": []any{
						map[string]any{
							"name":  container,
							"image": "ghcr.io/azure/unbounded-net-controller:v1",
							"args":  []any{"--config-file=/etc/unbounded-net/config.yaml"},
						},
					},
				},
			},
		},
	}}
}

// validEntries parses and validates a payload the way the operator does.
func validEntries(t *testing.T, data map[string]string) []SourcedEntry {
	t.Helper()

	entries, err := parseAll(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if err := ValidateErr(entries); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	return entries
}

func controllerArgs(t *testing.T, workload *unstructured.Unstructured) []string {
	t.Helper()

	return podSpec(t, workload).Containers[0].Args
}

func TestParseImagesBecomesAnEntryForTheNetController(t *testing.T) {
	document := `apiVersion: ` + APIVersion + `
images:
  kubeProxy:
    image: myreg.example/kube-proxy
overrides:
  - component: net
    kind: DaemonSet
    extraArgs:
      node: ["--verbose"]
`

	entries := validEntries(t, map[string]string{"kube-proxy.yaml": document})

	if len(entries) != 2 {
		t.Fatalf("entries = %d, want the list entry and the images entry", len(entries))
	}

	if got := entries[0].Source.String(); got != "kube-proxy.yaml[0]" {
		t.Fatalf("first source = %q, want the list entry first", got)
	}

	image := entries[1]
	if got := image.Source.String(); got != "kube-proxy.yaml:images.kubeProxy" {
		t.Fatalf("images source = %q, want kube-proxy.yaml:images.kubeProxy", got)
	}

	if image.Entry.Component != "net" || image.Entry.Kind != "Deployment" || image.Entry.KubeProxyImage != "myreg.example/kube-proxy" {
		t.Fatalf("images entry = %+v, want net/Deployment carrying the image", image.Entry)
	}
}

func TestParseAcceptsADocumentThatOnlySetsImages(t *testing.T) {
	entries := validEntries(t, map[string]string{"kube-proxy.yaml": imagesDocument("myreg.example/kube-proxy:v1.34.2")})

	if len(entries) != 1 || entries[0].Entry.KubeProxyImage != "myreg.example/kube-proxy:v1.34.2" {
		t.Fatalf("entries = %+v, want the images entry alone", entries)
	}
}

// TestParseRejectsUnknownImageFields pins that the images block is decoded as
// strictly as the rest of the document, and that the field it becomes cannot be
// written in an overrides list.
func TestParseRejectsUnknownImageFields(t *testing.T) {
	cases := map[string]string{
		"unknown image": `images:
  pause:
    image: myreg.example/pause:3.9
`,
		"unknown field": `images:
  kubeProxy:
    image: myreg.example/kube-proxy
    tag: v1.34.2
`,
		"internal field in an entry": `overrides:
  - component: net
    kind: Deployment
    kubeProxyImage: myreg.example/kube-proxy
`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			entries, problems, err := Parse(map[string]string{"kube-proxy.yaml": "apiVersion: " + APIVersion + "\n" + body})
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}

			if len(entries) != 0 || len(problems) != 1 || !problems[0].KeyLevel() {
				t.Fatalf("entries = %+v, problems = %+v, want the key rejected", entries, problems)
			}
		})
	}
}

func TestValidateKubeProxyImage(t *testing.T) {
	cases := []struct {
		image string
		want  string
	}{
		{image: "myreg.example/kube-proxy"},
		{image: "localhost:5000/kube-proxy"},
		{image: "myreg.example/kube-proxy:v1.34.2"},
		{image: "myreg.example/kube-proxy@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
		{image: `""`, want: "image is required"},
		{image: "myreg.example/Kube-Proxy", want: "malformed name"},
		{image: "myreg.example/kube-proxy:v1.34.2+k3s1", want: "malformed tag"},
	}

	for _, tc := range cases {
		t.Run(tc.image, func(t *testing.T) {
			entries, err := parseAll(map[string]string{"kube-proxy.yaml": imagesDocument(tc.image)})
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}

			problems := Validate(entries)

			if tc.want == "" {
				if len(problems) != 0 {
					t.Fatalf("problems = %v, want none", problems)
				}

				return
			}

			if len(problems) != 1 || !strings.Contains(problems[0].String(), tc.want) {
				t.Fatalf("problems = %v, want one mentioning %q", problems, tc.want)
			}

			problem := problems[0]
			if problem.KeyLevel() || problem.Component != "net" || problem.Kind != "Deployment" {
				t.Fatalf("problem = %+v, want it scoped to the net controller Deployment", problem)
			}

			if !strings.HasPrefix(problem.String(), "kube-proxy.yaml:images.kubeProxy: ") {
				t.Fatalf("problem = %q, want it to name images.kubeProxy", problem.String())
			}
		})
	}
}

func TestApplyKubeProxyImagePassesTheFlag(t *testing.T) {
	workload := netControllerDeployment("controller")
	plan := planWith(workload, "net", "")

	entries := validEntries(t, map[string]string{"kube-proxy.yaml": imagesDocument("myreg.example/kube-proxy")})

	report := Apply(plan, entries, nil)
	if report.Failed() {
		t.Fatalf("Apply: %v", report.Err())
	}

	got := plan.Operations[0].Object

	want := []string{"--config-file=/etc/unbounded-net/config.yaml", "--managed-kube-proxy-image=myreg.example/kube-proxy"}
	if args := controllerArgs(t, got); strings.Join(args, " ") != strings.Join(want, " ") {
		t.Fatalf("args = %v, want %v", args, want)
	}

	annotations := got.GetAnnotations()
	if annotations[SourceAnnotation] != "kube-proxy.yaml:images.kubeProxy" || annotations[HashAnnotation] == "" {
		t.Fatalf("annotations = %v, want the override recorded on the workload", annotations)
	}

	// The controller's own image is untouched, so nothing drifts from the
	// operator's version.
	if drift, ok := annotations[VersionDriftAnnotation]; ok {
		t.Fatalf("version drift = %q, want none", drift)
	}
}

// TestApplyKubeProxyImageFollowsReplacedArgs pins that the flag survives a patch
// that replaces the argument list, as extraArgs does.
func TestApplyKubeProxyImageFollowsReplacedArgs(t *testing.T) {
	workload := netControllerDeployment("controller")
	plan := planWith(workload, "net", "")

	entries := validEntries(t, map[string]string{
		"args.yaml": doc(`  - component: net
    kind: Deployment
    patch:
      spec:
        template:
          spec:
            containers:
              - name: controller
                args: ["--replaced"]
    extraArgs:
      controller: ["--appended"]
`),
		"kube-proxy.yaml": imagesDocument("myreg.example/kube-proxy"),
	})

	report := Apply(plan, entries, nil)
	if report.Failed() {
		t.Fatalf("Apply: %v", report.Err())
	}

	want := []string{"--replaced", "--appended", "--managed-kube-proxy-image=myreg.example/kube-proxy"}
	if args := controllerArgs(t, plan.Operations[0].Object); strings.Join(args, " ") != strings.Join(want, " ") {
		t.Fatalf("args = %v, want %v", args, want)
	}
}

func TestApplyKubeProxyImageAgreementAcrossKeysPassesTheFlagOnce(t *testing.T) {
	workload := netControllerDeployment("controller")
	plan := planWith(workload, "net", "")

	entries := validEntries(t, map[string]string{
		"a.yaml": imagesDocument("myreg.example/kube-proxy"),
		"b.yaml": imagesDocument("myreg.example/kube-proxy"),
	})

	report := Apply(plan, entries, nil)
	if report.Failed() {
		t.Fatalf("Apply: %v", report.Err())
	}

	want := []string{"--config-file=/etc/unbounded-net/config.yaml", "--managed-kube-proxy-image=myreg.example/kube-proxy"}
	if args := controllerArgs(t, plan.Operations[0].Object); strings.Join(args, " ") != strings.Join(want, " ") {
		t.Fatalf("args = %v, want %v", args, want)
	}
}

func TestApplyKubeProxyImageRejectsDisagreement(t *testing.T) {
	cases := map[string]map[string]string{
		"two images": {
			"a.yaml": imagesDocument("myreg.example/kube-proxy"),
			"b.yaml": imagesDocument("other.example/kube-proxy"),
		},
		"the flag in extraArgs": {
			"a.yaml": doc(`  - component: net
    kind: Deployment
    extraArgs:
      controller: ["--managed-kube-proxy-image=other.example/kube-proxy"]
`),
			"b.yaml": imagesDocument("myreg.example/kube-proxy"),
		},
		"the flag in a replaced argument list": {
			"a.yaml": doc(`  - component: net
    kind: Deployment
    patch:
      spec:
        template:
          spec:
            containers:
              - name: controller
                args: ["--managed-kube-proxy-image", "other.example/kube-proxy"]
`),
			"b.yaml": imagesDocument("myreg.example/kube-proxy"),
		},
	}

	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			plan := planWith(netControllerDeployment("controller"), "net", "")

			report := Apply(plan, validEntries(t, data), nil)
			if !report.Failed() {
				t.Fatal("Apply succeeded, want the disagreement rejected")
			}

			for _, want := range []string{"a.yaml", "b.yaml:images.kubeProxy"} {
				if !strings.Contains(report.Err().Error(), want) {
					t.Fatalf("error = %v, want it to name %s", report.Err(), want)
				}
			}

			if plan.Len() != 0 || len(report.Withheld) != 1 {
				t.Fatalf("plan = %d operations, withheld = %v, want the workload withheld", plan.Len(), report.Withheld)
			}
		})
	}
}

// TestApplyKubeProxyImageAllowsOtherControllerArgs pins that only the image
// flag conflicts: images.kubeProxy and extraArgs on the same container are not
// two contributors appending to it.
func TestApplyKubeProxyImageAllowsOtherControllerArgs(t *testing.T) {
	plan := planWith(netControllerDeployment("controller"), "net", "")

	entries := validEntries(t, map[string]string{
		"a.yaml": doc(`  - component: net
    kind: Deployment
    extraArgs:
      controller: ["--managed-kube-proxy=true"]
`),
		"b.yaml": imagesDocument("myreg.example/kube-proxy"),
	})

	report := Apply(plan, entries, nil)
	if report.Failed() {
		t.Fatalf("Apply: %v", report.Err())
	}

	want := []string{"--config-file=/etc/unbounded-net/config.yaml", "--managed-kube-proxy=true", "--managed-kube-proxy-image=myreg.example/kube-proxy"}
	if args := controllerArgs(t, plan.Operations[0].Object); strings.Join(args, " ") != strings.Join(want, " ") {
		t.Fatalf("args = %v, want %v", args, want)
	}
}

// TestApplyKubeProxyImageRequiresTheControllerContainer covers a release that
// renames the container: the image must be reported as undeliverable rather
// than dropped.
func TestApplyKubeProxyImageRequiresTheControllerContainer(t *testing.T) {
	plan := planWith(netControllerDeployment("renamed"), "net", "")

	report := Apply(plan, validEntries(t, map[string]string{"kube-proxy.yaml": imagesDocument("myreg.example/kube-proxy")}), nil)
	if !report.Failed() || !strings.Contains(report.Err().Error(), `no container "controller"`) {
		t.Fatalf("report = %+v, want the missing container reported", report)
	}

	if plan.Len() != 0 {
		t.Fatal("the workload was left in the plan, want it withheld")
	}
}

func TestApplyKubeProxyImageHashFollowsTheImage(t *testing.T) {
	hashFor := func(image string) string {
		plan := planWith(netControllerDeployment("controller"), "net", "")

		report := Apply(plan, validEntries(t, map[string]string{"kube-proxy.yaml": imagesDocument(image)}), nil)
		if report.Failed() {
			t.Fatalf("Apply: %v", report.Err())
		}

		return report.Workloads[0].Hash
	}

	if hashFor("myreg.example/kube-proxy") == hashFor("other.example/kube-proxy") {
		t.Fatal("two different images hashed the same, so changing the image would not register as a new override")
	}
}

// TestEntryHashEncodingOmitsAnUnsetKubeProxyImage guards every hash already
// stamped on a workload: the contributor hash encodes entries as JSON, so an
// entry that sets no image must encode exactly as it did before the field
// existed.
func TestEntryHashEncodingOmitsAnUnsetKubeProxyImage(t *testing.T) {
	encoded, err := json.Marshal(Entry{Component: "net", Kind: "DaemonSet", ExtraArgs: map[string][]string{"node": {"--verbose"}}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if strings.Contains(string(encoded), "KubeProxyImage") {
		t.Fatalf("entry encodes as %s, want no KubeProxyImage field when unset", encoded)
	}
}
