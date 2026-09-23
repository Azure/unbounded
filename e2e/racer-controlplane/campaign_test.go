//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controlplane_test

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	originfixture "github.com/Azure/unbounded/e2e/racer/fixture"
	sdk "github.com/Azure/unbounded/pkg/racer"
)

type dataplane struct {
	node                      *corev1.Node
	pod                       *corev1.Pod
	ip, dir, sockets, metrics string
	process                   *process
	client                    *sdk.Client
}

type localStatus struct {
	Ready bool
	TLS   struct {
		Generation                uint64
		Issuer                    string
		InstalledWorkers, Workers int
		Error                     *string
	}
	Storage struct {
		Phase                                string
		AppliedBytes, AppliedVersion, Shards uint64
		Boot                                 string
	}
}

type nodeStatus struct {
	Phase, PolicyPhase, PolicyIdentity, Boot, Error                     string
	ValidationError                                                     *string
	PolicyVersion, AppliedVersion, AppliedBytes, EffectiveBytes, Shards uint64
	Fresh                                                               bool
}

type bundle struct {
	Generation   uint64 `json:"generation"`
	Active       string `json:"active"`
	Certificates string `json:"certificates"`
}

func (c *campaign) worker(index int, site string) *dataplane {
	t := c.t
	ip := fmt.Sprintf("192.0.2.%d", 30+index)
	node, err := c.kube.CoreV1().Nodes().Create(c.ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("worker-%d", index), Labels: map[string]string{"unbounded-cloud.io/site": site, "kubernetes.io/os": "linux"}, Annotations: map[string]string{prefix + "cache-size": "64Mi"}}}, metav1.CreateOptions{})
	require(t, err)

	node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	node, err = c.kube.CoreV1().Nodes().UpdateStatus(c.ctx, node, metav1.UpdateOptions{})
	require(t, err)
	s, err := c.dynamic.Resource(siteResource).Get(c.ctx, site, metav1.GetOptions{})
	require(t, err)

	labels := map[string]string{prefix + "dataplane": "true", prefix + "universe": site}
	template := corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{ServiceAccountName: "racer-dataplane", Containers: []corev1.Container{{Name: "dataplane", Image: "fixture"}}}}
	ds, err := c.kube.AppsV1().DaemonSets(namespace).Create(c.ctx, &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Labels: map[string]string{prefix + "component": "racer-dataplane"}, OwnerReferences: []metav1.OwnerReference{owner("unbounded-cloud.io/v1alpha3", "Site", site, s.GetUID())}}, Spec: appsv1.DaemonSetSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}, Template: template}}, metav1.CreateOptions{})
	require(t, err)

	template.Spec.NodeName = node.Name
	pod, err := c.kube.CoreV1().Pods(namespace).Create(c.ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Labels: labels, OwnerReferences: []metav1.OwnerReference{owner("apps/v1", "DaemonSet", ds.Name, ds.UID)}}, Spec: template.Spec}, metav1.CreateOptions{})
	require(t, err)

	pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, PodIP: ip, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
	pod, err = c.kube.CoreV1().Pods(namespace).UpdateStatus(c.ctx, pod, metav1.UpdateOptions{})
	require(t, err)

	dir := filepath.Join(c.dir, node.Name)
	require(t, os.MkdirAll(dir, 0o700))
	// Keep host-side UDS names short too, irrespective of Go's test temp name.
	sockets, err := os.MkdirTemp(filepath.Dir(c.socketRoot), "dp-")
	require(t, err)
	t.Cleanup(func() { require(t, os.RemoveAll(sockets)) })
	d := &dataplane{node: node, pod: pod, ip: ip, dir: dir, sockets: sockets, metrics: net.JoinHostPort(ip, port(t))}

	return d
}

