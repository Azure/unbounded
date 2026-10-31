//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	gantrySite      = "gantry-racer-e2e"
	gantryCacheName = "gantry"
	gantryEnv       = "GANTRY_RACER_ENABLED"
	gantryGateLog   = "Racer sockets accept connections; mirror startup gate released"
	gantryRepo      = "unbounded/gantry"

	// upstreamRegistry is the local registry as Gantry pods see it on the
	// kind network. Racer mode requires a bare host[:port] registry name.
	upstreamRegistry = registryContainer + ":5000"

	manifestAccept = "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, " +
		"application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json"
)

// TestGantryRacerBackend checks the operator's Gantry Racer integration end to
// end: ClusterCache/gantry switches an operator-managed Gantry to the Racer
// backend, Gantry serves registry content through Racer by digest from its
// node-local mirror, repeat reads come from the cache rather than the
// upstream registry, and deleting the cache reverts Gantry.
func TestGantryRacerBackend(t *testing.T) {
	h := cluster(t)

	t.Cleanup(func() {
		h.dumpDiagnostics()
		h.try(2*time.Minute, "kubectl", "delete", "clustercache", gantryCacheName, "--ignore-not-found", "--wait=false")
		h.try(2*time.Minute, "kubectl", "delete", "site", gantrySite, "--ignore-not-found", "--wait=false")
	})

	// Gantry is a Site component: a Site that leaves it at its default
	// (enabled) installs it. The CIDRs match no kind node, and other optional
	// components default to disabled.
	h.apply(fmt.Sprintf(`apiVersion: unbounded-cloud.io/v1alpha3
kind: Site
metadata:
  name: %s
spec:
  nodeCidrs: ["10.250.0.0/16"]
  manageCniPlugin: false
  podCidrAssignments:
  - cidrBlocks: ["10.251.0.0/16"]
    assignmentEnabled: false
`, gantrySite))

	// Point Gantry at the local registry. The operator seeds gantry-config
	// once, preserves edits, and rolls the agent when the payload changes.
	h.waitExists("configmap", "gantry-config", 5*time.Minute)
	h.patchGantryUpstream()

	h.rollout("daemonset", "gantry", 5*time.Minute)

	if env := h.gantryEnv(); env != "" {
		t.Fatalf("Gantry without ClusterCache/%s has %s=%q, want unset", gantryCacheName, gantryEnv, env)
	}

	h.apply(fmt.Sprintf(`apiVersion: racer.unbounded-cloud.io/v1alpha1
kind: ClusterCache
metadata:
  name: %s
`, gantryCacheName))
	h.installRacer()

	h.waitFor("Gantry DaemonSet in Racer mode", 5*time.Minute, func() (bool, string) {
		env := h.gantryEnv()
		return env == "true", gantryEnv + "=" + env
	})
	h.checkGantryRacerSpec()
	h.rollout("daemonset", "gantry", 5*time.Minute)

	pods := h.gantryPods()
	for _, pod := range pods {
		h.waitFor(pod.name+" Racer startup gate", 2*time.Minute, func() (bool, string) {
			logs, err := h.exec(time.Minute, nil, "kubectl", "-n", operatorNamespace, "logs", pod.name, "-c", "gantry")
			return err == nil && strings.Contains(logs, gantryGateLog), "gate log not found"
		})
	}

	manifestDigest, manifest, blobDigest := h.registryImage(gantryRepo, imageTag)
	node := pods[0].node
	t.Logf("fetching %s@%s and blob %s through Gantry on %s", gantryRepo, manifestDigest, blobDigest, node)

	h.mirrorFetch(node, "manifests", manifestDigest, manifest.mediaType)
	h.mirrorFetch(node, "blobs", blobDigest, "")

	originGets := h.gantryMetricSum(node, pods, "gantry_racer_origin_requests_total", `method="GET"`)
	if originGets == 0 {
		t.Fatal("Gantry made no upstream GET for a cold Racer read")
	}

	served := h.gantryMetricSum(node, pods, "gantry_racer_mirror_requests_total", `status="200"`)

	h.mirrorFetch(node, "manifests", manifestDigest, manifest.mediaType)
	h.mirrorFetch(node, "blobs", blobDigest, "")

	if got := h.gantryMetricSum(node, pods, "gantry_racer_mirror_requests_total", `status="200"`); got < served+2 {
		t.Fatalf("Gantry mirror 200 responses = %v after repeat reads, want at least %v", got, served+2)
	}

	if got := h.gantryMetricSum(node, pods, "gantry_racer_origin_requests_total", `method="GET"`); got != originGets {
		t.Fatalf("upstream GETs went from %v to %v on repeat reads; want them served from Racer", originGets, got)
	}

	// A digest the configured registry does not have reaches the origin
	// through Racer, which reports not found.
	missing := "sha256:" + sha256Hex([]byte("gantry-racer-e2e-missing"))
	if status, id := h.mirrorStatus(node, "blobs", missing, upstreamRegistry); status != http.StatusNotFound || id == "" {
		t.Fatalf("missing blob: status %d, Gantry-Racer-Request-ID %q; want 404 with a request ID", status, id)
	}

	// An unconfigured registry is refused before Racer is consulted.
	if status, id := h.mirrorStatus(node, "blobs", blobDigest, "unconfigured.example"); status != http.StatusNotFound || id != "" {
		t.Fatalf("unconfigured registry: status %d, Gantry-Racer-Request-ID %q; want 404 without a request ID", status, id)
	}

	h.kubectl("delete", "clustercache", gantryCacheName, "--wait=true", "--timeout=5m")
	h.waitFor("Gantry DaemonSet to leave Racer mode", 5*time.Minute, func() (bool, string) {
		env := h.gantryEnv()
		return env == "", gantryEnv + "=" + env
	})
	h.rollout("daemonset", "gantry", 5*time.Minute)

	if volumes := h.kubectl("-n", operatorNamespace, "get", "daemonset", "gantry", "-o",
		"jsonpath={.spec.template.spec.volumes[*].name}"); strings.Contains(volumes, "racer-") {
		t.Fatalf("Gantry kept Racer volumes after ClusterCache deletion: %s", volumes)
	}
}

