//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	writeCacheName = "e2e-writes"
	writeNamespace = "racer-writes"
	// writeObjectSize spans three 16 MiB pages, the last one short.
	writeObjectSize = 40 << 20
	probePath       = "/writeprobe"
	// Probe exit codes, from e2e/racer/writeprobe.
	probeExitExists   = 3
	probeExitNotFound = 4
)

// TestWritesNeverUpload covers cache-only writes on a ClusterCache without an
// origin: a multi-page PUT acknowledged after two durable copies, a read on
// another node, create-once, survival of a deleted Racer pod, and DELETE.
func TestWritesNeverUpload(t *testing.T) {
	h := cluster(t)

	t.Cleanup(func() { h.dumpDiagnostics(writeNamespace) })

	h.apply(fmt.Sprintf(`apiVersion: racer.unbounded-cloud.io/v1alpha1
kind: ClusterCache
metadata:
  name: %s
spec:
  writes:
    replicas: 2
    upload:
      mode: Never
`, writeCacheName))
	h.installRacer()

	nodes := h.racerNodes()
	if len(nodes) < 3 {
		t.Fatalf("write e2e needs Racer on at least 3 nodes, got %v", nodes)
	}

	h.try(time.Minute, "kubectl", "create", "namespace", writeNamespace)

	probe := h.buildWriteProbe()
	clients := make(map[string]string, len(nodes))

	for _, node := range nodes[:3] {
		clients[node] = h.startProbePod(node, writeCacheName, probe)
	}

	writer, reader, other := clients[nodes[0]], clients[nodes[1]], clients[nodes[2]]
	size := fmt.Sprint(writeObjectSize)

	t.Run("put and read on another node", func(t *testing.T) {
		key := writeKey("multi-page")
		out := h.probe(writer, 0, "put", writeCacheName, key, size, "1", "2", "never")

		if !strings.Contains(out, "state=never") {
			t.Fatalf("put upload state: %s", out)
		}

		h.probe(reader, 0, "get", writeCacheName, key, size, "1")
		h.probe(writer, 0, "get", writeCacheName, key, size, "1")
	})

	t.Run("create once", func(t *testing.T) {
		key := writeKey("create-once")
		h.probe(writer, 0, "put", writeCacheName, key, "4096", "2", "2", "never")
		// A second PUT from any node, even with different bytes, is 412.
		h.probe(reader, probeExitExists, "put", writeCacheName, key, "4096", "3", "2", "never")
		h.probe(other, 0, "get", writeCacheName, key, "4096", "2")
	})

	t.Run("delete", func(t *testing.T) {
		key := writeKey("delete")
		h.probe(writer, 0, "put", writeCacheName, key, size, "4", "2", "never")
		h.probe(reader, 0, "get", writeCacheName, key, size, "4")
		h.probe(other, 0, "delete", writeCacheName, key)

		for _, pod := range []string{writer, reader, other} {
			h.waitProbe(pod, probeExitNotFound, "get", writeCacheName, key, size, "4")
		}

		h.probe(writer, probeExitNotFound, "delete", writeCacheName, key)
	})

	t.Run("survives a deleted racer pod", func(t *testing.T) {
		// With replicas=2 any single lost holder leaves a durable copy, so
		// deleting the writer node's Racer pod must not lose the object.
		key := writeKey("durable")
		h.probe(writer, 0, "put", writeCacheName, key, size, "5", "2", "never")

		victim := h.racerPodOn(nodes[0])
		h.kubectl("-n", operatorNamespace, "delete", "pod", victim, "--wait=true", "--timeout=2m")
		h.rollout("daemonset", "racer", 10*time.Minute)

		h.waitProbe(reader, 0, "get", writeCacheName, key, size, "5")
		h.waitProbe(other, 0, "get", writeCacheName, key, size, "5")
		// The restarted node rejoins and reads it too.
		h.waitProbe(writer, 0, "get", writeCacheName, key, size, "5")
	})
}

// TestWritesUploadToOrigin is the auto and persist upload flow. The harness
// origin is a public, anonymous, read-only Azure container, and the suite has
// no Azure Blob emulator (such as Azurite) that Racer's Azure origin can
// reach with write credentials, so there is nothing to upload to.
func TestWritesUploadToOrigin(t *testing.T) {
	t.Skip("no writable Azure-compatible origin in the kind harness: the shared origin is a " +
		"public read-only container and there is no Azurite deployment; upload is covered by the " +
		"WP6 transport tests")
}

// writeKey derives a stable 64-hex cache key for name.
func writeKey(name string) string {
	sum := sha256.Sum256([]byte("racer-writes-e2e/" + name))
	return hex.EncodeToString(sum[:])
}

