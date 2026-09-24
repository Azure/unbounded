//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controlplane_test

import (
	"bytes"
	"context"
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
	racermeta "github.com/Azure/unbounded/internal/racer"
	sdk "github.com/Azure/unbounded/pkg/racersdk"
)

type dataplane struct {
	node                      *corev1.Node
	pod                       *corev1.Pod
	ip, dir, sockets, metrics string
	process                   *process
	client                    *sdk.Client
	origin                    *originfixture.Origin
}

type localStatus struct {
	Ready          bool
	ActiveRevision uint64
	TLS            struct {
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
	node, err := c.kube.CoreV1().Nodes().Create(c.ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("worker-%d", index), Labels: map[string]string{"unbounded-cloud.io/site": site, "kubernetes.io/os": "linux"}, Annotations: map[string]string{prefix + "cache-size": "1Gi"}}}, metav1.CreateOptions{})
	require(t, err)

	node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	node, err = c.kube.CoreV1().Nodes().UpdateStatus(c.ctx, node, metav1.UpdateOptions{})
	require(t, err)

	labels := map[string]string{prefix + "dataplane": "true", prefix + "component": "racer-dataplane"}
	template := corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{ServiceAccountName: "racer-dataplane", Containers: []corev1.Container{{Name: "dataplane", Image: "fixture"}}}}

	ds, err := c.kube.AppsV1().DaemonSets(namespace).Get(c.ctx, "racer-dataplane", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		ds, err = c.kube.AppsV1().DaemonSets(namespace).Create(c.ctx, &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "racer-dataplane", Labels: labels}, Spec: appsv1.DaemonSetSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}, Template: template}}, metav1.CreateOptions{})
	}

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
	env := []string{"RACER_CONTROL_PLANE_URL=https://" + control + "/v1/config", "RACER_UNIVERSE=" + identity("universe", d.node.Labels["unbounded-cloud.io/site"]), "RACER_NODE=" + identity("node", string(d.node.UID)), "RACER_TLS_TRUST_DIR=" + d.dir, "RACER_CONTROL_TOKEN_FILE=" + filepath.Join(d.dir, "token"), "RACER_ENROLL_URL=https://" + enroll + "/v1/enroll", "RACER_TRUST_PROOF_URL=https://" + proof + "/v1/proof", "RACER_CONTROL_SERVER_NAME=racer-controlplane." + namespace + ".svc", "RACER_POD_NAMESPACE=" + namespace, "RACER_POD_NAME=" + d.pod.Name, "RACER_POD_UID=" + string(d.pod.UID), "RACER_POD_IP=" + d.ip, "RACER_SLAB_PATH=" + filepath.Join(d.dir, "cache.slab"), "RACER_SLAB_SIZE=" + creationSize, "RACER_SHARDS=1", "RACER_IO_WORKERS=1", "RACER_COMPUTE_WORKERS=1", "RACER_BUFFERS_PER_NODE=8", "RACER_RDMA_MODE=disabled", "RACER_METRICS_ADDR=" + d.metrics, "RACER_SLAB_IOPS=100", "RACER_SLAB_IO_BURST=1"}
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

	cacheSocket, originSocket, err := racermeta.CacheSockets(c.socketRoot, string(v.GetUID()))
	if err != nil {
		return err
	}

	actualCache, _, _ := unstructured.NestedString(v.Object, "status", "cacheSocket")

	actualOrigin, _, _ := unstructured.NestedString(v.Object, "status", "originSocket")
	if actualCache != cacheSocket || actualOrigin != originSocket {
		return fmt.Errorf("cache %s socket status does not match UID %s: %v", name, v.GetUID(), v.Object["status"])
	}

	return nil
}

func (c *campaign) startOrigins(workers []*dataplane) {
	t := c.t

	for _, d := range workers {
		name := d.node.Labels["unbounded-cloud.io/site"]
		cache, err := c.dynamic.Resource(cacheResource).Get(c.ctx, name, metav1.GetOptions{})
		require(t, err)
		cacheSocket, originSocket, err := racermeta.CacheSockets(d.sockets, string(cache.GetUID()))
		require(t, err)
		require(t, os.MkdirAll(filepath.Dir(originSocket), 0o700))
		l, err := net.Listen("unix", originSocket)
		require(t, err)

		backend := originfixture.NewOrigin()
		backend.Source = d.node.Name
		d.origin = backend
		origin := httptest.NewUnstartedServer(backend)
		_ = origin.Listener.Close()
		origin.Listener = l
		origin.Start()
		t.Cleanup(origin.Close)

		d.client, err = sdk.NewClient(cacheSocket, sdk.ClientOptions{})
		require(t, err)
		t.Cleanup(d.client.CloseIdleConnections)
	}
}

