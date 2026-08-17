// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package release contains tests for the shell tooling in hack/release/.
//
// wait-rollouts.sh gates the deploy jobs in nightly.yaml and
// release-upgrade.yaml. It cannot be exercised by those workflows before it
// merges, and a mistake in it either fails a good deploy or lets a broken one
// through, so its control flow is covered here instead.
//
// The tests drive the real script with a fake kubectl on PATH, following the
// pattern in hack/cmd/gantry-benchmark/enable_test.go.
package release

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	// releaseTag is the tag the deploy under test is rolling out. The operator
	// resolves every component image as <registry>/<repository>:<its own
	// compiled version>, so the release tag and the image tag are the same
	// string, which is what lets wait-rollouts.sh tell the release being
	// deployed from the one it replaces.
	releaseTag = "nightly-abc"

	// previousTag is the tag the cluster was on before. A workload still
	// carrying it has not been updated by the operator yet.
	previousTag = "nightly-old"

	// gantryImage is the image the workload under test wants.
	gantryImage = "ghcr.io/azure/gantry:" + releaseTag

	// staleImage is an image a previous revision wanted. Pods still running it
	// belong to a superseded revision and must not gate the current rollout.
	staleImage = "ghcr.io/azure/gantry:" + previousTag

	// initImage is the pinned third-party init image the gantry DaemonSet
	// pulls. A blip on a public registry must not fail a deploy on its own.
	initImage = "mcr.microsoft.com/cbl-mariner/busybox:2.0"

	selectorLabel = "app.kubernetes.io/name=gantry"

	target = "ds/gantry"
)

// container describes one container in a fake pod.
type container struct {
	name    string
	image   string
	waiting string
	message string
	init    bool
}

// pod describes one fake pod.
type pod struct {
	name        string
	terminating bool
	containers  []container
	noStatus    bool

	// node is .spec.nodeName. Empty means unscheduled, which the tolerance
	// filter counts as a real scheduling failure rather than a stranded pod.
	node string
	// notReady sets phase Running with Ready=False, the shape of a pod on a
	// node the kubelet has stopped reporting for.
	notReady bool
}

// node describes one fake cluster node.
type node struct {
	name string
	site string
	// ready is the value of the Ready condition's status. Empty means the
	// condition is absent entirely. An unreachable kubelet reports "Unknown",
	// not "False", which is the case tolerance exists for.
	ready string
}

// dsStatus is the DaemonSet status tolerance is decided on.
type dsStatus struct {
	desired    int
	ready      int
	updated    int
	generation int
	observed   int
}

// fake stubs kubectl for one script invocation. Responses are keyed by the
// kind of call the script makes, and may vary by call number so a scenario can
// change state underneath a poll loop.
type fake struct {
	t   *testing.T
	dir string
}

// reply is one canned kubectl response.
type reply struct {
	stdout string
	stderr string
	exit   int
	// sleep holds the process open, so the poll loop in wait_rollout gets a
	// chance to run while "kubectl rollout status" is still in flight.
	sleep string
}

// kubectlStub dispatches on the shape of the call rather than matching exact
// argument lists, so the script's flag ordering can change without breaking
// every test. Keys are:
//
//	rollout           - kubectl rollout status ...
//	pods              - kubectl get pods --selector ... -o json
//	getjson-<target>  - kubectl get <target> -o json
//	get-<target>      - kubectl get <target>
//
// A file named <key>.<n>.<field> overrides <key>.<field> on the nth call.
const kubectlStub = `#!/usr/bin/env bash
set -u

# A real kubectl is a single process, so killing it stops the work. Reproduce
# that here: without the trap the sleep would outlive its parent and keep the
# test's output pipe open, hiding whether the script's own kill worked.
interruptible_sleep() {
  sleep "$1" &
  local child=$!

  trap 'kill "'"$child"'" 2>/dev/null; exit 143' TERM INT
  wait "$child" 2>/dev/null || true
  trap - TERM INT
}

printf '%s\n' "$*" >> "${FIXTURE_DIR}/calls.log"

joined="$*"
args=("$@")

if [[ "$joined" == *"rollout status"* ]]; then
  key="rollout"
elif [[ "$joined" == *"get pods"* ]]; then
  key="pods"
else
  target=""
  for (( i = 0; i < $#; i++ )); do
    if [[ "${args[i]}" == "get" ]]; then
      target="${args[i+1]}"
      break
    fi
  done
  target="${target//\//_}"
  if [[ "$joined" == *"-o json"* ]]; then
    key="getjson-${target}"
  else
    key="get-${target}"
  fi
fi

countfile="${FIXTURE_DIR}/.count.${key}"
count=$(( $(cat "$countfile" 2>/dev/null || echo 0) + 1 ))
printf '%s' "$count" > "$countfile"

prefix="${FIXTURE_DIR}/${key}"
for field in out err exit sleep; do
  if [[ -f "${prefix}.${count}.${field}" ]]; then
    prefix="${prefix}.${count}"
    break
  fi
done

[[ -f "${prefix}.sleep" ]] && interruptible_sleep "$(cat "${prefix}.sleep")"
[[ -f "${prefix}.err" ]] && cat "${prefix}.err" >&2
[[ -f "${prefix}.out" ]] && cat "${prefix}.out"

if [[ -f "${prefix}.exit" ]]; then
  exit "$(cat "${prefix}.exit")"
fi

exit 0
`