// A byte-for-byte TCP relay stands in for kube-proxy's Service routing only.
// All TLS handshakes and HTTP handling terminate in the production binaries.
func (c *campaign) relay(target *atomic.Pointer[replica], endpoint func(*replica) string) string {
	c.t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require(c.t, err)
	c.t.Cleanup(func() { _ = l.Close() })

	go func() {
		for {
			down, err := l.Accept()
			if err != nil {
				return
			}

			go func() {
				defer down.Close()

				r := target.Load()
				if r == nil {
					return
				}

				up, err := net.DialTimeout("tcp", endpoint(r), time.Second)
				if err != nil {
					return
				}
				defer up.Close()

				done := make(chan struct{})

				go func() { _, _ = io.Copy(up, down); _ = up.Close(); close(done) }()

				_, _ = io.Copy(down, up)
				_ = down.Close()

				<-done
			}()
		}
	}()

	return l.Addr().String()
}

func (c *campaign) startDataplane(d *dataplane, control, enroll, proof, creationSize string) {
	t := c.t
	require(t, os.WriteFile(filepath.Join(d.dir, "token"), []byte(c.token(d.pod)), 0o600))
	env := []string{"RACER_CONTROL_PLANE_URL=https://" + control + "/v4/config", "RACER_UNIVERSE=" + identity("universe", d.node.Labels["unbounded-cloud.io/site"]), "RACER_NODE=" + identity("node", string(d.node.UID)), "RACER_TLS_TRUST_DIR=" + d.dir, "RACER_CONTROL_TOKEN_FILE=" + filepath.Join(d.dir, "token"), "RACER_ENROLL_URL=https://" + enroll + "/v3/enroll", "RACER_TRUST_PROOF_URL=https://" + proof + "/v3/proof", "RACER_CONTROL_SERVER_NAME=racer-controlplane." + namespace + ".svc", "RACER_POD_NAMESPACE=" + namespace, "RACER_POD_NAME=" + d.pod.Name, "RACER_POD_UID=" + string(d.pod.UID), "RACER_POD_IP=" + d.ip, "RACER_SLAB_PATH=" + filepath.Join(d.dir, "cache.slab"), "RACER_SLAB_SIZE=" + creationSize, "RACER_SHARDS=1", "RACER_IO_WORKERS=1", "RACER_COMPUTE_WORKERS=1", "RACER_BUFFERS_PER_NODE=8", "RACER_RDMA_MODE=disabled", "RACER_METRICS_ADDR=" + d.metrics, "RACER_SLAB_IOPS=100", "RACER_SLAB_IO_BURST=1"}
	args := []string{"-n", "unshare", "--mount", "sh", "-ec", `mount --make-rprivate /; mount --bind "$1" "$2"; shift 2; exec "$@"`, "racer-live", d.sockets, c.socketRoot, "setpriv", "--reuid", strconv.Itoa(os.Getuid()), "--regid", strconv.Itoa(os.Getgid()), "--clear-groups", "env"}
	args = append(args, env...)
	args = append(args, "RACER_HTTP_DIAGNOSTICS=1")
	args = append(args, c.dpBinary)
	cmd := exec.Command("sudo", args...)
	cmd.Env = cleanEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	d.process = c.start(d.node.Name, cmd)
	d.process.privileged = true
}

func (c *campaign) trust() (bundle, []byte, error) {
	cm, err := c.kube.CoreV1().ConfigMaps(namespace).Get(c.ctx, "racer-trust", metav1.GetOptions{})
	if err != nil {
		return bundle{}, nil, err
	}

	raw := []byte(cm.Data["bundle.json"])

	var b bundle

	err = json.Unmarshal(raw, &b)

	return b, raw, err
}