// patchGantryUpstream replaces the default upstream registry in gantry-config
// with the local registry, served over plain HTTP.
func (h *harness) patchGantryUpstream() {
	h.t.Helper()

	config := h.kubectl("-n", operatorNamespace, "get", "configmap", "gantry-config", "-o", `jsonpath={.data.config\.yaml}`)

	const defaultUpstream = `  - name: "registry.example.com"
    endpoint: "https://registry.example.com"`

	if !strings.Contains(config, defaultUpstream) {
		h.t.Fatalf("gantry-config does not list the default upstream registry:\n%s", config)
	}

	config = strings.Replace(config, defaultUpstream, fmt.Sprintf(`  - name: %q
    endpoint: "http://%s"`, upstreamRegistry, upstreamRegistry), 1)

	patch, err := json.Marshal(map[string]any{"data": map[string]string{"config.yaml": config}})
	if err != nil {
		h.t.Fatalf("marshal gantry-config patch: %v", err)
	}

	h.kubectl("-n", operatorNamespace, "patch", "configmap", "gantry-config", "--type=merge", "-p", string(patch))
}

// gantryEnv returns the GANTRY_RACER_ENABLED value on the gantry container.
func (h *harness) gantryEnv() string {
	h.t.Helper()

	return strings.TrimSpace(h.kubectl("-n", operatorNamespace, "get", "daemonset", "gantry", "-o",
		`jsonpath={.spec.template.spec.containers[?(@.name=="gantry")].env[?(@.name=="`+gantryEnv+`")].value}`))
}