// newFake installs a fake kubectl into a temporary bin directory.
func newFake(t *testing.T) *fake {
	t.Helper()

	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")

	if err := os.MkdirAll(binDir, 0o750); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	stub := filepath.Join(binDir, "kubectl")
	if err := os.WriteFile(stub, []byte(kubectlStub), 0o700); err != nil {
		t.Fatalf("write kubectl stub: %v", err)
	}

	return &fake{t: t, dir: dir}
}

// binDir is the directory prepended to PATH.
func (f *fake) binDir() string { return filepath.Join(f.dir, "bin") }

// set records the default response for a kind of call.
func (f *fake) set(key string, r reply) {
	f.t.Helper()
	f.write(key, r)
}

// setNth records the response for the nth call of a kind.
func (f *fake) setNth(key string, n int, r reply) {
	f.t.Helper()
	f.write(key+"."+strconv.Itoa(n), r)
}

func (f *fake) write(prefix string, r reply) {
	f.t.Helper()

	for field, value := range map[string]string{
		"out":   r.stdout,
		"err":   r.stderr,
		"sleep": r.sleep,
	} {
		if value == "" {
			continue
		}

		f.writeFile(prefix+"."+field, value)
	}

	if r.exit != 0 {
		f.writeFile(prefix+".exit", strconv.Itoa(r.exit))
	}
}

func (f *fake) writeFile(name, content string) {
	f.t.Helper()

	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(content), 0o600); err != nil {
		f.t.Fatalf("write fixture %s: %v", name, err)
	}
}

// calls returns every kubectl invocation the script made.
func (f *fake) calls() string {
	f.t.Helper()

	data, err := os.ReadFile(filepath.Join(f.dir, "calls.log"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ""
		}

		f.t.Fatalf("read calls.log: %v", err)
	}

	return string(data)
}

// workload renders a workload with the given selector and container images.
//
// It deliberately emits NO kind, so node_tolerance stops at its kind check.
// The image-guard tests below are about the image guard; a fixture that opted
// them all into the tolerance path as well would make an unrelated change to
// tolerance able to break every one of them.
func workload(selector string, images ...string) string {
	return renderWorkload("", selector, images, nil, nil)
}

// workloadWithInit renders a workload that also declares an init container,
// matching the shape of deploy/gantry/daemonset.yaml.tmpl.
func workloadWithInit(selector, image, init string) string {
	return renderWorkload("", selector, []string{image}, []string{init}, nil)
}

// daemonSet renders a DaemonSet complete with the kind and status fields
// node_tolerance reads, which is what a real `kubectl get ds/x -o json`
// returns.
func daemonSet(selector string, status dsStatus, images ...string) string {
	return renderWorkload("DaemonSet", selector, images, nil, &status)
}

// deployment renders a Deployment carrying a DaemonSet-shaped status. A
// Deployment reschedules off a dead node, so its shortfall must never be
// excused however good the rest of the evidence looks.
func deployment(selector string, status dsStatus, images ...string) string {
	return renderWorkload("Deployment", selector, images, nil, &status)
}

func renderWorkload(kind, selector string, images, initImages []string, status *dsStatus) string {
	labels := map[string]string{}

	if selector != "" {
		key, value, _ := strings.Cut(selector, "=")
		labels[key] = value
	}

	build := func(names []string, prefix string) []map[string]string {
		out := make([]map[string]string, 0, len(names))
		for i, image := range names {
			out = append(out, map[string]string{
				"name":  prefix + strconv.Itoa(i),
				"image": image,
			})
		}

		return out
	}

	spec := map[string]any{
		"spec": map[string]any{
			"selector": map[string]any{"matchLabels": labels},
			"template": map[string]any{
				"spec": map[string]any{
					"containers":     build(images, "c"),
					"initContainers": build(initImages, "i"),
				},
			},
		},
	}

	if kind != "" {
		spec["kind"] = kind
	}

	if status != nil {
		spec["metadata"] = map[string]any{"generation": status.generation}
		spec["status"] = map[string]any{
			"desiredNumberScheduled": status.desired,
			"numberReady":            status.ready,
			"updatedNumberScheduled": status.updated,
			"observedGeneration":     status.observed,
		}
	}

	return marshal(spec)
}

// nodeList renders a node list in the shape kubectl returns.
func nodeList(nodes ...node) string {
	items := make([]map[string]any, 0, len(nodes))

	for _, n := range nodes {
		labels := map[string]any{}
		if n.site != "" {
			labels["unbounded-cloud.io/site"] = n.site
		}

		status := map[string]any{}
		if n.ready != "" {
			status["conditions"] = []map[string]any{
				{"type": "MemoryPressure", "status": "False"},
				{"type": "Ready", "status": n.ready},
			}
		}

		items = append(items, map[string]any{
			"metadata": map[string]any{"name": n.name, "labels": labels},
			"status":   status,
		})
	}

	return marshal(map[string]any{"items": items})
}