func (c *campaign) projectTrust(workers ...*dataplane) func() {
	stop := make(chan struct{})
	done := make(chan struct{})

	var failure atomic.Pointer[error]

	go func() {
		defer close(done)

		var prior string

		for {
			_, raw, err := c.trust()
			if err == nil && string(raw) != prior {
				for _, d := range workers {
					err = os.WriteFile(filepath.Join(d.dir, "bundle.next"), raw, 0o600)
					if err == nil {
						err = os.Rename(filepath.Join(d.dir, "bundle.next"), filepath.Join(d.dir, "bundle.json"))
					}

					if err != nil {
						failure.Store(&err)
						return
					}
				}

				prior = string(raw)
			}

			select {
			case <-stop:
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}()

	return func() {
		close(stop)
		<-done

		if err := failure.Load(); err != nil {
			c.t.Error(*err)
		}
	}
}

func (c *campaign) storage(d *dataplane, phase string, size uint64) (localStatus, nodeStatus) {
	c.t.Helper()

	var (
		local    localStatus
		observed nodeStatus
	)

	c.await(d.node.Name+" storage "+phase, 60*time.Second, func() error {
		if err := c.getJSON("http://"+d.metrics+"/status", &local); err != nil {
			return err
		}

		n, err := c.kube.CoreV1().Nodes().Get(c.ctx, d.node.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		if err = json.Unmarshal([]byte(n.Annotations[prefix+"cache-status"]), &observed); err != nil {
			return err
		}

		if local.Storage.Phase != phase || local.Storage.AppliedBytes != size || observed.PolicyPhase != phase || observed.AppliedBytes != size || !observed.Fresh || observed.Boot != local.Storage.Boot {
			return fmt.Errorf("local=%+v node=%+v", local, observed)
		}

		return nil
	})

	return local, observed
}

func (c *campaign) size(d *dataplane, value string) {
	c.t.Helper()

	raw, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{prefix + "cache-size": value}}})
	_, err := c.kube.CoreV1().Nodes().Patch(c.ctx, d.node.Name, types.MergePatchType, raw, metav1.PatchOptions{})
	require(c.t, err)
}