// racerNodes returns the sorted nodes running a Racer dataplane pod.
func (h *harness) racerNodes() []string {
	h.t.Helper()

	out := h.kubectl("-n", operatorNamespace, "get", "pods", "-l", dataplaneLabel,
		"-o", `jsonpath={range .items[*]}{.spec.nodeName}{"\n"}{end}`)
	nodes := strings.Fields(out)
	sort.Strings(nodes)

	return nodes
}

// racerPodOn returns the Racer dataplane pod on node.
func (h *harness) racerPodOn(node string) string {
	h.t.Helper()

	pod := strings.TrimSpace(h.kubectl("-n", operatorNamespace, "get", "pods", "-l", dataplaneLabel,
		"--field-selector", "spec.nodeName="+node, "-o", "jsonpath={.items[0].metadata.name}"))
	if pod == "" {
		h.t.Fatalf("no racer pod on %s", node)
	}

	return pod
}

// buildWriteProbe cross-compiles e2e/racer/writeprobe for the kind nodes and
// returns the binary.
func (h *harness) buildWriteProbe() []byte {
	h.t.Helper()

	out := filepath.Join(h.t.TempDir(), "writeprobe")

	cmd := exec.Command("go", "build", "-tags", "e2e", "-trimpath", "-o", out, "./e2e/racer/writeprobe")
	cmd.Dir = h.repoRoot

	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")

	if msg, err := cmd.CombinedOutput(); err != nil {
		h.t.Fatalf("build writeprobe: %v\n%s", err, msg)
	}

	bin, err := os.ReadFile(out)
	if err != nil {
		h.t.Fatal(err)
	}

	return bin
}

// startProbePod runs a pod on node that mounts cache's client socket
// directory, copies the probe into it, and waits for the socket.
func (h *harness) startProbePod(node, cache string, probe []byte) string {
	h.t.Helper()

	name := "writeprobe-" + node
	dir := "/run/racer/" + cache + "/client"
	h.apply(fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  nodeName: %[3]s
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  containers:
  - name: probe
    image: busybox:1.36
    command: ["sleep", "infinity"]
    volumeMounts:
    - name: client
      mountPath: %[4]s
  volumes:
  - name: client
    hostPath:
      path: %[4]s
      type: DirectoryOrCreate
`, name, writeNamespace, node, dir))
	h.kubectl("-n", writeNamespace, "wait", "--for=condition=Ready", "pod/"+name, "--timeout=3m")

	out, err := h.exec(2*time.Minute, probe, "kubectl", "-n", writeNamespace, "exec", "-i", name, "--",
		"sh", "-c", "cat > "+probePath+" && chmod 0755 "+probePath)
	if err != nil {
		h.t.Fatalf("copy writeprobe to %s: %v\n%s", name, err, out)
	}

	h.waitFor("cache socket in "+name, 5*time.Minute, func() (bool, string) {
		out, err := h.exec(time.Minute, nil, "kubectl", "-n", writeNamespace, "exec", name, "--",
			"test", "-S", dir+"/socket")

		return err == nil, out
	})

	return name
}

// runProbe runs the probe in pod and returns its output and exit code.
func (h *harness) runProbe(pod string, args ...string) (string, int) {
	h.t.Helper()

	argv := append([]string{"-n", writeNamespace, "exec", pod, "--", probePath}, args...)
	out, err := h.exec(5*time.Minute, nil, "kubectl", argv...)

	// kubectl exec propagates the remote exit status.
	if exitErr := (*exec.ExitError)(nil); errors.As(err, &exitErr) {
		return out, exitErr.ExitCode()
	} else if err != nil {
		h.t.Fatalf("writeprobe %v in %s: %v\n%s", args, pod, err, out)
	}

	return out, 0
}

// probe runs the probe and fails unless it exits with want.
func (h *harness) probe(pod string, want int, args ...string) string {
	h.t.Helper()

	out, code := h.runProbe(pod, args...)
	if code != want {
		h.t.Fatalf("writeprobe %v in %s: exit %d, want %d\n%s", args, pod, code, want, out)
	}

	h.t.Logf("writeprobe %s in %s: %s", args[0], pod, strings.TrimSpace(out))

	return out
}

// waitProbe retries the probe until it exits with want, for steps that race
// membership changes after a pod restart or a delete.
func (h *harness) waitProbe(pod string, want int, args ...string) {
	h.t.Helper()
	h.waitFor(fmt.Sprintf("writeprobe %v in %s to exit %d", args, pod, want), 5*time.Minute, func() (bool, string) {
		out, code := h.runProbe(pod, args...)
		return code == want, fmt.Sprintf("exit %d: %s", code, out)
	})
}
