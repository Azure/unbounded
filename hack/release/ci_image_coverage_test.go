// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package release

import (
	"fmt"
	"os"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// release.yaml only runs on a tag push, so an image that PR CI never builds
// for a platform is first built for that platform during the release. The
// v0.19.0 racer image failed that way on linux/arm64. These tests hold ci.yaml
// to building every (Containerfile, target, platform) that release.yaml builds.

const (
	ciWorkflow      = "../../.github/workflows/ci.yaml"
	releaseWorkflow = "../../.github/workflows/release.yaml"
)

// imageBuild is one docker/build-push-action invocation for one platform.
type imageBuild struct {
	File     string
	Target   string
	Platform string
	// Args holds the build-defining build-args as sorted, comma-separated
	// KEY=VALUE pairs, so builds that select different stages stay distinct.
	Args string
}

func (b imageBuild) String() string {
	s := b.File
	if b.Target != "" {
		s += " target " + b.Target
	}

	if b.Args != "" {
		s += " args " + b.Args
	}

	return fmt.Sprintf("%s (%s)", s, b.Platform)
}

// buildDefiningArgs lists the build-args that change what an image is, as
// opposed to release metadata such as VERSION, GIT_COMMIT, CONTAINER_REGISTRY,
// or GO_VERSION, which differ between ci.yaml and release.yaml by design. This
// is an allowlist rather than a denylist: metadata values reference steps,
// env, and github contexts the test cannot resolve, and a new metadata arg
// must not make every release build look uncovered. A new arg that selects a
// stage or variant belongs here, including feature flags such as
// RACER_NATIVE_CUDA that skip parts of the build (images/racer/Containerfile).
//
// Values are compared literally: an arg set explicitly to its Containerfile
// default is treated as different from omitting it. That can only report a
// false miss, never a false match, and no workflow relies on the equivalence
// today, so the defaults are not parsed out of the Containerfiles.
var buildDefiningArgs = map[string]bool{
	"INSTALLER_KERNEL":  true,
	"RACER_NATIVE_CUDA": true,
	"RACER_NATIVE_RDMA": true,
}

// conditionalArg matches a `${{ matrix.x && format('KEY={0}', matrix.x) || ... }}`
// line whose fallback is the empty string, the idiom ci.yaml uses to emit a
// build-arg only for matrix entries that set it.
var conditionalArg = regexp.MustCompile(
	`^\$\{\{\s*matrix\.([A-Za-z0-9_.-]+)\s*&&\s*format\('([A-Za-z0-9_]+)=\{0\}',\s*matrix\.([A-Za-z0-9_.-]+)\)\s*\|\|\s*''\s*\}\}$`)

// lookupMatrix resolves a dotted matrix path, returning nil when it is unset.
func lookupMatrix(combo map[string]any, path string) any {
	var cur any = combo
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}

		cur = m[part]
	}

	return cur
}

// buildArgs returns the build-defining args of a build-args input, normalized
// as sorted KEY=VALUE pairs joined by commas. Empty values are dropped since
// they leave the Containerfile default in effect.
func buildArgs(value string, combo map[string]any) (string, error) {
	var args []string

	for _, line := range strings.Split(value, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if m := conditionalArg.FindStringSubmatch(line); m != nil {
			if m[1] != m[3] {
				return "", fmt.Errorf("unsupported conditional build-arg %q", line)
			}

			v := lookupMatrix(combo, m[1])
			if v != nil && v != false && fmt.Sprint(v) != "" && buildDefiningArgs[m[2]] {
				args = append(args, m[2]+"="+fmt.Sprint(v))
			}

			continue
		}

		key, raw, ok := strings.Cut(line, "=")
		if !ok {
			return "", fmt.Errorf("build-arg %q is not KEY=VALUE", line)
		}

		if !buildDefiningArgs[key] {
			continue
		}

		v, err := expand(raw, combo)
		if err != nil {
			return "", fmt.Errorf("build-arg %s: %w", key, err)
		}

		if v != "" {
			args = append(args, key+"="+v)
		}
	}

	sort.Strings(args)

	return strings.Join(args, ","), nil
}

type imageWorkflow struct {
	Jobs map[string]imageJob `yaml:"jobs"`
}