// podList renders a pod list in the shape kubectl returns.
func podList(pods ...pod) string {
	items := make([]map[string]any, 0, len(pods))

	for _, p := range pods {
		meta := map[string]any{"name": p.name}
		if p.terminating {
			meta["deletionTimestamp"] = "2026-08-13T00:00:00Z"
		}

		specContainers := []map[string]any{}
		specInit := []map[string]any{}
		statuses := []map[string]any{}
		initStatuses := []map[string]any{}

		for _, c := range p.containers {
			entry := map[string]any{"name": c.name, "image": c.image}

			state := map[string]any{"running": map[string]any{}}

			if c.waiting != "" {
				waiting := map[string]any{"reason": c.waiting}
				if c.message != "" {
					waiting["message"] = c.message
				}

				state = map[string]any{"waiting": waiting}
			}

			status := map[string]any{"name": c.name, "image": c.image, "state": state}

			if c.init {
				specInit = append(specInit, entry)
				initStatuses = append(initStatuses, status)
			} else {
				specContainers = append(specContainers, entry)
				statuses = append(statuses, status)
			}
		}

		podSpec := map[string]any{"containers": specContainers, "initContainers": specInit}
		if p.node != "" {
			podSpec["nodeName"] = p.node
		}

		item := map[string]any{
			"metadata": meta,
			"spec":     podSpec,
		}

		// Running+Ready unless the fixture says otherwise. Both are read by the
		// tolerance filter; the image guard ignores them.
		readiness := map[string]any{
			"phase": "Running",
			"conditions": []map[string]any{
				{"type": "Ready", "status": "True"},
			},
		}

		if p.notReady {
			readiness["conditions"] = []map[string]any{
				{"type": "Ready", "status": "False"},
			}
		}

		if p.noStatus {
			item["status"] = map[string]any{"phase": "Pending"}
		} else {
			item["status"] = map[string]any{
				"phase":                 readiness["phase"],
				"conditions":            readiness["conditions"],
				"containerStatuses":     statuses,
				"initContainerStatuses": initStatuses,
			}
		}

		items = append(items, item)
	}

	return marshal(map[string]any{"items": items})
}

func marshal(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}

	return string(data)
}

// run executes the real script against the fake kubectl.
func (f *fake) run(env map[string]string, args ...string) (string, int) {
	f.t.Helper()

	script, err := filepath.Abs("wait-rollouts.sh")
	if err != nil {
		f.t.Fatalf("resolve script: %v", err)
	}

	kubeconfig := filepath.Join(f.dir, "kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte("fake"), 0o600); err != nil {
		f.t.Fatalf("write kubeconfig: %v", err)
	}

	base := map[string]string{
		"PATH":                        f.binDir() + string(os.PathListSeparator) + os.Getenv("PATH"),
		"FIXTURE_DIR":                 f.dir,
		"KUBECONFIG":                  kubeconfig,
		"TMPDIR":                      f.dir,
		"NAMESPACE":                   "unbounded-system",
		"POLL_INTERVAL_SECONDS":       "1",
		"IMAGE_FAILURE_GRACE_SECONDS": "2",
		"CREATE_TIMEOUT_SECONDS":      "5",
		"ROLLOUT_TIMEOUT":             "30s",
	}
	for key, value := range env {
		base[key] = value
	}

	environ := make([]string, 0, len(base))
	for key, value := range base {
		environ = append(environ, key+"="+value)
	}

	command := exec.Command("bash", append([]string{script}, args...)...)
	command.Env = environ

	output, err := command.CombinedOutput()

	code := 0

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else if err != nil {
		f.t.Fatalf("run script: %v", err)
	}

	return string(output), code
}

// requireBash4 skips when the host bash is too old to run the scripts in this
// directory. Both of them use bash 4 features (associative arrays, mapfile).
func requireBash4(t *testing.T) {
	t.Helper()

	path, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not on PATH; skipping shell script tests")
	}

	out, err := exec.Command(path, "-c", "echo ${BASH_VERSINFO[0]}").Output()
	if err != nil {
		t.Skipf("cannot determine bash version: %v", err)
	}

	major, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || major < 4 {
		t.Skipf("these scripts require bash 4+; found %q", strings.TrimSpace(string(out)))
	}
}

// requireGit skips when git is unavailable, which the resolver fixtures need.
func requireGit(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; skipping next-version.sh tests")
	}
}

// requireBash skips when the host cannot run wait-rollouts.sh. jq is checked
// here rather than in requireBash4 because only this script needs it.
func requireBash(t *testing.T) {
	t.Helper()

	requireBash4(t)

	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH; skipping wait-rollouts.sh tests")
	}
}

func requireContains(t *testing.T, output, want string) {
	t.Helper()

	if !strings.Contains(output, want) {
		t.Errorf("expected output to contain %q\n--- output ---\n%s", want, output)
	}
}

func requireNotContains(t *testing.T, output, want string) {
	t.Helper()

	if strings.Contains(output, want) {
		t.Errorf("expected output NOT to contain %q\n--- output ---\n%s", want, output)
	}
}

func requireCode(t *testing.T, got, want int, output string) {
	t.Helper()

	if got != want {
		t.Errorf("expected exit code %d, got %d\n--- output ---\n%s", want, got, output)
	}
}

// requireGroupsBalanced guards the log-group handling. An unbalanced group
// renders the failing target collapsed in the Actions UI, which is how the
// original failure stayed unnoticed for so long.
func requireGroupsBalanced(t *testing.T, output string) {
	t.Helper()

	open := strings.Count(output, "::group::")
	closed := strings.Count(output, "::endgroup::")

	if open != closed {
		t.Errorf("unbalanced log groups: %d open, %d closed\n--- output ---\n%s", open, closed, output)
	}
}

