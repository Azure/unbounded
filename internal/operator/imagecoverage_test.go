// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The operator resolves every component image as
// <registry>/<repository>:<operator version>, so any pipeline that deploys the
// operator has to build and push all of them at that tag. When one is missing
// the workload stalls in ImagePullBackOff, which is how the gantry image went
// unbuilt in the nightly for fifteen consecutive nights.
//
// These tests hold the pipelines to that set. Add a component and forget its
// image and this fails at review time rather than on a cluster.
//
// The workflows keep their explicit, reviewable matrices; nothing here
// generates them. A new component should show up as a visible diff line.

// componentImages maps each operator component to the image repositories it
// applies to its workloads, as bare repository names. The full reference is
// derived at reconcile time by component.Config.Image.
//
// This is deliberately a table here rather than a method on the component
// interface: the reconciler never needs it, only the release pipelines do, and
// putting it on the interface would make every implementation and every test
// fake carry a value none of them use.
//
// TestComponentImagesCoversRegistry keeps it honest, failing if a component is
// added or removed without a matching edit here, and rejecting an empty list so
// the check cannot be silenced by registering a component with no images.
//
// What it does not cover is an existing component growing an ADDITIONAL image.
// The registry cross-check compares component names, not the lists under them,
// so extending net.go's workload gate with a third repository leaves this table
// stale and every test in this file green. Nothing ties these values back to
// the cfg.Image call sites they mirror. The same is true of a repository name
// that is merely wrong.
//
// Both of those are only caught at deploy time, by the ImagePullBackOff guard
// in hack/release/wait-rollouts.sh, and then only for the workloads that gate
// passes over. That list covers the three cluster components and, on the
// release path, every Site that enables metalman; it still omits storage.
//
// Images pinned to a fixed public reference are not operator-managed and do not
// belong here, such as the busybox init container in gantry's DaemonSet.
var componentImages = map[string][]string{
	"net":             {"unbounded-net-controller", "unbounded-net-node"},
	"machina":         {"machina"},
	"gantry":          {"gantry"},
	"metalman":        {"metalman"},
	"storage":         {"unbounded-storage-supervisor"},
	"token-refresher": {"token-refresher"},
}

// releaseBOMSource is the tool whose hardcoded image list feeds the signed
// release BOM. It legitimately carries images the operator does not manage
// (netboot, orca, the operator itself), so it is checked for containment.
const releaseBOMSource = "hack/cmd/release-bom/main.go"

// releaseBOMVar is the variable in that file holding the image list.
const releaseBOMVar = "releaseImageNames"

// deployWorkflows are the workflows that build images and then deploy the
// operator, so each must cover the full set of component images.
var deployWorkflows = []string{
	".github/workflows/nightly.yaml",
	".github/workflows/release.yaml",
}

// registryTagPattern matches the repository segment of a pushed image tag, for
// example "${{ env.REGISTRY }}/machina:${{ env.TAG }}". Cutting at the first
// colon keeps arch suffixes such as "-${{ matrix.arch }}" out of the capture,
// since they belong to the tag rather than the repository.
var registryTagPattern = regexp.MustCompile(`^\$\{\{\s*env\.REGISTRY\s*\}\}/([^:]+):`)

// matrixComponentPattern matches a repository segment that interpolates the
// component matrix, which is expanded against that job's matrix entries.
var matrixComponentPattern = regexp.MustCompile(`^\$\{\{\s*matrix\.component\.name\s*\}\}$`)

