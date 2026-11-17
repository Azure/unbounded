//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

const (
	pageSizeSmallCacheName = "e2e-page-size-small"
	racerObjectPageSizeEnv = "RACER_OBJECT_PAGE_SIZE"
	// pageSizeDefaultLabel is the page size the racer-object sidecar and the
	// cluster use when neither the ClusterCache nor the sidecar sets one.
	pageSizeDefaultLabel = "16MiB"
)

// pageSizeObject is an immutable blob in the public MNIST container with a
// known digest and length, so a cache read can prove it returned every byte.
type pageSizeObject struct {
	key    string
	sha256 string
	size   int
}

// pageSizeSmallObjects models the many small immutable objects a git client
// reads: manifests, checksum sidecars, and small label files. Some fit in one
// 4KiB page and some span a few pages.
var pageSizeSmallObjects = []pageSizeObject{
	{key: "processedManifest/manifest.json", sha256: "073da036833407615b173e04b167c581c6302825df78a5c5a69e351d39ae117d", size: 1344},
	{key: "processedManifest/manifest.json.sha256", sha256: "d4ca7a33239952fe9f21b80996b915dfa1f995b860d235cc6df22ae3e5b3c151", size: 128},
	{key: "processedManifest/spdx_2.2/manifest.spdx.json", sha256: "a9bb0c010dd1ebc98731f43b0f6e53474ca483f4c58ee07cbf7e39d43cd12aff", size: 3923},
	{key: "processedManifest/spdx_2.2/manifest.spdx.json.sha256", sha256: "15cd0ae77123f121e01dedc35807dd7b92ebeafcd478aac8a5c967973d699f74", size: 128},
	{key: "t10k-labels-idx1-ubyte.gz", sha256: "f7ae60f92e00ec6debd23a6088c31dbd2371eca3ffa0defaefb259924204aec6", size: 4542},
	{key: "t10k-labels-idx1-ubyte.csv", sha256: "b00c1c90c51a6005aa65dbdac2843589c7580a99541ad50ec435a545b6c25947", size: 20000},
	{key: "temp/t10k-labels-idx1-ubyte.csv", sha256: "e9bfb041a4e05a2908580dc95d4d93544d50705ec0de9d191c9e83ba2db9a392", size: 20006},
	{key: "train-labels-idx1-ubyte.gz", sha256: "3552534a0a558bbed6aed32b30c495cca23d567ec52cac8be1a0730e8010255c", size: 28881},
}

// pageSizeLargeObject spans about thirty 4KiB pages, so a wrong cache page
// size or a truncated subscription cannot produce the right digest by accident.
var pageSizeLargeObject = pageSizeObject{
	key:    "train-labels-idx1-ubyte.csv",
	sha256: "3a59b521d740e5f50843de7eb9045de3f84d2abd4e87841e7b2b699e67bb7958",
	size:   120000,
}

// TestClusterCachePerPageSize runs a 4KiB ClusterCache and the default 16MiB
// ClusterCache in the same cluster and reads the same objects through each.
//
// The webhook injects the sidecar page size from spec.pageSize, so the 4KiB
// pod must carry --page-size=4KiB while the default pod omits it and uses the
// sidecar's 16MiB default. racer-object is a read-only object API, so the
// git-like workload is reads: many small immutable blobs read twice, plus one
// larger blob read whole and across a 4KiB page boundary.
func TestClusterCachePerPageSize(t *testing.T) {
	requireAzureEgress(t, containerURL+"/"+pageSizeLargeObject.key)

	h := cluster(t)

	const (
		smallNamespace   = "fetch-page-size-small"
		defaultNamespace = "fetch-page-size-default"
	)

	t.Cleanup(func() {
		h.dumpDiagnostics(smallNamespace, defaultNamespace)
		h.try(time.Minute, "kubectl", "delete", "clustercache", pageSizeSmallCacheName, "--ignore-not-found", "--wait=false")
		h.try(time.Minute, "kubectl", "delete", "namespace", smallNamespace, defaultNamespace, "--ignore-not-found", "--wait=false")
	})

	h.apply(pageSizeCacheManifest(pageSizeSmallCacheName, "4KiB"))
	h.applyCache()
	h.installRacer()
	h.waitWebhook()
	h.waitConverged("both page-size caches published")

	t.Run("4KiB cache", func(t *testing.T) {
		child := h.forTest(t)
		child.apply(pageSizeNamespaceManifest(smallNamespace))
		child.apply(pageSizeFetchPod(smallNamespace, pageSizeSmallCacheName))
		child.requireSidecarPageSize(smallNamespace, "fetch", "4KiB")
		child.waitPageSizeFetch(smallNamespace)
	})

	t.Run("16MiB default cache", func(t *testing.T) {
		child := h.forTest(t)
		child.apply(pageSizeNamespaceManifest(defaultNamespace))
		child.apply(pageSizeFetchPod(defaultNamespace, cacheName))
		child.requireSidecarPageSize(defaultNamespace, "fetch", pageSizeDefaultLabel)
		child.waitPageSizeFetch(defaultNamespace)
	})
}