type imageJob struct {
	RunsOn   any `yaml:"runs-on"`
	Strategy struct {
		Matrix map[string]any `yaml:"matrix"`
	} `yaml:"strategy"`
	Steps []struct {
		Uses string            `yaml:"uses"`
		With map[string]string `yaml:"with"`
	} `yaml:"steps"`
}

var matrixExpr = regexp.MustCompile(`\$\{\{\s*matrix\.([A-Za-z0-9_.-]+)\s*\}\}`)

// matrixCombinations expands a strategy matrix into its job combinations. It
// implements the subset of GitHub's rules the workflows use: the cartesian
// product of the dimensions, minus partial-match excludes, plus includes. An
// include extends every original combination whose dimension values it agrees
// with, and is a combination of its own when it agrees with none.
func matrixCombinations(matrix map[string]any) ([]map[string]any, error) {
	var keys []string

	for key := range matrix {
		if key != "include" && key != "exclude" {
			keys = append(keys, key)
		}
	}

	sort.Strings(keys)

	combos := []map[string]any{{}}
	if len(keys) == 0 {
		combos = nil
	}

	for _, key := range keys {
		values, ok := matrix[key].([]any)
		if !ok {
			return nil, fmt.Errorf("matrix dimension %q is %T, want a list", key, matrix[key])
		}

		var next []map[string]any

		for _, combo := range combos {
			for _, value := range values {
				c := make(map[string]any, len(combo)+1)
				for k, v := range combo {
					c[k] = v
				}

				c[key] = value
				next = append(next, c)
			}
		}

		combos = next
	}

	excludes, err := matrixEntries(matrix, "exclude")
	if err != nil {
		return nil, err
	}

	combos = slices.DeleteFunc(combos, func(combo map[string]any) bool {
		return slices.ContainsFunc(excludes, func(exclude map[string]any) bool {
			return partialMatch(exclude, combo)
		})
	})

	includes, err := matrixEntries(matrix, "include")
	if err != nil {
		return nil, err
	}

	originals := len(combos)

	for _, include := range includes {
		// With no dimensions, every include is a combination of its own.
		if len(keys) == 0 {
			combos = append(combos, include)
			continue
		}

		// Otherwise an include extends each original combination whose
		// dimension values it agrees with, and becomes a new combination when
		// it agrees with none.
		matched := false

		for _, combo := range combos[:originals] {
			agrees := true

			for _, key := range keys {
				if value, ok := include[key]; ok && !reflect.DeepEqual(value, combo[key]) {
					agrees = false
					break
				}
			}

			if agrees {
				matched = true

				for k, v := range include {
					combo[k] = v
				}
			}
		}

		if !matched {
			combos = append(combos, include)
		}
	}

	// A job without a matrix runs once. A matrix whose excludes remove every
	// combination runs zero times, so it must not fall back to one.
	if len(matrix) == 0 {
		combos = []map[string]any{{}}
	}

	return combos, nil
}

func matrixEntries(matrix map[string]any, key string) ([]map[string]any, error) {
	raw, ok := matrix[key]
	if !ok {
		return nil, nil
	}

	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("matrix %s is %T, want a list", key, raw)
	}

	entries := make([]map[string]any, 0, len(list))

	for _, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("matrix %s entry is %T, want a mapping", key, item)
		}

		entries = append(entries, entry)
	}

	return entries, nil
}

// partialMatch reports whether every value in want, recursively, equals the
// value at the same path in got.
func partialMatch(want, got map[string]any) bool {
	for key, w := range want {
		g, ok := got[key]
		if !ok {
			return false
		}

		wm, wok := w.(map[string]any)
		gm, gok := g.(map[string]any)

		switch {
		case wok && gok:
			if !partialMatch(wm, gm) {
				return false
			}
		case wok || gok:
			return false
		case fmt.Sprint(w) != fmt.Sprint(g):
			return false
		}
	}

	return true
}

// expand substitutes `${{ matrix.a.b }}` references from combo. Any other
// expression, or a matrix path that does not resolve, is an error so that a
// new indirection fails the test instead of being silently skipped.
func expand(value string, combo map[string]any) (string, error) {
	var err error

	out := matrixExpr.ReplaceAllStringFunc(value, func(expr string) string {
		path := strings.Split(matrixExpr.FindStringSubmatch(expr)[1], ".")

		var cur any = combo
		for _, part := range path {
			m, ok := cur.(map[string]any)
			if !ok {
				cur = nil
				break
			}

			cur = m[part]
		}

		if cur == nil {
			err = fmt.Errorf("%s does not resolve in matrix %v", expr, combo)
			return ""
		}

		return fmt.Sprint(cur)
	})
	if err != nil {
		return "", err
	}

	if strings.Contains(out, "${{") {
		return "", fmt.Errorf("unsupported expression in %q", value)
	}

	return out, nil
}