func TestSucceedsWhenEveryWorkloadRollsOut(t *testing.T) {
	requireBash(t)
	t.Parallel()

	f := newFake(t)
	f.set("getjson-ds_gantry", reply{stdout: workload(selectorLabel, gantryImage)})
	f.set("getjson-deploy_machina-controller", reply{stdout: workload("app=machina", "ghcr.io/azure/machina:x")})
	f.set("pods", reply{stdout: podList(
		pod{name: "gantry-a", containers: []container{{name: "c0", image: gantryImage}}},
		// A Pending pod that has no containerStatuses at all must not crash the
		// jq filter.
		pod{name: "gantry-b", noStatus: true, containers: []container{{name: "c0", image: gantryImage}}},
	)})

	output, code := f.run(nil, target, "deploy/machina-controller")

	requireCode(t, code, 0, output)
	requireContains(t, output, "OK: all workloads rolled out")
	requireGroupsBalanced(t, output)
}

func TestFailsAndNamesTheImageWhenPullBackoffPersists(t *testing.T) {
	requireBash(t)
	t.Parallel()

	f := newFake(t)
	f.set("getjson-ds_gantry", reply{stdout: workload(selectorLabel, gantryImage)})
	f.set("rollout", reply{sleep: "20"})
	f.set("pods", reply{stdout: podList(pod{name: "gantry-dh58w", containers: []container{
		{name: "c0", image: gantryImage, waiting: "ImagePullBackOff", message: `Back-off pulling image "` + gantryImage + `"`},
	}})})

	output, code := f.run(nil, target)

	requireCode(t, code, 1, output)
	requireContains(t, output, "pod unbounded-system/gantry-dh58w container c0 cannot pull "+gantryImage)
	requireContains(t, output, "ImagePullBackOff")
	requireContains(t, output, "the pipeline is missing an image the operator references")
	requireGroupsBalanced(t, output)
}

func TestToleratesPullBackoffThatRecoversWithinGrace(t *testing.T) {
	requireBash(t)
	t.Parallel()

	f := newFake(t)
	f.set("getjson-ds_gantry", reply{stdout: workload(selectorLabel, gantryImage)})
	f.set("rollout", reply{sleep: "4"})

	// Backing off on the first poll, healthy afterwards: a registry blip.
	f.setNth("pods", 1, reply{stdout: podList(pod{name: "gantry-a", containers: []container{
		{name: "c0", image: gantryImage, waiting: "ImagePullBackOff"},
	}})})
	f.set("pods", reply{stdout: podList(pod{name: "gantry-a", containers: []container{
		{name: "c0", image: gantryImage},
	}})})

	output, code := f.run(map[string]string{"IMAGE_FAILURE_GRACE_SECONDS": "30"}, target)

	requireCode(t, code, 0, output)
	requireContains(t, output, "allowing 30s for it to recover")
	requireNotContains(t, output, "::error::")
	requireGroupsBalanced(t, output)
}

func TestFailsImmediatelyOnInvalidImageName(t *testing.T) {
	requireBash(t)
	t.Parallel()

	f := newFake(t)
	f.set("getjson-ds_gantry", reply{stdout: workload(selectorLabel, "ghcr.io/azure/gantry:BAD:REF")})
	f.set("rollout", reply{sleep: "20"})
	f.set("pods", reply{stdout: podList(pod{name: "gantry-a", containers: []container{
		{name: "c0", image: "ghcr.io/azure/gantry:BAD:REF", waiting: "InvalidImageName"},
	}})})

	// A grace period long enough that only the terminal path can abort here.
	output, code := f.run(map[string]string{"IMAGE_FAILURE_GRACE_SECONDS": "600"}, target)

	requireCode(t, code, 1, output)
	requireContains(t, output, "InvalidImageName")
	requireGroupsBalanced(t, output)
}

func TestIgnoresPodsFromASupersededRevision(t *testing.T) {
	requireBash(t)
	t.Parallel()

	f := newFake(t)
	f.set("getjson-ds_gantry", reply{stdout: workload(selectorLabel, gantryImage)})
	f.set("pods", reply{stdout: podList(
		// Wedged, but on an image this rollout no longer wants. Replacing it is
		// exactly what the rollout under way is doing.
		pod{name: "gantry-old", containers: []container{
			{name: "c0", image: staleImage, waiting: "ImagePullBackOff"},
		}},
		pod{name: "gantry-new", containers: []container{{name: "c0", image: gantryImage}}},
	)})

	output, code := f.run(map[string]string{"IMAGE_FAILURE_GRACE_SECONDS": "0"}, target)

	requireCode(t, code, 0, output)
	requireNotContains(t, output, staleImage)
	// The guard must have actually inspected the pods; otherwise this passes
	// for the wrong reason.
	requireContains(t, f.calls(), "get pods --selector")
	requireGroupsBalanced(t, output)
}

func TestIgnoresTerminatingPods(t *testing.T) {
	requireBash(t)
	t.Parallel()

	f := newFake(t)
	f.set("getjson-ds_gantry", reply{stdout: workload(selectorLabel, gantryImage)})
	f.set("pods", reply{stdout: podList(pod{
		name:        "gantry-terminating",
		terminating: true,
		containers:  []container{{name: "c0", image: gantryImage, waiting: "ImagePullBackOff"}},
	})})

	output, code := f.run(map[string]string{"IMAGE_FAILURE_GRACE_SECONDS": "0"}, target)

	requireCode(t, code, 0, output)
	requireContains(t, f.calls(), "get pods --selector")
	requireGroupsBalanced(t, output)
}

