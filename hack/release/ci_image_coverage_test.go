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
}

func (b imageBuild) String() string {
	if b.Target == "" {
		return fmt.Sprintf("%s (%s)", b.File, b.Platform)
	}

	return fmt.Sprintf("%s target %s (%s)", b.File, b.Target, b.Platform)
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

				for _, platform := range strings.Split(platforms, ",") {
					build := imageBuild{File: file, Target: target, Platform: strings.TrimSpace(platform)}
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