func (c *campaign) createSite(site string) {
	c.create(siteResource, map[string]any{"apiVersion": "unbounded-cloud.io/v1alpha3", "kind": "Site", "metadata": map[string]any{"name": site, "labels": map[string]any{"campaign": site}}, "spec": map[string]any{"nodeCidrs": []any{"10.0.0.0/16"}, "podCidrAssignments": []any{map[string]any{"cidrBlocks": []any{"10.1.0.0/16"}}}}})
	c.create(cacheResource, map[string]any{"apiVersion": "racer.unbounded-cloud.io/v1alpha1", "kind": "ClusterCache", "metadata": map[string]any{"name": site}, "spec": map[string]any{"siteSelector": map[string]any{"matchLabels": map[string]any{"campaign": site}}}})
}

// Isolate cold HEAD -> conditional GET through real peers from storage changes,
// control-plane failover, and CA rotation. Every failed read is terminal.
func TestColdObjectMultiPeer(t *testing.T) {
	c := newCampaign(t)
	c.createSite("edge")
	workers := []*dataplane{c.worker(0, "edge"), c.worker(1, "edge")}
	c.startOrigins(workers)

	for _, r := range c.replicas {
		c.startReplica(r)
	}

	var leader *replica

	c.await("leader", 60*time.Second, func() error { var err error; leader, err = c.leader(); return err })

	stopProjection := c.projectTrust(workers...)
	defer stopProjection()

	for _, d := range workers {
		c.await(d.node.Name+" trust projection", 5*time.Second, func() error { _, err := os.Stat(filepath.Join(d.dir, "bundle.json")); return err })
		c.startDataplane(d, leader.control, leader.enroll, leader.trust, "1073741824")
		c.storage(d, "applied", 1<<30)
	}

	c.await("two ready peers", 60*time.Second, func() error { return c.cacheReady("edge", 2, 2) })

	record, stopDiagnostics := c.diagnostics(workers)
	defer stopDiagnostics()
	defer record("cold-object-final")

	for count := 0; count < 8; count++ {
		for _, d := range workers {
			target := fmt.Sprintf("/live-payload-%d", count)
			object, err := d.client.Open(c.ctx, target)
			require(t, err)

			meta := object.Metadata()

			data, err := readObject(c.ctx, object)
			if err != nil || !bytes.Equal(data, originfixture.Body(1)) {
				t.Fatalf("%s GET %s HEAD=%+v: bytes=%d error=%v", d.node.Name, target, meta, len(data), err)
			}

			if meta.ETag != originfixture.ETag(1) || meta.ContentType != "application/octet-stream" {
				t.Fatalf("%s HEAD %s: %+v", d.node.Name, target, meta)
			}
		}
	}

	var peerRequests float64

	for _, d := range workers {
		for _, line := range strings.Split(c.metrics(d.metrics), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && strings.HasPrefix(fields[0], `racer_dataplane_upstream_requests_total{destination="peer",transport="http"`) {
				value, err := strconv.ParseFloat(fields[1], 64)
				require(t, err)

				peerRequests += value
			}
		}
	}

	if peerRequests == 0 {
		t.Fatal("cold-object regression did not exercise peer HTTP")
	}
}

func readObject(ctx context.Context, object *sdk.Object) ([]byte, error) {
	stream, err := object.Stream(ctx)
	if err != nil {
		return nil, err
	}
	defer stream.Close()

	var data bytes.Buffer

	_, err = stream.WriteTo(&data)

	return data.Bytes(), err
}