func TestIgnoresTransientErrImagePull(t *testing.T) {
	requireBash(t)
	t.Parallel()

	f := newFake(t)
	f.set("getjson-ds_gantry", reply{stdout: workload(selectorLabel, gantryImage)})
	f.set("pods", reply{stdout: podList(pod{name: "gantry-a", containers: []container{
		{name: "c0", image: gantryImage, waiting: "ErrImagePull"},
	}})})

	output, code := f.run(map[string]string{"IMAGE_FAILURE_GRACE_SECONDS": "0"}, target)

	requireCode(t, code, 0, output)
	requireContains(t, f.calls(), "get pods --selector")
	requireGroupsBalanced(t, output)
}

// TestScopesThePodQueryToTheWorkload is the regression guard for the deadlock
// where an unrelated tenant of the namespace, such as Orca, could abort the
// core deploy that a later job depends on.
func TestScopesThePodQueryToTheWorkload(t *testing.T) {
	requireBash(t)
	t.Parallel()

	f := newFake(t)
	f.set("getjson-ds_gantry", reply{stdout: workload(selectorLabel, gantryImage)})
	f.set("pods", reply{stdout: podList(pod{name: "gantry-a", containers: []container{
		{name: "c0", image: gantryImage},
	}})})

	output, code := f.run(nil, target)

	requireCode(t, code, 0, output)
	requireContains(t, f.calls(), "get pods --selector "+selectorLabel)
	requireNotContains(t, f.calls(), "get pods -o json")
}

func TestSkipsTheImageCheckWithoutAMatchLabelsSelector(t *testing.T) {
	requireBash(t)
	t.Parallel()

	f := newFake(t)
	f.set("getjson-ds_gantry", reply{stdout: workload("", gantryImage)})

	output, code := f.run(nil, target)

	requireCode(t, code, 0, output)
	requireContains(t, output, "no matchLabels selector; skipping the image check")
	requireNotContains(t, f.calls(), "get pods")
}

// TestDetectsAFailedInitContainerImage covers the init path: a failed init
// image never lets the main container start, so the pod would otherwise sit in
// Pending with no main-container status to look at.
func TestDetectsAFailedInitContainerImage(t *testing.T) {
	requireBash(t)
	t.Parallel()

	f := newFake(t)
	f.set("getjson-ds_gantry", reply{stdout: workloadWithInit(selectorLabel, gantryImage, initImage)})
	f.set("rollout", reply{sleep: "20"})
	f.set("pods", reply{stdout: podList(pod{name: "gantry-a", containers: []container{
		{name: "chown-hostpaths", image: initImage, waiting: "ImagePullBackOff", init: true},
	}})})

	output, code := f.run(nil, target)

	requireCode(t, code, 1, output)
	requireContains(t, output, "container chown-hostpaths cannot pull "+initImage)
	requireGroupsBalanced(t, output)
}

func TestPropagatesRolloutFailure(t *testing.T) {
	requireBash(t)
	t.Parallel()

	f := newFake(t)
	f.set("getjson-ds_gantry", reply{stdout: workload(selectorLabel, gantryImage)})
	f.set("pods", reply{stdout: podList(pod{name: "gantry-a", containers: []container{
		{name: "c0", image: gantryImage},
	}})})
	f.set("rollout", reply{
		stderr: "error: deployment exceeded its progress deadline",
		exit:   1,
	})

	output, code := f.run(nil, target)

	requireCode(t, code, 1, output)
	requireContains(t, output, "exceeded its progress deadline")
	requireGroupsBalanced(t, output)
}

func TestTimesOutWhenTheWorkloadIsNeverCreated(t *testing.T) {
	requireBash(t)
	t.Parallel()

	f := newFake(t)
	f.set("get-ds_gantry", reply{
		stderr: `Error from server (NotFound): daemonsets.apps "gantry" not found`,
		exit:   1,
	})

	output, code := f.run(map[string]string{"CREATE_TIMEOUT_SECONDS": "1"}, target)

	requireCode(t, code, 1, output)
	requireContains(t, output, "timed out waiting for ds/gantry in unbounded-system to be created")
	requireGroupsBalanced(t, output)
}

// TestReportsNonNotFoundErrorsWhileWaiting covers the case where the wait was
// previously reported as a workload the operator never created, hiding the
// real cause.
func TestReportsNonNotFoundErrorsWhileWaiting(t *testing.T) {
	requireBash(t)
	t.Parallel()

	f := newFake(t)
	f.set("get-ds_gantry", reply{
		stderr: `Error from server (Forbidden): daemonsets.apps "gantry" is forbidden`,
		exit:   1,
	})

	output, code := f.run(map[string]string{"CREATE_TIMEOUT_SECONDS": "1"}, target)

	requireCode(t, code, 1, output)
	requireContains(t, output, "::warning::querying ds/gantry in unbounded-system failed")
	requireContains(t, output, "Forbidden")
	requireContains(t, output, "last error:")
	requireGroupsBalanced(t, output)
}

// TestWarnsButContinuesWhenThePodQueryFails covers the fail-open contract: a
// transient API error must not fail a deploy, but it must not be silent either.
func TestWarnsButContinuesWhenThePodQueryFails(t *testing.T) {
	requireBash(t)
	t.Parallel()

	f := newFake(t)
	f.set("getjson-ds_gantry", reply{stdout: workload(selectorLabel, gantryImage)})
	f.set("rollout", reply{sleep: "3"})
	f.set("pods", reply{
		stderr: "Unable to connect to the server: dial tcp: i/o timeout",
		exit:   1,
	})

	output, code := f.run(nil, target)

	requireCode(t, code, 0, output)
	requireContains(t, output, "::warning::could not list pods for ds/gantry")
	requireContains(t, output, "Unable to connect to the server")
	requireGroupsBalanced(t, output)
}