func (c *campaign) cacheReady(name string, desired, ready int64) error {
	v, err := c.dynamic.Resource(cacheResource).Get(c.ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	d, _, _ := unstructured.NestedInt64(v.Object, "status", "participants", "desired")

	r, _, _ := unstructured.NestedInt64(v.Object, "status", "participants", "ready")
	if d != desired || r != ready {
		return fmt.Errorf("cache %s participants desired=%d ready=%d status=%v", name, d, r, v.Object["status"])
	}

	return nil
}

func TestProductionBinaryCampaign(t *testing.T) {
	c := newCampaign(t)
	for _, site := range []string{"edge", "independent"} {
		c.create(siteResource, map[string]any{"apiVersion": "unbounded-cloud.io/v1alpha3", "kind": "Site", "metadata": map[string]any{"name": site, "labels": map[string]any{"campaign": site}}, "spec": map[string]any{"nodeCidrs": []any{"10.0.0.0/16"}, "podCidrAssignments": []any{map[string]any{"cidrBlocks": []any{"10.1.0.0/16"}}}, "components": map[string]any{"racer": map[string]any{"enabled": true, "cacheSize": "64Mi"}}}})
		c.create(cacheResource, map[string]any{"apiVersion": "racer.unbounded-cloud.io/v1alpha1", "kind": "P2PCache", "metadata": map[string]any{"name": site}, "spec": map[string]any{"siteSelector": map[string]any{"matchLabels": map[string]any{"campaign": site}}}})
	}

	a, b := c.worker(0, "edge"), c.worker(1, "edge")
	independent := c.worker(2, "independent")

	workers := []*dataplane{a, b, independent}
	for _, d := range workers {
		name := d.node.Labels["unbounded-cloud.io/site"]
		require(t, os.MkdirAll(filepath.Join(d.sockets, name), 0o700))
		l, err := net.Listen("unix", filepath.Join(d.sockets, name, "origin"))
		require(t, err)

		backend := originfixture.NewOrigin()
		backend.Source = d.node.Name
		origin := httptest.NewUnstartedServer(backend)
		_ = origin.Listener.Close()
		origin.Listener = l
		origin.Start()
		t.Cleanup(origin.Close)

		d.client, err = sdk.NewClient(filepath.Join(d.sockets, name, "cache"), sdk.ClientOptions{})
		require(t, err)
		t.Cleanup(d.client.CloseIdleConnections)
	}

	for _, r := range c.replicas {
		c.startReplica(r)
	}

	var leader *replica

	c.await("two CP processes elect one leader", 60*time.Second, func() error { var err error; leader, err = c.leader(); return err })

	var target atomic.Pointer[replica]
	target.Store(leader)
	control := c.relay(&target, func(r *replica) string { return r.control })
	enroll := c.relay(&target, func(r *replica) string { return r.enroll })
	proof := c.relay(&target, func(r *replica) string { return r.trust })
	initial, _, err := c.trust()
	require(t, err)

	stopProjection := c.projectTrust(workers...)
	defer stopProjection()

	c.await("trust projection", 5*time.Second, func() error { _, err := os.Stat(filepath.Join(a.dir, "bundle.json")); return err })
	// Exercise the actual bootstrap subcommand against API Node/Service data.
	cmd := exec.Command(c.cpBinary, "--bootstrap-node", a.node.Name, "--bootstrap-universe", "edge", "--bootstrap-namespace", namespace)
	cmd.Env = append(cleanEnv(), "KUBECONFIG="+c.kubeconfig, "POD_IP=10.1.0.2")
	out, err := cmd.CombinedOutput()
	require(t, err)

	if !strings.Contains(string(out), identity("node", string(a.node.UID))) || !strings.Contains(string(out), identity("universe", "edge")) {
		t.Fatalf("bootstrap identity mismatch: %s", out)
	}
	// Certificate-free callers cannot reach the mutual-TLS control endpoint.
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(initial.Certificates)) {
		t.Fatal("invalid public trust")
	}

	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "racer-controlplane." + namespace + ".svc", MinVersion: tls.VersionTLS13}}

	anonymous := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	defer anonymous.CloseIdleConnections()

	response, err := anonymous.Get("https://" + control + "/v4/config")
	if err == nil {
		response.Body.Close()
		t.Fatalf("control accepted missing client certificate: %d", response.StatusCode)
	}
	// One selected daemon is deliberately absent. Its peer and a separate universe
	// must still install desired state; readiness must not claim both are converged.
	c.startDataplane(a, control, enroll, proof, "67108864")
	c.startDataplane(independent, control, enroll, proof, "67108864")
	c.storage(a, "applied", 64<<20)
	c.storage(independent, "applied", 64<<20)
	c.await("independent convergence while selected peer is absent", 60*time.Second, func() error {
		if err := c.cacheReady("edge", 2, 1); err != nil {
			return err
		}

		return c.cacheReady("independent", 1, 1)
	})
	c.startDataplane(b, control, enroll, proof, "67108864")
	c.storage(b, "applied", 64<<20)
	c.await("all edge participants converged", 60*time.Second, func() error { return c.cacheReady("edge", 2, 2) })

	topologyKey := "racer-v4-topology-" + identity("universe", "edge")
	topology, err := c.kube.CoreV1().ConfigMaps(namespace).Get(c.ctx, topologyKey, metav1.GetOptions{})
	require(t, err)

	first, policy := c.storage(a, "applied", 64<<20)
	stat := func() os.FileInfo {
		t.Helper()

		v, err := os.Stat(filepath.Join(a.dir, "cache.slab"))
		require(t, err)

		return v
	}
	inode := stat()

	for _, step := range []struct {
		quantity      string
		bytes, shards uint64
	}{{"20Gi", 20 << 30, 2}, {"96Mi", 96 << 20, 1}} {
		c.size(a, step.quantity)

		local, status := c.storage(a, "applied", step.bytes)
		if local.Storage.Boot != first.Storage.Boot || local.Storage.Shards != step.shards || status.PolicyIdentity != policy.PolicyIdentity || status.AppliedVersion != status.PolicyVersion || os.SameFile(inode, stat()) || stat().Size() != int64(step.bytes) {
			t.Fatalf("resize lost process/policy identity or inode replacement: %+v %+v", local, status)
		}

		inode = stat()
	}

	c.size(a, "5Ti")

	_, failed := c.storage(a, "failed", 96<<20)
	if failed.Error == "" || failed.EffectiveBytes != 5<<40 || !os.SameFile(inode, stat()) {
		t.Fatalf("runtime rejection lost last good storage: %+v", failed)
	}

	c.size(a, "invalid")
	c.await("invalid intent retains committed storage version", 30*time.Second, func() error {
		n, err := c.kube.CoreV1().Nodes().Get(c.ctx, a.node.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		var s nodeStatus
		if err = json.Unmarshal([]byte(n.Annotations[prefix+"cache-status"]), &s); err != nil {
			return err
		}

		if s.Phase != "invalid" || s.ValidationError == nil || s.PolicyVersion != failed.PolicyVersion || s.AppliedBytes != 96<<20 {
			return fmt.Errorf("invalid status: %+v", s)
		}

		return nil
	})
	c.size(a, "96Mi")
	_, applied := c.storage(a, "applied", 96<<20)
	c.size(a, "100663296")

	_, equivalent := c.storage(a, "applied", 96<<20)
	if equivalent.PolicyVersion != applied.PolicyVersion || !os.SameFile(inode, stat()) {
		t.Fatal("equivalent storage quantity reset policy or inode")
	}

	current, err := c.kube.CoreV1().ConfigMaps(namespace).Get(c.ctx, topologyKey, metav1.GetOptions{})
	require(t, err)

	if current.Data["pointer"] != topology.Data["pointer"] {
		t.Fatal("storage-only changes mutated topology")
	}
	// CAS evidence uses the actual API: preserve an old pointer, change topology
	// through a watch, then prove the old resourceVersion cannot overwrite it.
	c.patch(cacheResource, "edge", `{"spec":{"cacheGeneration":2}}`)
	c.await("real watch publishes changed topology", 60*time.Second, func() error {
		v, err := c.kube.CoreV1().ConfigMaps(namespace).Get(c.ctx, topologyKey, metav1.GetOptions{})
		if err != nil {
			return err
		}

		if v.Data["pointer"] == topology.Data["pointer"] {
			return fmt.Errorf("pointer unchanged")
		}

		var volumes []struct {
			CacheGeneration int64 `json:"cache_generation"`
		}
		if err := json.Unmarshal(c.record(v)["volumes"], &volumes); err != nil {
			return err
		}

		if len(volumes) != 1 || volumes[0].CacheGeneration != 2 {
			return fmt.Errorf("watched cache generation not yet committed: %+v", volumes)
		}

		return c.cacheReady("edge", 2, 2)
	})

	_, err = c.kube.CoreV1().ConfigMaps(namespace).Update(c.ctx, topology, metav1.UpdateOptions{})
	if !apierrors.IsConflict(err) {
		t.Fatalf("stale topology CAS: %v", err)
	}

	before, err := c.kube.CoreV1().ConfigMaps(namespace).Get(c.ctx, topologyKey, metav1.GetOptions{})
	require(t, err)
	// Kill the elected process without releasing its Lease. The warm replica must
	// wait out the real Lease, claim durable state, and re-observe daemon feedback.
	leader.process.stop(true)
	c.await("crash failover via Lease expiry", 75*time.Second, func() error {
		r, err := c.leader()
		if err != nil {
			return err
		}

		if r == leader {
			return fmt.Errorf("old route still selected")
		}

		target.Store(r)

		return nil
	})
	c.await("failover reacknowledges topology", 60*time.Second, func() error { return c.cacheReady("edge", 2, 2) })
	after, err := c.kube.CoreV1().ConfigMaps(namespace).Get(c.ctx, topologyKey, metav1.GetOptions{})
	require(t, err)

	var oldPointer, newPointer struct{ Digest, Fence string }
	require(t, json.Unmarshal([]byte(before.Data["pointer"]), &oldPointer))
	require(t, json.Unmarshal([]byte(after.Data["pointer"]), &newPointer))

	if oldPointer.Digest != newPointer.Digest || oldPointer.Fence == newPointer.Fence {
		oldRecord, newRecord := c.record(before), c.record(after)
		for key, value := range oldRecord {
			if string(value) != string(newRecord[key]) {
				t.Logf("changed persisted field %s: before=%s after=%s", key, summary(value), summary(newRecord[key]))
			}
		}

		t.Errorf("failover changed durable content or failed to fence: %+v %+v", oldPointer, newPointer)
	}

	c.startReplica(leader)
	c.await("restarted CP requests Pod replacement for old boot", 30*time.Second, func() error {
		_, err := c.kube.CoreV1().Pods(namespace).Get(c.ctx, leader.pod.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}

		return fmt.Errorf("old Pod still present: %v", err)
	})
	leader.process.stop(false)
	// Envtest has no ReplicaSet controller. Supply its replacement Pod, with a
	// fresh API-assigned UID, only after the production CP deletes the old boot.
	oldPod := leader.pod
	pod, err := c.kube.CoreV1().Pods(namespace).Create(c.ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: oldPod.Name, Labels: oldPod.Labels, OwnerReferences: oldPod.OwnerReferences}, Spec: oldPod.Spec}, metav1.CreateOptions{})
	require(t, err)

	pod.Status = oldPod.Status
	leader.pod, err = c.kube.CoreV1().Pods(namespace).UpdateStatus(c.ctx, pod, metav1.UpdateOptions{})
	require(t, err)
	c.startReplica(leader)
	c.await("restarted CP installs fresh replica key", 45*time.Second, func() error {
		r, err := c.http.Get("http://" + leader.health + "/readyz")
		if err != nil {
			return err
		}
		defer r.Body.Close()

		if r.StatusCode != 200 {
			return fmt.Errorf("replica readiness %d", r.StatusCode)
		}

		return nil
	})
	retained, _, err := c.trust()
	require(t, err)

	if retained.Active != initial.Active {
		t.Fatal("restart regenerated CA")
	}

	c.rotationTraffic(workers, initial)
	// A fresh Kubernetes Pod UID represents the restarted daemon. Retain the
	// actual slab inode and deliberately invalid creation-size environment.
	a.process.stop(false)

	zero := int64(0)
	require(t, c.kube.CoreV1().Pods(namespace).Delete(c.ctx, a.pod.Name, metav1.DeleteOptions{GracePeriodSeconds: &zero}))
	replacement, err := c.kube.CoreV1().Pods(namespace).Create(c.ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: a.pod.Name, Labels: a.pod.Labels, OwnerReferences: a.pod.OwnerReferences}, Spec: a.pod.Spec}, metav1.CreateOptions{})
	require(t, err)

	replacement.Status = a.pod.Status
	a.pod, err = c.kube.CoreV1().Pods(namespace).UpdateStatus(c.ctx, replacement, metav1.UpdateOptions{})
	require(t, err)
	c.startDataplane(a, control, enroll, proof, "not-a-size")

	restarted, reack := c.storage(a, "applied", 96<<20)
	if restarted.Storage.Boot == first.Storage.Boot || reack.PolicyIdentity != applied.PolicyIdentity || reack.PolicyVersion != applied.PolicyVersion || !os.SameFile(inode, stat()) {
		t.Fatalf("daemon restart lost inode or durable policy: %+v", reack)
	}
}