// checkGantryRacerSpec verifies the socket mounts and that Gantry follows the
// dataplane's node placement, so it is the origin on every Racer node.
func (h *harness) checkGantryRacerSpec() {
	h.t.Helper()

	mounts := h.kubectl("-n", operatorNamespace, "get", "daemonset", "gantry", "-o",
		`jsonpath={range .spec.template.spec.volumes[*]}{.name}={.hostPath.path}:{.hostPath.type}{"\n"}{end}`)
	for _, want := range []string{
		"racer-client=/run/racer/gantry/client:Directory",
		"racer-origin=/run/racer/gantry/origin:Directory",
	} {
		if !strings.Contains(mounts, want) {
			h.t.Fatalf("Gantry volumes missing %s:\n%s", want, mounts)
		}
	}

	const placement = `jsonpath={.spec.template.spec.nodeSelector}|{.spec.template.spec.tolerations}|{.spec.template.spec.affinity.nodeAffinity}`

	gantry := h.kubectl("-n", operatorNamespace, "get", "daemonset", "gantry", "-o", placement)
	dataplane := h.kubectl("-n", operatorNamespace, "get", "daemonset", "racer", "-o", placement)

	if gantry != dataplane {
		h.t.Fatalf("Gantry placement does not follow racer:\ngantry:    %s\ndataplane: %s", gantry, dataplane)
	}
}

type gantryPod struct{ name, node, ip string }

// gantryPods lists the Gantry agent Pods once the DaemonSet has settled.
// A rollout can finish while an old Pod is still terminating, for example on
// a node that the Racer placement no longer selects, so wait until every
// listed Pod is live, has an IP, and the count matches the desired count.
func (h *harness) gantryPods() []gantryPod {
	h.t.Helper()

	var pods []gantryPod

	h.waitFor("Gantry Pods to settle", 2*time.Minute, func() (bool, string) {
		desired := strings.TrimSpace(h.kubectl("-n", operatorNamespace, "get", "daemonset", "gantry", "-o",
			"jsonpath={.status.desiredNumberScheduled}"))
		out := h.kubectl("-n", operatorNamespace, "get", "pods", "-l", "app.kubernetes.io/name=gantry", "-o",
			`jsonpath={range .items[*]}{.metadata.name} {.spec.nodeName} {.status.podIP} {.metadata.deletionTimestamp}{"\n"}{end}`)

		pods = nil

		for line := range strings.Lines(out) {
			fields := strings.Fields(line)
			if len(fields) != 3 {
				return false, "waiting for Pods without IPs or still terminating:\n" + out
			}

			pods = append(pods, gantryPod{name: fields[0], node: fields[1], ip: fields[2]})
		}

		if len(pods) == 0 || strconv.Itoa(len(pods)) != desired {
			return false, fmt.Sprintf("have %d Pods, want %s:\n%s", len(pods), desired, out)
		}

		return true, ""
	})

	return pods
}

type registryManifest struct {
	mediaType string
	body      []byte
}

// registryImage resolves repo:tag in the local registry to an image manifest
// digest, following an index to its first image, and returns the smallest
// layer digest as the blob to fetch.
func (h *harness) registryImage(repo, tag string) (string, registryManifest, string) {
	h.t.Helper()

	digest, manifest := h.registryManifest(repo, tag)

	var doc struct {
		Manifests []struct {
			Digest string `json:"digest"`
		} `json:"manifests"`
		Layers []struct {
			Digest string `json:"digest"`
			Size   int64  `json:"size"`
		} `json:"layers"`
	}

	if err := json.Unmarshal(manifest.body, &doc); err != nil {
		h.t.Fatalf("decode %s@%s: %v", repo, digest, err)
	}

	if len(doc.Manifests) > 0 {
		return h.registryImage(repo, doc.Manifests[0].Digest)
	}

	if len(doc.Layers) == 0 {
		h.t.Fatalf("%s@%s has no layers:\n%s", repo, digest, manifest.body)
	}

	blob := doc.Layers[0]
	for _, layer := range doc.Layers[1:] {
		if layer.Size < blob.Size {
			blob = layer
		}
	}

	return digest, manifest, blob.Digest
}

