//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	containerURL = "https://azureopendatastorage.blob.core.windows.net/" + bucket
	bucket       = "mnist"
	blobKey      = "t10k-labels-idx1-ubyte.gz"
	blobSHA256   = "f7ae60f92e00ec6debd23a6088c31dbd2371eca3ffa0defaefb259924204aec6"
	cacheName    = "e2e-cache"
)

// TestObjectCacheFromAzureOrigin installs Racer through the operator from a
// ClusterCache with an anonymous Azure origin, then reads a public blob
// through the injected racer-object sidecar using both the S3 and Azure APIs.
func TestObjectCacheFromAzureOrigin(t *testing.T) {
	requireAzureEgress(t, containerURL+"/"+blobKey)

	h := cluster(t)

	apis := []string{"s3", "azure"}

	namespaces := make([]string, 0, len(apis))
	for _, api := range apis {
		namespaces = append(namespaces, "fetch-"+api)
	}

	t.Cleanup(func() { h.dumpDiagnostics(namespaces...) })

	h.applyCache()
	h.installRacer()
	h.checkDataplaneHardware()
	h.rollout("daemonset", "racer-origin-"+cacheName, 5*time.Minute)
	h.waitWebhook()

	for i, api := range apis {
		ns := namespaces[i]
		h.kubectl("create", "namespace", ns)
		h.apply(fetchPod(ns, api))
	}

	for i, api := range apis {
		ns := namespaces[i]
		t.Logf("verifying %s API fetches in %s", api, ns)
		h.waitFetch(ns)
	}
}

// fetchPod returns a pod that reads the blob twice through the sidecar and
// verifies its digest each time.
// checkDataplaneHardware reads /debug/hardware from a racer pod
// through the API server pod proxy and checks the report shape. Kind nodes
// always expose block devices, but RDMA devices are not expected.
func (h *harness) checkDataplaneHardware() {
	h.t.Helper()

	pod := strings.TrimSpace(h.kubectl("-n", operatorNamespace, "get", "pods",
		"-l", "app.kubernetes.io/name=racer",
		"-o", "jsonpath={.items[0].metadata.name}"))
	if pod == "" {
		h.t.Fatal("no racer pod found")
	}

	port := strings.TrimSpace(h.kubectl("-n", operatorNamespace, "get", "pod", pod, "-o",
		`jsonpath={.spec.containers[?(@.name=="dataplane")].ports[?(@.name=="diagnostics")].containerPort}`))
	if port == "" {
		h.t.Fatalf("pod %s has no diagnostics containerPort", pod)
	}

	path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s:%s/proxy/debug/hardware", operatorNamespace, pod, port)

	var report map[string]json.RawMessage

	h.waitFor("dataplane /debug/hardware", 2*time.Minute, func() (bool, string) {
		out, err := h.exec(time.Minute, nil, "kubectl", "get", "--raw", path)
		if err != nil {
			return false, out
		}

		report = nil
		if err := json.Unmarshal([]byte(out), &report); err != nil {
			return false, fmt.Sprintf("decode: %v: %s", err, out)
		}

		return true, ""
	})

	for _, key := range []string{"block_devices", "rdma_devices", "racer", "errors"} {
		if _, ok := report[key]; !ok {
			h.t.Fatalf("/debug/hardware is missing key %q", key)
		}
	}

	var blockDevices []json.RawMessage
	if err := json.Unmarshal(report["block_devices"], &blockDevices); err != nil {
		h.t.Fatalf("block_devices is not an array: %v", err)
	}

	if len(blockDevices) == 0 {
		h.t.Fatal("block_devices is empty")
	}

	var racer map[string]json.RawMessage
	if err := json.Unmarshal(report["racer"], &racer); err != nil || racer == nil {
		h.t.Fatalf("racer is not a JSON object: %v: %s", err, report["racer"])
	}

	h.t.Logf("/debug/hardware on %s reported %d block devices", pod, len(blockDevices))
}

func fetchPod(ns, api string) string {
	url := fmt.Sprintf("http://127.0.0.1:8080/%s/%s", bucket, blobKey)
	script := fmt.Sprintf(`set -eu
for i in 1 2; do
  n=0
  until curl -fsS -o /tmp/blob %[1]s; do
    n=$((n+1)); [ "$n" -lt 60 ] || exit 1; sleep 2
  done
  echo "%[2]s  /tmp/blob" | sha256sum -c - && echo "fetch $i sha256-ok"
done`, url, blobSHA256)

	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: fetch
  namespace: %[1]s
  labels:
    app: fetch
    racer.unbounded-cloud.io/object-sidecar: enabled
  annotations:
    racer.unbounded-cloud.io/object-cache: %[2]s
    racer.unbounded-cloud.io/object-api: %[3]s
spec:
  restartPolicy: Never
  containers:
  - name: app
    image: curlimages/curl:8.11.1
    command: ["sh", "-c", %[4]q]
`, ns, cacheName, api, script)
}

// applyCache creates the ClusterCache with an anonymous Azure origin. It is
// idempotent so every test can call it.
func (h *harness) applyCache() {
	h.t.Helper()
	h.apply(fmt.Sprintf(`apiVersion: racer.unbounded-cloud.io/v1alpha1
kind: ClusterCache
metadata:
  name: %s
spec:
  origin:
    azure:
      containerURL: %s
`, cacheName, containerURL))
}

// waitFetch waits for the fetch pod in ns to succeed, then checks the sidecar
// was injected and both fetches verified.
func (h *harness) waitFetch(ns string) {
	h.t.Helper()
	h.waitFor("fetch pod in "+ns+" to succeed", 5*time.Minute, func() (bool, string) {
		out, _ := h.exec(time.Minute, nil, "kubectl", "-n", ns, "get", "pod", "fetch", "-o", "jsonpath={.status.phase}")

		phase := strings.TrimSpace(out)
		if phase == "Failed" {
			h.t.Fatalf("fetch pod in %s failed", ns)
		}

		return phase == "Succeeded", "phase=" + phase
	})

	injected := h.kubectl("-n", ns, "get", "pod", "fetch", "-o",
		`jsonpath={.metadata.annotations.racer\.unbounded-cloud\.io/object-sidecar-injected} {.spec.initContainers[*].name}`)
	if !strings.Contains(injected, "racer-object") {
		h.t.Fatalf("racer-object sidecar not injected: %q", injected)
	}

	logs := h.kubectl("-n", ns, "logs", "fetch", "-c", "app")
	if got := strings.Count(logs, "sha256-ok"); got != 2 {
		h.t.Fatalf("want 2 verified fetches, got %d; logs:\n%s", got, logs)
	}
}