// runnerPlatform is the platform docker builds for when a step names none: the
// runner's own.
func runnerPlatform(runsOn string) string {
	if strings.Contains(runsOn, "arm") {
		return "linux/arm64"
	}

	return "linux/amd64"
}

// imageBuilds returns every image build in a workflow, one per platform.
func imageBuilds(data []byte) (map[imageBuild][]string, error) {
	var wf imageWorkflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		return nil, err
	}

	builds := map[imageBuild][]string{}

	for name, job := range wf.Jobs {
		combos, err := matrixCombinations(job.Strategy.Matrix)
		if err != nil {
			return nil, fmt.Errorf("job %s: %w", name, err)
		}

		for _, combo := range combos {
			for _, step := range job.Steps {
				if !strings.HasPrefix(step.Uses, "docker/build-push-action@") {
					continue
				}

				file, err := expand(step.With["file"], combo)
				if err != nil {
					return nil, fmt.Errorf("job %s file: %w", name, err)
				}

				if file == "" {
					return nil, fmt.Errorf("job %s: build step without a file", name)
				}

				target, err := expand(step.With["target"], combo)
				if err != nil {
					return nil, fmt.Errorf("job %s target: %w", name, err)
				}

				platforms, err := expand(step.With["platforms"], combo)
				if err != nil {
					return nil, fmt.Errorf("job %s platforms: %w", name, err)
				}

				if platforms == "" {
					runsOn, err := expand(fmt.Sprint(job.RunsOn), combo)
					if err != nil {
						return nil, fmt.Errorf("job %s: build step without platforms on runner: %w", name, err)
					}

					platforms = runnerPlatform(runsOn)
				}

				args, err := buildArgs(step.With["build-args"], combo)
				if err != nil {
					return nil, fmt.Errorf("job %s build-args: %w", name, err)
				}

				for _, platform := range strings.Split(platforms, ",") {
					build := imageBuild{File: file, Target: target, Platform: strings.TrimSpace(platform), Args: args}
					if !slices.Contains(builds[build], name) {
						builds[build] = append(builds[build], name)
					}
				}
			}
		}
	}

	return builds, nil
}

// missingBuilds returns the builds in release that ci does not run, sorted.
func missingBuilds(release, ci map[imageBuild][]string) []string {
	var missing []string

	for build, jobs := range release {
		if _, ok := ci[build]; !ok {
			missing = append(missing, fmt.Sprintf("%s, built by release job %s", build, strings.Join(jobs, ", ")))
		}
	}

	sort.Strings(missing)

	return missing
}

func readImageBuilds(t *testing.T, path string) map[imageBuild][]string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	builds, err := imageBuilds(data)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}

	return builds
}

func TestCIBuildsEveryReleaseImage(t *testing.T) {
	t.Parallel()

	release := readImageBuilds(t, releaseWorkflow)
	ci := readImageBuilds(t, ciWorkflow)

	// Guard against the parser quietly finding nothing, which would make the
	// coverage check vacuous.
	for _, want := range []imageBuild{
		{File: "images/racer/Containerfile", Platform: "linux/arm64"},
		{File: "images/net/Containerfile", Target: "node", Platform: "linux/arm64"},
		{File: "images/racer-vllm/Containerfile", Platform: "linux/amd64"},
		{File: "images/netboot/Containerfile", Platform: "linux/arm64", Args: "INSTALLER_KERNEL=ubuntu"},
		{File: "images/netboot/Containerfile", Platform: "linux/arm64", Args: "INSTALLER_KERNEL=azurelinux"},
	} {
		if _, ok := release[want]; !ok {
			t.Errorf("release.yaml no longer builds %s; update this test", want)
		}
	}

	if missing := missingBuilds(release, ci); len(missing) > 0 {
		t.Errorf("ci.yaml does not build these release images; add them to its release-images job:\n  %s",
			strings.Join(missing, "\n  "))
	}
}