func (h *harness) registryManifest(repo, ref string) (string, registryManifest) {
	h.t.Helper()

	url := fmt.Sprintf("http://127.0.0.1:%s/v2/%s/manifests/%s", h.registryPort, repo, ref)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		h.t.Fatalf("new request: %v", err)
	}

	req.Header.Set("Accept", manifestAccept)

	resp, err := (&http.Client{Timeout: time.Minute}).Do(req)
	if err != nil {
		h.t.Fatalf("GET %s: %v", url, err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		h.t.Fatalf("GET %s: status %s, err %v\n%s", url, resp.Status, err, body)
	}

	return "sha256:" + sha256Hex(body), registryManifest{mediaType: resp.Header.Get("Content-Type"), body: body}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// mirrorURL is a Gantry mirror URL on the node-local hostPort.
func mirrorURL(kind, digest, registry string) string {
	return fmt.Sprintf("http://127.0.0.1:5000/v2/%s/%s/%s?ns=%s", gantryRepo, kind, digest, registry)
}

// mirrorFetch reads a digest through the Gantry mirror on node and verifies
// the content digest on the node.
func (h *harness) mirrorFetch(node, kind, digest, accept string) {
	h.t.Helper()

	header := ""
	if accept != "" {
		header = fmt.Sprintf("-H 'Accept: %s'", accept)
	}

	script := fmt.Sprintf(`set -eu
curl -fsS --max-time 120 %s -o /tmp/gantry-racer-e2e '%s'
sha256sum /tmp/gantry-racer-e2e | cut -d' ' -f1
rm -f /tmp/gantry-racer-e2e`, header, mirrorURL(kind, digest, upstreamRegistry))

	var got string

	h.waitFor(fmt.Sprintf("%s %s through Gantry on %s", kind, digest, node), 3*time.Minute, func() (bool, string) {
		out, err := h.exec(3*time.Minute, nil, "docker", "exec", node, "sh", "-c", script)
		got = strings.TrimSpace(out)

		return err == nil, got
	})

	if want := strings.TrimPrefix(digest, "sha256:"); got != want {
		h.t.Fatalf("%s %s through Gantry has digest sha256:%s", kind, digest, got)
	}
}

// mirrorStatus returns the HTTP status and Gantry-Racer-Request-ID header of
// a Gantry mirror read on node.
func (h *harness) mirrorStatus(node, kind, digest, registry string) (int, string) {
	h.t.Helper()

	out := h.run(3*time.Minute, "docker", "exec", node, "curl", "-sS", "--max-time", "120", "-o", "/dev/null",
		"-D", "-", mirrorURL(kind, digest, registry))

	status := 0
	id := ""

	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if fields := strings.Fields(line); len(fields) >= 2 && strings.HasPrefix(fields[0], "HTTP/") {
			status, _ = strconv.Atoi(fields[1])
		}

		if name, value, ok := strings.Cut(line, ":"); ok && strings.EqualFold(name, "Gantry-Racer-Request-ID") {
			id = strings.TrimSpace(value)
		}
	}

	return status, id
}

// gantryMetricSum sums a counter across every Gantry Pod's metrics endpoint,
// scraped from node over the Pod network. Upstream reads happen on whichever
// node Racer picks as origin, so origin counters must be summed cluster-wide.
func (h *harness) gantryMetricSum(node string, pods []gantryPod, name, label string) float64 {
	h.t.Helper()

	var total float64

	for _, pod := range pods {
		out := h.run(time.Minute, "docker", "exec", node, "curl", "-fsS", "--max-time", "30",
			fmt.Sprintf("http://%s:9095/metrics", pod.ip))

		for line := range strings.Lines(out) {
			series, value, ok := strings.Cut(strings.TrimSpace(line), " ")
			if !ok || !strings.HasPrefix(series, name+"{") || !strings.Contains(series, label) {
				continue
			}

			v, err := strconv.ParseFloat(value, 64)
			if err != nil {
				h.t.Fatalf("parse %s from %s: %v", line, pod.name, err)
			}

			total += v
		}
	}

	return total
}