// TestDisablesTheGuardWhenPodStatusCannotBeParsed covers the other fail-open
// path: unparseable output disables the check loudly rather than being
// silently read as "no image failures".
func TestDisablesTheGuardWhenPodStatusCannotBeParsed(t *testing.T) {
	requireBash(t)
	t.Parallel()

	f := newFake(t)
	f.set("getjson-ds_gantry", reply{stdout: workload(selectorLabel, gantryImage)})
	f.set("rollout", reply{sleep: "3"})
	f.set("pods", reply{stdout: "this is not json"})

	output, code := f.run(nil, target)

	requireCode(t, code, 0, output)
	requireContains(t, output, "image guard disabled")
	requireContains(t, output, "fix wait-rollouts.sh")
	requireGroupsBalanced(t, output)
}

func TestRejectsMissingArguments(t *testing.T) {
	requireBash(t)
	t.Parallel()

	f := newFake(t)

	output, code := f.run(nil)

	requireCode(t, code, 2, output)
	requireContains(t, output, "requires at least one <kind>/<name> argument")
}

func TestRequiresKubeconfig(t *testing.T) {
	requireBash(t)
	t.Parallel()

	f := newFake(t)

	output, code := f.run(map[string]string{"KUBECONFIG": ""}, target)

	if code == 0 {
		t.Errorf("expected a non-zero exit without KUBECONFIG\n--- output ---\n%s", output)
	}

	requireContains(t, output, "KUBECONFIG")
}

// ---------------------------------------------------------------------------
// Degraded-node tolerance.
//
// A DaemonSet counts every node toward desiredNumberScheduled, including nodes
// the kubelet has stopped reporting for, so one unreachable node blocks
// `rollout status` until its timeout every single time. Tolerating that is the
// only thing in this script that can turn a failing wait into a passing one,
// which is why it is fail-closed and why the cases below spend most of their
// effort on the ways it must REFUSE.
// ---------------------------------------------------------------------------

// degradedNodes is a three-node cluster with one node the cluster has lost
// contact with.
//
// The condition is Unknown, not False: that is what an unreachable kubelet
// reports, and a check written against False would silently never fire.
func degradedNodes() string {
	return nodeList(
		node{name: "node-a", site: "hq", ready: "True"},
		node{name: "node-b", site: "hq", ready: "True"},
		node{name: "spark-3d37", site: "boulderlab", ready: "Unknown"},
	)
}

// strandedFleet is the pod list that goes with degradedNodes: two healthy pods
// on reachable nodes and one the controller can neither update nor reap.
func strandedFleet(image string) string {
	return podList(
		pod{name: "gantry-a", node: "node-a", containers: []container{{name: "c0", image: image}}},
		pod{name: "gantry-b", node: "node-b", containers: []container{{name: "c0", image: image}}},
		pod{
			name: "gantry-c", node: "spark-3d37", notReady: true,
			containers: []container{{name: "c0", image: image}},
		},
	)
}

// shortByOne is the status of a DaemonSet whose only missing pod is the
// stranded one: the controller has caught up, and everything it can reach is
// updated and Ready.
var shortByOne = dsStatus{desired: 3, ready: 2, updated: 2, generation: 4, observed: 4}

// tolerating is the env a deploy gate runs with: the release cap of two dead
// nodes, and the tag the operator resolves component images at.
var tolerating = map[string]string{
	"MAX_NOTREADY_NODES": "2",
	"EXPECTED_IMAGE_TAG": releaseTag,
}

func withEnv(extra map[string]string) map[string]string {
	merged := map[string]string{}
	for key, value := range tolerating {
		merged[key] = value
	}

	for key, value := range extra {
		merged[key] = value
	}

	return merged
}

func TestToleratesAShortfallExplainedByUnreachableNodes(t *testing.T) {
	requireBash(t)
	t.Parallel()

	f := newFake(t)
	f.set("getjson-ds_gantry", reply{stdout: daemonSet(selectorLabel, shortByOne, gantryImage)})
	f.set("getjson-nodes", reply{stdout: degradedNodes()})
	f.set("pods", reply{stdout: strandedFleet(gantryImage)})
	// Held open: without tolerance this wait would run to its timeout, which is
	// the behaviour being replaced.
	f.set("rollout", reply{sleep: "20"})

	output, code := f.run(tolerating, target)

	requireCode(t, code, 0, output)
	requireContains(t, output, "tolerating the ds/gantry shortfall")
	requireContains(t, output, "short 1 of 3 pods")
	requireContains(t, output, "boulderlab[spark-3d37]")
	requireContains(t, output, "OK: all workloads rolled out")
	requireGroupsBalanced(t, output)
}

// TestRefusesToTolerateThePreviousRelease is the regression guard for a
// false green: component workloads are updated asynchronously, so early in a
// deploy the DaemonSet still carries the tag it had before. Every other
// condition holds in that window, so without the image check the gate would
// pass in about a second having validated nothing.
func TestRefusesToTolerateThePreviousRelease(t *testing.T) {
	requireBash(t)
	t.Parallel()

	f := newFake(t)
	f.set("getjson-ds_gantry", reply{stdout: daemonSet(selectorLabel, shortByOne, staleImage)})
	f.set("getjson-nodes", reply{stdout: degradedNodes()})
	f.set("pods", reply{stdout: strandedFleet(staleImage)})
	f.set("rollout", reply{sleep: "3", exit: 1})

	output, code := f.run(tolerating, target)

	requireCode(t, code, 1, output)
	requireContains(t, output, "does not reference :"+releaseTag+" yet")
	requireNotContains(t, output, "tolerating the ds/gantry shortfall")
	requireGroupsBalanced(t, output)
}