func TestMissingBuildsRacerFeatureFlags(t *testing.T) {
	t.Parallel()

	release, err := imageBuilds([]byte(`
jobs:
  racer:
    runs-on: ubuntu-latest
    steps:
      - uses: docker/build-push-action@v7
        with:
          file: images/racer/Containerfile
          platforms: linux/arm64
          build-args: |
            VERSION=${{ github.ref_name }}
`))
	if err != nil {
		t.Fatal(err)
	}

	ci, err := imageBuilds([]byte(`
jobs:
  racer:
    runs-on: ubuntu-latest
    steps:
      - uses: docker/build-push-action@v7
        with:
          file: images/racer/Containerfile
          platforms: linux/arm64
          build-args: |
            RACER_NATIVE_CUDA=false
`))
	if err != nil {
		t.Fatal(err)
	}

	missing := missingBuilds(release, ci)
	if len(missing) != 1 || !strings.Contains(missing[0], "images/racer/Containerfile (linux/arm64)") {
		t.Errorf("missingBuilds = %q, want the default racer build reported missing", missing)
	}

	ciRDMA, err := imageBuilds([]byte(`
jobs:
  racer:
    runs-on: ubuntu-latest
    steps:
      - uses: docker/build-push-action@v7
        with:
          file: images/racer/Containerfile
          platforms: linux/arm64
          build-args: |
            RACER_NATIVE_RDMA=false
`))
	if err != nil {
		t.Fatal(err)
	}

	if missing := missingBuilds(release, ciRDMA); len(missing) != 1 {
		t.Errorf("missingBuilds with RDMA disabled = %q, want one miss", missing)
	}
}