func TestProductionBinaryCampaign(t *testing.T) {
	c := newCampaign(t)
	for _, site := range []string{"edge", "independent"} {
		c.createSite(site)
	}

	a, b := c.worker(0, "edge"), c.worker(1, "edge")
	independent := c.worker(2, "independent")

	workers := []*dataplane{a, b, independent}
	c.startOrigins(workers)

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
	cmd := exec.Command(c.cpBinary, "--bootstrap-node", a.node.Name, "--bootstrap-namespace", namespace)
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

	response, err := anonymous.Get("https://" + control + "/v1/config")
	if err == nil {
		response.Body.Close()
		t.Fatalf("control accepted missing client certificate: %d", response.StatusCode)
	}
	// One selected daemon is deliberately absent. Its peer and a separate universe
	// must still install desired state; readiness must not claim both are converged.
	c.startDataplane(a, control, enroll, proof, "1073741824")
	c.startDataplane(independent, control, enroll, proof, "1073741824")
	c.storage(a, "applied", 1<<30)
	c.storage(independent, "applied", 1<<30)
	c.await("independent convergence while selected peer is absent", 60*time.Second, func() error {
		if err := c.cacheReady("edge", 2, 1); err != nil {
			return err
		}

		return c.cacheReady("independent", 1, 1)
	})
	c.startDataplane(b, control, enroll, proof, "1073741824")
	c.storage(b, "applied", 1<<30)
	c.await("all edge participants converged", 60*time.Second, func() error { return c.cacheReady("edge", 2, 2) })

	first, policy := c.storage(a, "applied", 1<<30)
	if policy.PolicyIdentity != identity("storage", string(a.node.UID)) {
		t.Fatalf("storage identity is not derived from Node UID: %s", policy.PolicyIdentity)
	}

	c.footprint()

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
	}{{"20Gi", 20 << 30, 2}, {"1536Mi", 1536 << 20, 1}} {
		c.size(a, step.quantity)

		local, status := c.storage(a, "applied", step.bytes)
		if local.Storage.Boot != first.Storage.Boot || local.Storage.Shards != step.shards || status.PolicyIdentity != policy.PolicyIdentity || status.AppliedVersion != status.PolicyVersion || os.SameFile(inode, stat()) || stat().Size() != int64(step.bytes) {
			t.Fatalf("resize lost process/policy identity or inode replacement: %+v %+v", local, status)
		}

		inode = stat()
	}

	c.size(a, "5Ti")

	_, failed := c.storage(a, "failed", 1536<<20)
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

		if s.Phase != "invalid" || s.ValidationError == nil || s.PolicyVersion != failed.PolicyVersion || s.AppliedBytes != 1536<<20 || !os.SameFile(inode, stat()) {
			return fmt.Errorf("invalid status: %+v", s)
		}

		return nil
	})
	c.size(a, "1536Mi")
	_, applied := c.storage(a, "applied", 1536<<20)
	c.size(a, "1610612736")

	_, equivalent := c.storage(a, "applied", 1536<<20)
	if equivalent.PolicyVersion != applied.PolicyVersion || !os.SameFile(inode, stat()) {
		t.Fatal("equivalent storage quantity reset policy or inode")
	}

	var current localStatus
	require(t, c.getJSON("http://"+a.metrics+"/status", &current))

	if current.ActiveRevision != first.ActiveRevision {
		t.Fatal("storage-only changes mutated topology")
	}
	// Desired payloads are disposable. Verify real watch publication through the
	// installed dataplane revision instead of reading a persisted topology record.
	c.patch(cacheResource, "edge", `{"spec":{"cacheGeneration":2}}`)
	beforeRevision := c.convergedRevision(first.ActiveRevision, a, b)
	before := c.checkpoint()
	// Kill the elected process without releasing its Lease. The warm replica must
	// wait out the real Lease, rebuild desired state, and re-observe daemon feedback.
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
	beforeRevision = c.convergedRevision(beforeRevision, a, b)
	c.checkNewReservation(before)

	_, afterFailover := c.storage(a, "applied", 1536<<20)
	if afterFailover.PolicyIdentity != applied.PolicyIdentity || afterFailover.PolicyVersion <= applied.PolicyVersion || !os.SameFile(inode, stat()) {
		t.Fatal("failover must rebuild a newer storage policy without replacing the slab")
	}
	// A process restart in the same Pod is valid: no durable boot registry forces
	// Pod deletion. The replacement process obtains a new signed boot identity.
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
	pod, err := c.kube.CoreV1().Pods(namespace).Get(c.ctx, leader.pod.Name, metav1.GetOptions{})
	require(t, err)

	if pod.UID != leader.pod.UID {
		t.Fatal("same-Pod controller restart changed Pod UID")
	}

	c.replicaClaims(leader)

	before = c.checkpoint()
	for _, r := range c.replicas {
		r.process.stop(true)
	}

	for _, r := range c.replicas {
		c.startReplica(r)
	}

	c.await("all controllers restart", 75*time.Second, func() error {
		r, err := c.leader()
		if err != nil {
			return err
		}

		target.Store(r)

		return nil
	})
	beforeRevision = c.convergedRevision(beforeRevision, a, b)
	c.checkNewReservation(before)

	_, beforeReplacement := c.storage(a, "applied", 1536<<20)
	if beforeReplacement.PolicyIdentity != applied.PolicyIdentity || beforeReplacement.PolicyVersion <= afterFailover.PolicyVersion || !os.SameFile(inode, stat()) {
		t.Fatal("full CP restart lost deterministic storage identity, newer version, or slab inode")
	}
	// Envtest has no ReplicaSet controller. Replace the follower Pod explicitly
	// and verify the new UID receives its own signed process-local certificate.
	for _, r := range c.replicas {
		if r == target.Load() {
			continue
		}

		r.process.stop(false)
		old := r.pod
		zero := int64(0)
		require(t, c.kube.CoreV1().Pods(namespace).Delete(c.ctx, old.Name, metav1.DeleteOptions{GracePeriodSeconds: &zero}))
		replacement, err := c.kube.CoreV1().Pods(namespace).Create(c.ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: old.Name, Labels: map[string]string{prefix + "component": "racer-controlplane"}, OwnerReferences: old.OwnerReferences}, Spec: old.Spec}, metav1.CreateOptions{})
		require(t, err)

		replacement.Status = old.Status
		r.pod, err = c.kube.CoreV1().Pods(namespace).UpdateStatus(c.ctx, replacement, metav1.UpdateOptions{})
		require(t, err)

		if r.pod.UID == old.UID {
			t.Fatal("controller Pod replacement reused UID")
		}

		c.startReplica(r)
		c.replicaClaims(r)
	}

	c.footprint()
	retained, _, err := c.trust()
	require(t, err)

	if retained.Active != initial.Active {
		t.Fatal("restart regenerated CA")
	}

	// A selected, previously enrolled daemon can remain offline throughout root
	// retirement. Timed overlap and issuer expiry do not wait for its proofs.
	independent.process.stop(false)
	c.rotationTraffic(workers[:2], initial)
	c.startDataplane(independent, control, enroll, proof, "1073741824")
	c.storage(independent, "applied", 1<<30)
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

	restarted, reack := c.storage(a, "applied", 1536<<20)
	if restarted.Storage.Boot == first.Storage.Boot || reack.PolicyIdentity != applied.PolicyIdentity || reack.PolicyVersion != beforeReplacement.PolicyVersion || !os.SameFile(inode, stat()) {
		t.Fatalf("daemon restart lost inode or durable policy: %+v", reack)
	}

	c.convergedRevision(beforeRevision, a, b)
	c.scaleFootprint(independent)
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
					var data []byte

					data, err = readObject(c.ctx, object)
					if err == nil && !bytes.Equal(data, originfixture.Body(1)) {
						err = fmt.Errorf("SDK payload mismatch")
					}
				}

				if err != nil {
					record("traffic-error:" + d.node.Name)

					if object != nil {
						err = fmt.Errorf("HEAD metadata=%+v: %w", object.Metadata(), err)
					}

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
	requestedAt := time.Now()

	var (
		switchedAt time.Time
		oldExpiry  int64
	)

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
		if b.Generation == initial.Generation+1 && (b.Active != initial.Active || strings.Count(b.Certificates, "BEGIN CERTIFICATE") != 2) {
			t.Fatal("overlap must retain the old issuer and publish both roots")
		}

		if b.Generation == initial.Generation+2 {
			if time.Since(requestedAt) < overlapDelay || b.Active == initial.Active || strings.Count(b.Certificates, "BEGIN CERTIFICATE") != 2 {
				t.Fatal("issuer switched before timed overlap or dropped the old root")
			}

			if switchedAt.IsZero() {
				switchedAt = time.Now()

				secret, err := c.kube.CoreV1().Secrets(namespace).Get(c.ctx, "racer-ca", metav1.GetOptions{})
				if err != nil {
					return err
				}

				var state struct {
					Authorities []struct {
						Digest string `json:"digest"`
						Expiry int64  `json:"last_issued_expiry"`
					} `json:"authorities"`
				}
				require(t, json.Unmarshal(secret.Data["state.json"], &state))

				for _, ca := range state.Authorities {
					if ca.Digest == initial.Active {
						oldExpiry = ca.Expiry
					}
				}

				if oldExpiry <= switchedAt.Unix() {
					t.Fatal("missing durable old-issuer expiry watermark")
				}
			}
		}

		if b.Generation < initial.Generation+3 {
			return fmt.Errorf("generation=%d reads=%d", b.Generation, reads.Load())
		}

		if b.Active == initial.Active || strings.Count(b.Certificates, "BEGIN CERTIFICATE") != 1 {
			return fmt.Errorf("old CA not retired")
		}

		if oldExpiry == 0 || time.Now().Unix() < oldExpiry+1 {
			t.Fatal("old root retired before durable leaf expiry plus skew")
		}

		c.footprint()

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

		data, err := readObject(c.ctx, object)
		require(t, err)

		if !bytes.Equal(data, originfixture.Body(1)) {
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

// Capture public trust, management endpoints, and synthetic origin requests.
// Credentials and private CA state stay in Go's temporary directory and are
// removed during cleanup.
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
			if event != "sample" && d.origin != nil {
				observation["originHits"] = d.origin.Hits()
			}

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