// workflow is the subset of GitHub Actions workflow schema needed to find every
// image a workflow builds and pushes.
type workflow struct {
	Jobs map[string]struct {
		Strategy struct {
			Matrix struct {
				Component []struct {
					Name string `yaml:"name"`
				} `yaml:"component"`
			} `yaml:"matrix"`
		} `yaml:"strategy"`
		Steps []struct {
			Uses string `yaml:"uses"`
			With struct {
				// Push is any because it is a bare YAML bool today but could
				// become an expression string.
				Push    any    `yaml:"push"`
				Outputs string `yaml:"outputs"`
				Tags    string `yaml:"tags"`
			} `yaml:"with"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

// allComponentImages returns every declared repository, sorted and deduplicated.
func allComponentImages() []string {
	seen := map[string]struct{}{}

	for _, repositories := range componentImages {
		for _, repository := range repositories {
			seen[repository] = struct{}{}
		}
	}

	repositories := make([]string, 0, len(seen))
	for repository := range seen {
		repositories = append(repositories, repository)
	}

	sort.Strings(repositories)

	return repositories
}

// repoRoot walks up from this file to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot determine the path of this test file")
	}

	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod found above %s", filepath.Dir(file))
		}

		dir = parent
	}
}

// stepPushes reports whether a build-push-action step actually pushes. Two
// spellings are in use and both must be honored: `push: true`, and an
// `outputs:` entry carrying push=true. The net image steps use only the latter,
// so keying on `push` alone silently misses them.
func stepPushes(push any, outputs string) bool {
	if push != nil && fmt.Sprint(push) == "true" {
		return true
	}

	return strings.Contains(outputs, "push=true")
}

// workflowImages returns every image repository the workflow builds and pushes.
//
// Parsing the YAML tree rather than scanning text is deliberate: the comment
// above nightly's component-images matrix names machine-ops-controller while
// explaining that it is NOT built there, and lists every component that is. A
// textual scan reports both as built.
func workflowImages(t *testing.T, path string) map[string]string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var wf workflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	// repository -> job that builds it, for error messages.
	images := map[string]string{}
	steps := 0

	for jobName, job := range wf.Jobs {
		matrix := make([]string, 0, len(job.Strategy.Matrix.Component))
		for _, component := range job.Strategy.Matrix.Component {
			matrix = append(matrix, component.Name)
		}

		for _, step := range job.Steps {
			if !strings.HasPrefix(step.Uses, "docker/build-push-action") {
				continue
			}

			if !stepPushes(step.With.Push, step.With.Outputs) {
				continue
			}

			steps++

			// tags is a scalar in these workflows but the block form is valid
			// and used elsewhere (ci.yaml), so handle both.
			for _, tag := range strings.Split(step.With.Tags, "\n") {
				tag = strings.TrimSpace(tag)
				if tag == "" {
					continue
				}

				// Local scan-only builds push nothing; they load into the
				// daemon under a fake scan/ registry for Trivy.
				if strings.HasPrefix(tag, "scan/") {
					t.Errorf("%s: job %q pushes a scan/ tag %q, which should be push:false", path, jobName, tag)

					continue
				}

				match := registryTagPattern.FindStringSubmatch(tag)
				if match == nil {
					continue
				}

				repository := match[1]
				if matrixComponentPattern.MatchString(repository) {
					for _, name := range matrix {
						images[name] = jobName
					}

					continue
				}

				images[repository] = jobName
			}
		}
	}

	// Guard against a parser regression silently making these tests vacuous.
	if steps == 0 {
		t.Fatalf("%s: found no pushing build-push-action steps; the parser is broken", path)
	}

	if len(images) == 0 {
		t.Fatalf("%s: extracted no image repositories from %d pushing steps", path, steps)
	}

	for repository := range images {
		if strings.Contains(repository, "${{") {
			t.Fatalf("%s: repository %q was not expanded; the parser is broken", path, repository)
		}
	}

	return images
}

// releaseBOMImages extracts the image list from the release BOM tool. The
// variable is unexported in package main, so it is read from the AST rather
// than imported.
func releaseBOMImages(t *testing.T, path string) map[string]struct{} {
	t.Helper()

	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	images := map[string]struct{}{}

	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}

		for i, name := range spec.Names {
			if name.Name != releaseBOMVar || i >= len(spec.Values) {
				continue
			}

			composite, ok := spec.Values[i].(*ast.CompositeLit)
			if !ok {
				continue
			}

			for _, element := range composite.Elts {
				literal, ok := element.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					continue
				}

				images[strings.Trim(literal.Value, `"`)] = struct{}{}
			}
		}

		return true
	})

	if len(images) == 0 {
		t.Fatalf("%s: found no entries in %s", path, releaseBOMVar)
	}

	return images
}

// TestComponentImagesCoversRegistry keeps componentImages in step with the
// component registry, in both directions, so the table cannot quietly rot as
// components come and go.
func TestComponentImagesCoversRegistry(t *testing.T) {
	registry := DefaultRegistry()

	registered := map[string]struct{}{}
	for _, component := range registry.Cluster {
		registered[component.Name()] = struct{}{}
	}

	for _, component := range registry.Site {
		registered[component.Name()] = struct{}{}
	}

	for name := range registered {
		repositories, ok := componentImages[name]
		if !ok {
			t.Errorf(
				"component %q is registered in DefaultRegistry but has no entry in componentImages.\n"+
					"Add the image repositories it applies to its workloads, otherwise nothing "+
					"checks that the release pipelines build them.",
				name,
			)

			continue
		}

		// An empty list satisfies the key check above while making every other
		// assertion in this file iterate zero times for that component, so it is
		// the cheapest way to turn this test green without fixing anything.
		if len(repositories) == 0 {
			t.Errorf(
				"component %q has an empty entry in componentImages.\n"+
					"That silences the workflow and release BOM checks for it entirely. "+
					"List the image repositories it applies to its workloads.",
				name,
			)
		}
	}

	for name := range componentImages {
		if _, ok := registered[name]; !ok {
			t.Errorf(
				"componentImages lists %q, which is not a registered component.\n"+
					"Remove the entry, or correct it to the component's Name().",
				name,
			)
		}
	}
}

// TestDeployWorkflowsBuildEveryComponentImage is the check that would have
// caught the gantry omission at review time.
func TestDeployWorkflowsBuildEveryComponentImage(t *testing.T) {
	root := repoRoot(t)

	for _, relative := range deployWorkflows {
		t.Run(filepath.Base(relative), func(t *testing.T) {
			built := workflowImages(t, filepath.Join(root, relative))

			for _, component := range sortedComponents() {
				for _, repository := range componentImages[component] {
					if _, ok := built[repository]; !ok {
						t.Errorf(
							"operator component %q manages image repository %q, which %s does not build.\n"+
								"Add it to that workflow's component-images matrix, otherwise the workload "+
								"stalls in ImagePullBackOff on every deploy.",
							component, repository, relative,
						)
					}
				}
			}
		})
	}
}

// TestReleaseBOMCoversEveryComponentImage keeps the signed release BOM's
// hardcoded list in step with the component images. Containment, not equality:
// the BOM also records images the operator does not manage.
func TestReleaseBOMCoversEveryComponentImage(t *testing.T) {
	root := repoRoot(t)
	images := releaseBOMImages(t, filepath.Join(root, releaseBOMSource))

	for _, repository := range allComponentImages() {
		if _, ok := images[repository]; !ok {
			t.Errorf(
				"operator component image %q is missing from %s in %s.\n"+
					"The release BOM would omit its digest, and deploy-time verification "+
					"would not cover it.",
				repository, releaseBOMVar, releaseBOMSource,
			)
		}
	}
}

// sortedComponents keeps failure output stable across runs.
func sortedComponents() []string {
	names := make([]string, 0, len(componentImages))
	for name := range componentImages {
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}