// pageSizeCacheManifest returns a ClusterCache with an anonymous Azure origin
// and an explicit page size.
func pageSizeCacheManifest(name, pageSize string) string {
	return fmt.Sprintf(`apiVersion: racer.unbounded-cloud.io/v1alpha1
kind: ClusterCache
metadata:
  name: %s
spec:
  pageSize: %s
  origin:
    azure:
      containerURL: %s
`, name, pageSize, containerURL)
}

func pageSizeNamespaceManifest(name string) string {
	return fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n", name)
}

// TestPageSizeFetchPodManifest checks the generated pod without a cluster, so
// an escaping mistake fails a fast test instead of a long e2e run.
func TestPageSizeFetchPodManifest(t *testing.T) {
	manifest := pageSizeFetchPod("page-size-ns", pageSizeSmallCacheName)
	if strings.Contains(manifest, "\t") {
		t.Fatal("pod manifest contains a tab")
	}

	var pod corev1.Pod
	if err := yaml.Unmarshal([]byte(manifest), &pod); err != nil {
		t.Fatalf("decode pod manifest: %v\n%s", err, manifest)
	}

	if pod.Namespace != "page-size-ns" {
		t.Fatalf("namespace = %q", pod.Namespace)
	}

	if got := pod.Annotations["racer.unbounded-cloud.io/object-cache"]; got != pageSizeSmallCacheName {
		t.Fatalf("object-cache annotation = %q", got)
	}

	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("containers = %d, want 1", len(pod.Spec.Containers))
	}

	command := pod.Spec.Containers[0].Command
	if len(command) != 3 || command[0] != "sh" || command[1] != "-c" {
		t.Fatalf("command = %q", command)
	}

	script := command[2]
	for _, o := range pageSizeSmallObjects {
		if !strings.Contains(script, o.sha256) || !strings.Contains(script, o.key) {
			t.Fatalf("script is missing small object %s", o.key)
		}
	}

	if !strings.Contains(script, pageSizeLargeObject.sha256) || !strings.Contains(script, pageSizeLargeObject.key) {
		t.Fatal("script is missing the large object")
	}
}