// TestToleratesOnceTheOperatorUpdatesTheWorkload is the other half: the wait
// must not deadlock waiting for a tag that does arrive. Calls 1 and 2 are the
// spec resolve and the first poll, both still on the old release; the operator
// reconciles, and the poll after that tolerates.
func TestToleratesOnceTheOperatorUpdatesTheWorkload(t *testing.T) {
	requireBash(t)
	t.Parallel()

	f := newFake(t)
	f.set("getjson-ds_gantry", reply{stdout: daemonSet(selectorLabel, shortByOne, gantryImage)})
	f.setNth("getjson-ds_gantry", 1, reply{stdout: daemonSet(selectorLabel, shortByOne, staleImage)})
	f.setNth("getjson-ds_gantry", 2, reply{stdout: daemonSet(selectorLabel, shortByOne, staleImage)})
	f.set("getjson-nodes", reply{stdout: degradedNodes()})
	f.set("pods", reply{stdout: strandedFleet(gantryImage)})
	f.set("rollout", reply{sleep: "20"})

	output, code := f.run(tolerating, target)

	requireCode(t, code, 0, output)
	requireContains(t, output, "does not reference :"+releaseTag+" yet")
	requireContains(t, output, "tolerating the ds/gantry shortfall")
	requireGroupsBalanced(t, output)
}

func TestRefusesToTolerateBeyondTheCap(t *testing.T) {
	requireBash(t)
	t.Parallel()

	f := newFake(t)
	f.set("getjson-ds_gantry", reply{stdout: daemonSet(selectorLabel, shortByOne, gantryImage)})
	f.set("getjson-nodes", reply{stdout: nodeList(
		node{name: "node-a", site: "hq", ready: "True"},
		node{name: "node-b", site: "edge", ready: "Unknown"},
		node{name: "node-c", site: "edge", ready: "Unknown"},
		node{name: "spark-3d37", site: "boulderlab", ready: "Unknown"},
	)})
	f.set("pods", reply{stdout: strandedFleet(gantryImage)})
	f.set("rollout", reply{sleep: "3", exit: 1})

	output, code := f.run(tolerating, target)

	requireCode(t, code, 1, output)
	requireContains(t, output, "too many NotReady nodes")
	requireContains(t, output, "3 > MAX_NOTREADY_NODES=2")
	// Grouped by site and in first-seen order, so the message is stable enough
	// to be compared between polls of an unchanged cluster.
	requireContains(t, output, "edge[node-b, node-c] boulderlab[spark-3d37]")
	requireNotContains(t, output, "tolerating the ds/gantry shortfall")
}

// TestRefusesToTolerateWhenAReachablePodIsUnhealthy keeps the check honest
// about what it is excusing. A pod that is broken on a node the cluster can
// still talk to is a real failure, and the fact that some OTHER node is
// unreachable says nothing about it.
func TestRefusesToTolerateWhenAReachablePodIsUnhealthy(t *testing.T) {
	requireBash(t)
	t.Parallel()

	f := newFake(t)
	f.set("getjson-ds_gantry", reply{
		stdout: daemonSet(selectorLabel, dsStatus{desired: 3, ready: 1, updated: 2, generation: 4, observed: 4}, gantryImage),
	})
	f.set("getjson-nodes", reply{stdout: degradedNodes()})
	f.set("pods", reply{stdout: podList(
		pod{name: "gantry-a", node: "node-a", containers: []container{{name: "c0", image: gantryImage}}},
		// On a READY node and not Ready: a real failure.
		pod{
			name: "gantry-b", node: "node-b", notReady: true,
			containers: []container{{name: "c0", image: gantryImage}},
		},
		pod{
			name: "gantry-c", node: "spark-3d37", notReady: true,
			containers: []container{{name: "c0", image: gantryImage}},
		},
	)})
	f.set("rollout", reply{sleep: "3", exit: 1})

	output, code := f.run(tolerating, target)

	requireCode(t, code, 1, output)
	requireNotContains(t, output, "tolerating the ds/gantry shortfall")
}

// TestNeverToleratesADeploymentShortfall covers the kind check. A Deployment
// reschedules off a dead node, so a shortfall there is a scheduling problem,
// not a stranded pod. The kind comes from the spec read before the wait, so
// this must also cost no node query at all.
func TestNeverToleratesADeploymentShortfall(t *testing.T) {
	requireBash(t)
	t.Parallel()

	f := newFake(t)
	f.set("getjson-deploy_machina-controller", reply{
		stdout: deployment("app=machina", shortByOne, gantryImage),
	})
	f.set("getjson-nodes", reply{stdout: degradedNodes()})
	f.set("pods", reply{stdout: strandedFleet(gantryImage)})
	f.set("rollout", reply{sleep: "3", exit: 1})

	output, code := f.run(tolerating, "deploy/machina-controller")

	requireCode(t, code, 1, output)
	requireNotContains(t, output, "tolerating")
	requireNotContains(t, f.calls(), "get nodes")
}

