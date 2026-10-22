//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
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

	h := newHarness(t)
	h.setup()
	h.buildImages()
	h.install()

	apis := []string{"s3", "azure"}

	namespaces := make([]string, 0, len(apis))
	for _, api := range apis {
		namespaces = append(namespaces, "fetch-"+api)
	}

	t.Cleanup(func() { h.dumpDiagnostics(namespaces...) })

	h.apply(fmt.Sprintf(`apiVersion: racer.unbounded-cloud.io/v1alpha1
kind: ClusterCache
metadata:
  name: %s
spec:
  origin:
    azure:
      containerURL: %s
`, cacheName, containerURL))

	// Cap dataplane threads for kind: automatic sizing scales with host cores.
	// The operator seeds this ConfigMap once and preserves administrator edits,
	// rolling the DaemonSet on change.
	h.waitExists("configmap", "racer-dataplane-config", 5*time.Minute)
	h.kubectl("-n", operatorNamespace, "patch", "configmap", "racer-dataplane-config", "--type=merge",
		"-p", `{"data":{"RACER_MAX_THREADS":"2"}}`)

	h.rollout("deployment", "racer-controller", 5*time.Minute)
	h.rollout("daemonset", "racer-dataplane", 10*time.Minute)
	h.rollout("daemonset", "racer-origin-"+cacheName, 5*time.Minute)
	h.waitFor("sidecar injector webhook", 5*time.Minute, func() (bool, string) {
		out, err := h.exec(time.Minute, nil, "kubectl", "get", "mutatingwebhookconfiguration",
			"racer-object-sidecar-injector", "-o", "jsonpath={.webhooks[0].clientConfig.caBundle}")

		return err == nil && strings.TrimSpace(out) != "", out
	})

	for i, api := range apis {
		ns := namespaces[i]
		h.kubectl("create", "namespace", ns)
		h.apply(fetchPod(ns, api))
	}

	for i, api := range apis {
		ns := namespaces[i]
		t.Logf("verifying %s API fetches in %s", api, ns)
		h.waitFor("fetch pod in "+ns+" to succeed", 5*time.Minute, func() (bool, string) {
			out, _ := h.exec(time.Minute, nil, "kubectl", "-n", ns, "get", "pod", "fetch", "-o", "jsonpath={.status.phase}")

			phase := strings.TrimSpace(out)
			if phase == "Failed" {
				t.Fatalf("fetch pod in %s failed", ns)
			}

			return phase == "Succeeded", "phase=" + phase
		})

		injected := h.kubectl("-n", ns, "get", "pod", "fetch", "-o",
			`jsonpath={.metadata.annotations.racer\.unbounded-cloud\.io/object-sidecar-injected} {.spec.initContainers[*].name}`)
		if !strings.Contains(injected, "racer-object") {
			t.Fatalf("racer-object sidecar not injected: %q", injected)
		}

		logs := h.kubectl("-n", ns, "logs", "fetch", "-c", "app")
		if got := strings.Count(logs, "sha256-ok"); got != 2 {
			t.Fatalf("want 2 verified fetches, got %d; logs:\n%s", got, logs)
		}
	}
}

// fetchPod returns a pod that reads the blob twice through the sidecar and
// verifies its digest each time.
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