// pageSizeFetchPod returns a pod bound to cache. The injected racer-object
// sidecar serves the S3 API on loopback, and the app verifies every blob
// digest and length.
func pageSizeFetchPod(namespace, cache string) string {
	var objects strings.Builder

	for _, o := range pageSizeSmallObjects {
		fmt.Fprintf(&objects, "%s %s %d\n", o.sha256, o.key, o.size)
	}

	script := fmt.Sprintf(`set -eu
fetch() {
  n=0
  until curl -fsS --max-time 120 -o "$2" "$1"; do
    n=$((n+1))
    [ "$n" -lt 60 ] || return 1
    sleep 2
  done
}
check() {
  echo "$1  $2" | sha256sum -c -
  size=$(wc -c < "$2" | tr -d ' ')
  [ "$size" = "$3" ] || { echo "size mismatch for $2: got $size, want $3" >&2; exit 1; }
}
for round in 1 2; do
  while read -r sha key want; do
    fetch "http://127.0.0.1:8080/%s/$key" /tmp/object
    check "$sha" /tmp/object "$want"
  done <<'OBJECTS'
%sOBJECTS
  echo "small-round-$round-ok"
done
fetch "http://127.0.0.1:8080/%s/%s" /tmp/large
check "%s" /tmp/large %d
echo "large-object-ok"
curl -fsS -r 4095-8194 -D /tmp/range-head -o /tmp/range "http://127.0.0.1:8080/%s/%s"
large_size=$(wc -c < /tmp/large | tr -d ' ')
tr -d '\r' < /tmp/range-head > /tmp/range-head.txt
grep -q '^HTTP/1.1 206 ' /tmp/range-head.txt
grep -qix "content-range: bytes 4095-8194/$large_size" /tmp/range-head.txt
tail -c +4096 /tmp/large | head -c 4100 | cmp - /tmp/range
echo "large-range-ok"`,
		bucket, objects.String(),
		bucket, pageSizeLargeObject.key, pageSizeLargeObject.sha256, pageSizeLargeObject.size,
		bucket, pageSizeLargeObject.key)

	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: fetch
  namespace: %s
  labels:
    app: fetch
    racer.unbounded-cloud.io/object-sidecar: enabled
  annotations:
    racer.unbounded-cloud.io/object-cache: %s
    racer.unbounded-cloud.io/object-api: s3
spec:
  restartPolicy: Never
  containers:
  - name: app
    image: curlimages/curl:8.11.1
    command: ["sh", "-c", %q]
`, namespace, cache, script)
}

// waitPageSizeFetch waits for the app to finish and checks that the sidecar
// was injected and every digest, length, and range check ran.
func (h *harness) waitPageSizeFetch(namespace string) {
	h.t.Helper()

	h.waitFor("fetch pod in "+namespace+" to succeed", 10*time.Minute, func() (bool, string) {
		out, _ := h.exec(time.Minute, nil, "kubectl", "-n", namespace, "get", "pod", "fetch", "-o", "jsonpath={.status.phase}")

		phase := strings.TrimSpace(out)
		if phase == "Failed" {
			h.t.Fatalf("fetch pod in %s failed", namespace)
		}

		return phase == "Succeeded", "phase=" + phase
	})

	injected := h.kubectl("-n", namespace, "get", "pod", "fetch", "-o",
		`jsonpath={.metadata.annotations.racer\.unbounded-cloud\.io/object-sidecar-injected} {.spec.initContainers[*].name}`)
	if !strings.Contains(injected, "racer-object") {
		h.t.Fatalf("racer-object sidecar not injected: %q", injected)
	}

	logs := h.kubectl("-n", namespace, "logs", "fetch", "-c", "app")
	for _, marker := range []string{"small-round-1-ok", "small-round-2-ok", "large-object-ok", "large-range-ok"} {
		if !strings.Contains(logs, marker) {
			h.t.Fatalf("missing %q; logs:\n%s", marker, logs)
		}
	}
}

// requireSidecarPageSize checks the page size the injected sidecar was
// configured with. An absent flag or environment variable means the sidecar's
// 16MiB default.
func (h *harness) requireSidecarPageSize(namespace, pod, want string) {
	h.t.Helper()

	got, source, ok := h.sidecarPageSize(namespace, pod)
	if !ok {
		h.t.Fatalf("racer-object sidecar is not injected into %s/%s: %s", namespace, pod, source)
	}

	if got != want {
		h.t.Fatalf("racer-object sidecar in %s/%s uses page size %q via %s, want %q", namespace, pod, got, source, want)
	}

	h.t.Logf("racer-object sidecar in %s/%s uses page size %s via %s", namespace, pod, got, source)
}

// sidecarPageSize reads the effective page size from the injected sidecar.
// The webhook passes spec.pageSize as a flag, and the sidecar also accepts
// RACER_OBJECT_PAGE_SIZE as an environment fallback.
func (h *harness) sidecarPageSize(namespace, pod string) (string, string, bool) {
	h.t.Helper()

	var p corev1.Pod
	if !h.getJSON(&p, "-n", namespace, "get", "pod", pod) {
		return "", "pod not readable", false
	}

	for _, c := range p.Spec.InitContainers {
		if c.Name != "racer-object" {
			continue
		}

		for i, arg := range c.Args {
			name, value, hasValue := strings.Cut(arg, "=")
			if name != "--page-size" && name != "-page-size" {
				continue
			}

			if hasValue {
				return value, "flag", true
			}

			if i+1 < len(c.Args) {
				return c.Args[i+1], "flag", true
			}
		}

		for _, e := range c.Env {
			if e.Name != racerObjectPageSizeEnv {
				continue
			}

			if e.ValueFrom != nil {
				return "", "environment fallback from a source", true
			}

			return e.Value, racerObjectPageSizeEnv, true
		}

		return pageSizeDefaultLabel, "default", true
	}

	return "", "no racer-object init container", false
}