func TestRefusesToTolerateWhenNodeReadinessCannotBeRead(t *testing.T) {
	requireBash(t)
	t.Parallel()

	f := newFake(t)
	f.set("getjson-ds_gantry", reply{stdout: daemonSet(selectorLabel, shortByOne, gantryImage)})
	f.set("getjson-nodes", reply{stderr: "error: the server could not find the requested resource", exit: 1})
	f.set("pods", reply{stdout: strandedFleet(gantryImage)})
	f.set("rollout", reply{sleep: "3", exit: 1})

	output, code := f.run(tolerating, target)

	requireCode(t, code, 1, output)
	requireContains(t, output, "could not evaluate node readiness for ds/gantry")
	requireNotContains(t, output, "tolerating the ds/gantry shortfall")
}

// TestCapOfZeroDisablesToleranceWithoutQueryingNodes covers the switch-off
// path and the cost of it: a run that cannot tolerate anything must not pay
// for a full node list on every poll to find that out.
func TestCapOfZeroDisablesToleranceWithoutQueryingNodes(t *testing.T) {
	requireBash(t)
	t.Parallel()

	f := newFake(t)
	f.set("getjson-ds_gantry", reply{stdout: daemonSet(selectorLabel, shortByOne, gantryImage)})
	f.set("getjson-nodes", reply{stdout: degradedNodes()})
	f.set("pods", reply{stdout: strandedFleet(gantryImage)})
	f.set("rollout", reply{sleep: "3", exit: 1})

	output, code := f.run(withEnv(map[string]string{"MAX_NOTREADY_NODES": "0"}), target)

	requireCode(t, code, 1, output)
	requireNotContains(t, output, "tolerating the ds/gantry shortfall")
	requireNotContains(t, f.calls(), "get nodes")
}

// TestWithoutAnExpectedTagToleranceIsUnavailable covers the other switch-off
// path. With no tag there is no way to tell the release being deployed from
// the one it replaces, so the only safe answer is to keep waiting, and to say
// why rather than appearing to work.
func TestWithoutAnExpectedTagToleranceIsUnavailable(t *testing.T) {
	requireBash(t)
	t.Parallel()

	f := newFake(t)
	f.set("getjson-ds_gantry", reply{stdout: daemonSet(selectorLabel, shortByOne, gantryImage)})
	f.set("getjson-nodes", reply{stdout: degradedNodes()})
	f.set("pods", reply{stdout: strandedFleet(gantryImage)})
	f.set("rollout", reply{sleep: "3", exit: 1})

	output, code := f.run(map[string]string{"MAX_NOTREADY_NODES": "2"}, target)

	requireCode(t, code, 1, output)
	requireContains(t, output, "EXPECTED_IMAGE_TAG is not set")
	requireNotContains(t, output, "tolerating the ds/gantry shortfall")
	requireNotContains(t, f.calls(), "get nodes")
}

func TestRejectsANonNumericCap(t *testing.T) {
	requireBash(t)
	t.Parallel()

	f := newFake(t)

	output, code := f.run(withEnv(map[string]string{"MAX_NOTREADY_NODES": "two"}), target)

	requireCode(t, code, 2, output)
	requireContains(t, output, "MAX_NOTREADY_NODES must be a non-negative integer")
}

// TestAnUnreachableNodeCannotMaskAMissingImage locks in the order of the two
// polls. A stranded pod never pulls anything, so a NotReady node can never be
// the reason an image is missing, and the pipeline error must still win.
func TestAnUnreachableNodeCannotMaskAMissingImage(t *testing.T) {
	requireBash(t)
	t.Parallel()

	f := newFake(t)
	f.set("getjson-ds_gantry", reply{stdout: daemonSet(selectorLabel, shortByOne, gantryImage)})
	f.set("getjson-nodes", reply{stdout: degradedNodes()})
	f.set("pods", reply{stdout: podList(
		pod{
			name: "gantry-a", node: "node-a",
			containers: []container{{name: "c0", image: gantryImage, waiting: "InvalidImageName"}},
		},
		pod{
			name: "gantry-c", node: "spark-3d37", notReady: true,
			containers: []container{{name: "c0", image: gantryImage}},
		},
	)})
	f.set("rollout", reply{sleep: "20"})

	output, code := f.run(tolerating, target)

	requireCode(t, code, 1, output)
	requireContains(t, output, "cannot pull "+gantryImage)
	requireNotContains(t, output, "tolerating the ds/gantry shortfall")
	requireGroupsBalanced(t, output)
}

// TestRecordsATolerationInTheStepSummary covers the durable record. The run
// log scrolls past; the job summary is what a reviewer sees when asking what
// this release was actually validated against.
func TestRecordsATolerationInTheStepSummary(t *testing.T) {
	requireBash(t)
	t.Parallel()

	f := newFake(t)
	f.set("getjson-ds_gantry", reply{stdout: daemonSet(selectorLabel, shortByOne, gantryImage)})
	f.set("getjson-nodes", reply{stdout: degradedNodes()})
	f.set("pods", reply{stdout: strandedFleet(gantryImage)})
	f.set("rollout", reply{sleep: "20"})

	summary := filepath.Join(f.dir, "summary.md")

	output, code := f.run(withEnv(map[string]string{"GITHUB_STEP_SUMMARY": summary}), target)

	requireCode(t, code, 0, output)

	written, err := os.ReadFile(summary)
	if err != nil {
		t.Fatalf("read step summary: %v", err)
	}

	requireContains(t, string(written), "Degraded rollout tolerated: ds/gantry")
	requireContains(t, string(written), "Ready pods: 2/3")
	requireContains(t, string(written), "boulderlab[spark-3d37]")
	requireContains(t, string(written), "Deployed image tag: `"+releaseTag+"`")
}