func TestImageBuildsExpandsMatrix(t *testing.T) {
	t.Parallel()

	builds, err := imageBuilds([]byte(`
jobs:
  images:
    runs-on: ${{ matrix.arch == 'arm64' && 'ubuntu-24.04-arm' || 'ubuntu-latest' }}
    strategy:
      matrix:
        arch: [amd64, arm64]
        component:
          - {name: a, file: images/a/Containerfile}
          - {name: b, file: images/b/Containerfile}
        exclude:
          - arch: arm64
            component: {name: b}
        include:
          - arch: amd64
            component: {name: c, file: images/c/Containerfile}
    steps:
      - uses: actions/checkout@v1
      - uses: docker/build-push-action@v7
        with:
          file: ${{ matrix.component.file }}
          platforms: linux/${{ matrix.arch }}
  multi:
    runs-on: ubuntu-latest
    steps:
      - uses: docker/build-push-action@v7
        with:
          file: images/d/Containerfile
          target: node
          platforms: linux/amd64,linux/arm64
      - uses: docker/build-push-action@v7
        with:
          file: images/e/Containerfile
`))
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	for build := range builds {
		got = append(got, build.String())
	}

	sort.Strings(got)

	want := []string{
		"images/a/Containerfile (linux/amd64)",
		"images/a/Containerfile (linux/arm64)",
		"images/b/Containerfile (linux/amd64)",
		"images/c/Containerfile (linux/amd64)",
		"images/d/Containerfile target node (linux/amd64)",
		"images/d/Containerfile target node (linux/arm64)",
		"images/e/Containerfile (linux/amd64)",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("builds =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func TestImageBuildsIncludeExtendsEveryCombination(t *testing.T) {
	t.Parallel()

	for name, matrix := range map[string]string{
		"include only": `
        include:
          - {arch: amd64, runner: ubuntu-latest}
          - {arch: arm64, runner: ubuntu-24.04-arm}`,
		"include extends a dimension": `
        arch: [amd64, arm64]
        include:
          - {arch: amd64, runner: ubuntu-latest}
          - {arch: arm64, runner: ubuntu-24.04-arm}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			builds, err := imageBuilds([]byte(`
jobs:
  net:
    runs-on: ${{ matrix.runner }}
    strategy:
      matrix:` + matrix + `
    steps:
      - uses: docker/build-push-action@v7
        with:
          file: images/net/Containerfile
`))
			if err != nil {
				t.Fatal(err)
			}

			if len(builds) != 2 {
				t.Errorf("got %d builds, want 2: %v", len(builds), builds)
			}

			for _, platform := range []string{"linux/amd64", "linux/arm64"} {
				if _, ok := builds[imageBuild{File: "images/net/Containerfile", Platform: platform}]; !ok {
					t.Errorf("missing images/net/Containerfile for %s in %v", platform, builds)
				}
			}
		})
	}
}

func TestImageBuildsMatrixFallback(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		strategy string
		want     int
	}{
		"no matrix": {want: 1},
		"fully excluded matrix": {
			strategy: `
    strategy:
      matrix:
        arch: [amd64, arm64]
        exclude:
          - arch: amd64
          - arch: arm64`,
			want: 0,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			builds, err := imageBuilds([]byte(`
jobs:
  net:
    runs-on: ubuntu-latest` + tc.strategy + `
    steps:
      - uses: docker/build-push-action@v7
        with:
          file: images/net/Containerfile
          platforms: linux/amd64
`))
			if err != nil {
				t.Fatal(err)
			}

			if len(builds) != tc.want {
				t.Errorf("got %d builds, want %d: %v", len(builds), tc.want, builds)
			}
		})
	}
}

func TestImageBuildsRejectsUnresolvedExpressions(t *testing.T) {
	t.Parallel()

	for name, workflow := range map[string]string{
		"unknown matrix key": `
jobs:
  j:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        component: [{name: a}]
    steps:
      - uses: docker/build-push-action@v7
        with:
          file: ${{ matrix.component.file }}
`,
		"non-matrix expression": `
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - uses: docker/build-push-action@v7
        with:
          file: ${{ env.FILE }}
`,
		"missing file": `
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - uses: docker/build-push-action@v7
        with:
          platforms: linux/amd64
`,
		"scalar matrix dimension": `
jobs:
  j:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        arch: amd64
    steps:
      - uses: docker/build-push-action@v7
        with:
          file: images/a/Containerfile
`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, err := imageBuilds([]byte(workflow)); err == nil {
				t.Fatal("imageBuilds succeeded, want an error")
			}
		})
	}
}

func TestMissingBuildsReportsPlatformGaps(t *testing.T) {
	t.Parallel()

	release := map[imageBuild][]string{
		{File: "images/a/Containerfile", Platform: "linux/amd64"}: {"component-images"},
		{File: "images/a/Containerfile", Platform: "linux/arm64"}: {"component-images"},
	}
	ci := map[imageBuild][]string{
		{File: "images/a/Containerfile", Platform: "linux/amd64"}: {"release-images"},
		{File: "images/b/Containerfile", Platform: "linux/amd64"}: {"release-images"},
	}

	got := missingBuilds(release, ci)
	want := []string{"images/a/Containerfile (linux/arm64), built by release job component-images"}

	if !slices.Equal(got, want) {
		t.Fatalf("missingBuilds = %q, want %q", got, want)
	}

	if got := missingBuilds(ci, ci); len(got) != 0 {
		t.Fatalf("missingBuilds of a workflow against itself = %q, want none", got)
	}
}

func TestMissingBuildsReportsBuildArgGaps(t *testing.T) {
	t.Parallel()

	release, err := imageBuilds([]byte(`
jobs:
  netboot:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        component:
          - {name: netboot, kernel: ubuntu}
          - {name: netboot-azurelinux, kernel: azurelinux}
    steps:
      - uses: docker/build-push-action@v7
        with:
          file: images/netboot/Containerfile
          platforms: linux/amd64
          build-args: |
            VERSION=${{ needs.version.outputs.tag }}
            GIT_COMMIT=${{ github.sha }}
            INSTALLER_KERNEL=${{ matrix.component.kernel }}
`))
	if err != nil {
		t.Fatal(err)
	}

	ci, err := imageBuilds([]byte(`
jobs:
  release-images:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        component:
          - {name: netboot, kernel: ubuntu}
          - {name: metalman}
    steps:
      - uses: docker/build-push-action@v7
        with:
          file: images/netboot/Containerfile
          platforms: linux/amd64
          build-args: |
            VERSION=v0.0.0-ci
            ${{ matrix.component.kernel && format('INSTALLER_KERNEL={0}', matrix.component.kernel) || '' }}
`))
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := ci[imageBuild{File: "images/netboot/Containerfile", Platform: "linux/amd64"}]; !ok {
		t.Errorf("conditional build-arg without a kernel should produce no args: %v", ci)
	}

	got := missingBuilds(release, ci)
	want := []string{
		"images/netboot/Containerfile args INSTALLER_KERNEL=azurelinux (linux/amd64), built by release job netboot",
	}

	if !slices.Equal(got, want) {
		t.Fatalf("missingBuilds = %q, want %q", got, want)
	}
}