func summary(raw json.RawMessage) string {
	if len(raw) > 512 {
		return fmt.Sprintf("%d bytes digest=%s", len(raw), identity("diagnostic", string(raw)))
	}

	return string(raw)
}

func (c *campaign) record(cm *corev1.ConfigMap) map[string]json.RawMessage {
	c.t.Helper()

	var pointer struct{ Chunks []string }
	require(c.t, json.Unmarshal([]byte(cm.Data["pointer"]), &pointer))

	var raw []byte

	for _, chunk := range pointer.Chunks {
		v, err := c.kube.CoreV1().ConfigMaps(namespace).Get(c.ctx, "racer-v4-chunk-"+chunk, metav1.GetOptions{})
		require(c.t, err)

		raw = append(raw, v.BinaryData["content"]...)
	}

	var result map[string]json.RawMessage
	require(c.t, json.Unmarshal(raw, &result))

	return result
}

func (c *campaign) rotationTraffic(workers []*dataplane, initial bundle) {
	t := c.t

	record, stopDiagnostics := c.diagnostics(workers)
	defer stopDiagnostics()

	record("rotation-baseline")

	defer func() {
		if t.Failed() {
			b, _, err := c.trust()
			t.Logf("final public trust generation=%d active=%s error=%v", b.Generation, b.Active, err)

			for _, d := range workers {
				var s any

				err := c.getJSON("http://"+d.metrics+"/status", &s)
				t.Logf("%s status=%v error=%v", d.node.Name, s, err)
			}
		}
	}()
	// Fresh keys force metadata peer exchanges throughout the campaign. Each
	// successful SDK payload is checked, and any transient read error fails it.
	var reads atomic.Uint64

	stop := make(chan struct{})
	done := make(chan error, 1)

	go func() {
		for count := 0; ; count++ {
			for _, d := range workers[:2] {
				_, err := d.client.Open(c.ctx, fmt.Sprintf("/live-metadata-%d", count))

				var object *sdk.Object
				if err == nil {
					object, err = d.client.Open(c.ctx, fmt.Sprintf("/live-payload-%d", count%8))
				}

				if err == nil {
					data := make([]byte, originfixture.ObjectSize)

					var n int

					n, err = object.ReadAt(c.ctx, data, 0)
					if err == nil && (n != len(data) || !bytes.Equal(data, originfixture.Body(1))) {
						err = fmt.Errorf("SDK payload mismatch")
					}
				}

				if err != nil {
					record("traffic-error:" + d.node.Name)

					done <- fmt.Errorf("%s worker=%s iteration=%d: %w", time.Now().UTC().Format(time.RFC3339Nano), d.node.Name, count, err)

					return
				}

				reads.Add(1)
			}

			select {
			case <-stop:
				done <- nil
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()

	joined := false

	defer func() {
		close(stop)

		if !joined {
			err := <-done
			if err != nil {
				t.Error(err)
			}
		}
	}()

	c.await("baseline encrypted SDK reads", 30*time.Second, func() error {
		select {
		case err := <-done:
			joined = true

			t.Fatalf("SDK baseline failed: %v", err)
		default:
		}

		if reads.Load() < 4 {
			return fmt.Errorf("reads=%d", reads.Load())
		}

		return nil
	})
	_, err := c.kube.CoreV1().ConfigMaps(namespace).Patch(c.ctx, "racer-trust", types.MergePatchType, []byte(`{"metadata":{"annotations":{"racer.unbounded-cloud.io/rotate-ca":"live-campaign-1"}}}`), metav1.PatchOptions{})
	require(t, err)

	seen := map[uint64]bool{}

	var final bundle

	c.await("CA overlap, issuer switch, and expiry-gated retirement", 240*time.Second, func() error {
		select {
		case err := <-done:
			joined = true

			t.Errorf("SDK traffic failed during rotation after %d reads: %v", reads.Load(), err)
		default:
		}

		b, _, err := c.trust()
		if err != nil {
			return err
		}

		if !seen[b.Generation] {
			record(fmt.Sprintf("generation-%d", b.Generation))
			t.Logf("observed CA generation %d with %d verified SDK reads", b.Generation, reads.Load())
		}

		seen[b.Generation] = true
		if b.Generation < initial.Generation+3 {
			return fmt.Errorf("generation=%d reads=%d", b.Generation, reads.Load())
		}

		if b.Active == initial.Active || strings.Count(b.Certificates, "BEGIN CERTIFICATE") != 1 {
			return fmt.Errorf("old CA not retired")
		}

		final = b

		return nil
	})

	for _, d := range workers {
		c.await(d.node.Name+" installs retired trust", 30*time.Second, func() error {
			var s localStatus
			if err := c.getJSON("http://"+d.metrics+"/status", &s); err != nil {
				return err
			}

			if !s.Ready || s.TLS.Generation != final.Generation || s.TLS.Issuer != final.Active || s.TLS.InstalledWorkers != s.TLS.Workers || s.TLS.Error != nil {
				return fmt.Errorf("status=%+v", s)
			}

			return nil
		})
	}

	for _, d := range workers[:2] {
		object, err := d.client.Open(c.ctx, "/post-retirement")
		require(t, err)

		data := make([]byte, originfixture.ObjectSize)
		n, err := object.ReadAt(c.ctx, data, 0)
		require(t, err)

		if n != len(data) || !bytes.Equal(data, originfixture.Body(1)) {
			t.Fatal("post-retirement SDK data mismatch")
		}
	}

	for _, d := range workers[:2] {
		metrics := c.metrics(d.metrics)

		var peers, handshakes float64

		for _, line := range strings.Split(metrics, "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 {
				continue
			}

			value, _ := strconv.ParseFloat(fields[1], 64)
			if strings.HasPrefix(fields[0], `racer_dataplane_upstream_requests_total{destination="peer",transport="http"`) {
				peers += value
			}

			if fields[0] == "racer_dataplane_tls_handshakes_total" {
				handshakes = value
			}
		}

		if peers == 0 || handshakes < 3 {
			t.Fatalf("missing encrypted peer exchanges/reconnects: peers=%g TLS handshakes=%g", peers, handshakes)
		}

		t.Logf("%s peer requests=%g TLS handshakes=%g", d.node.Name, peers, handshakes)
	}

	if !seen[initial.Generation+1] || !seen[initial.Generation+2] || reads.Load() < 100 {
		t.Fatalf("missing rotation phases or traffic: generations=%v reads=%d", seen, reads.Load())
	}

	t.Logf("%d verified SDK reads across CA generations %d..%d", reads.Load(), initial.Generation, final.Generation)
}

// Capture only public trust and management endpoints. Credentials and private
// CA state stay in Go's temporary directory and are removed during cleanup.
func (c *campaign) diagnostics(workers []*dataplane) (func(string), func()) {
	c.t.Helper()
	file, err := os.Create(filepath.Join(c.artifacts, "observations.jsonl"))
	require(c.t, err)

	var mu sync.Mutex

	record := func(event string) {
		mu.Lock()
		defer mu.Unlock()

		entry := map[string]any{"at": time.Now().UTC().Format(time.RFC3339Nano), "event": event}
		b, _, err := c.trust()

		entry["trust"] = b
		if err != nil {
			entry["trustError"] = err.Error()
		}

		for _, d := range workers {
			var status any

			err := c.getJSON("http://"+d.metrics+"/status", &status)

			observation := map[string]any{"status": status}
			if err != nil {
				observation["error"] = err.Error()
			}

			resp, err := c.http.Get("http://" + d.metrics + "/metrics")
			if err == nil {
				raw, readErr := io.ReadAll(resp.Body)
				_ = resp.Body.Close()

				if readErr == nil {
					observation["metrics"] = string(raw)
				}
			}

			entry[d.node.Name] = observation
		}

		if err := json.NewEncoder(file).Encode(entry); err != nil {
			c.t.Errorf("persist diagnostics: %v", err)
		}
	}
	stop, done := make(chan struct{}), make(chan struct{})

	go func() {
		defer close(done)

		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				record("sample")
			}
		}
	}()

	return record, func() { close(stop); <-done; require(c.t, file.Close()) }
}
