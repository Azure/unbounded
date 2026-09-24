//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	core "k8s.io/api/core/v1"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/utils/ptr"

	racerapi "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/e2e/racer/fixture"
	racermeta "github.com/Azure/unbounded/internal/racer"
)

func intPort(port int) intstr.IntOrString { return intstr.FromInt(port) }

func TestDeployment(t *testing.T) {
	root := repository(t)
	im := buildImages(t, root)
	c := newCluster(t, root, im)
	c.deploy()
	t.Log("bootstrap, Unix routing, and subscription")

	revision := c.converge(0, "racer-volume")
	c.checkSubscription()
	keys := c.caSecrets()

	t.Log("staged CA rotation with continuous traffic and leader failover")
	c.rotateCA(keys)
	keys = c.caSecrets() // Persistence checks start after deliberate rotation.
	c.converge(revision-1, "racer-volume")
	t.Log("HEAD, GET, range, missing objects, cache hits, and peer forwarding")
	c.readsAndCaching()
	t.Log("origin socket rebinding preserves warm cache identity")

	for _, name := range []string{"origin", "origin-b"} {
		c.must("delete", "pod", name, "--wait=true")

		node := c.name + "-worker"
		if name == "origin-b" {
			node += "2"
		}

		c.fixturePod(name, node, "origin")
		c.must("wait", "--for=condition=Ready", "pod/"+name, "--timeout=90s")
	}

	before := len(c.hits())
	for _, probe := range []string{"probe-a", "probe-b"} {
		c.checkObject(probe, "racer-volume", "/object-0", 1)
	}

	if len(c.hits()) != before {
		t.Fatal("origin socket rebinding invalidated warm cache")
	}

	c.checkObject("probe-a", "racer-volume", "/after-origin-recreation", 1)
	t.Log("cache-generation update")
	c.setVersion("origin", 2)
	c.must("patch", "clustercache/racer-volume", "--type=merge", "-p", `{"spec":{"cacheGeneration":2}}`)

	revision = c.converge(revision, "racer-volume")
	for _, probe := range []string{"probe-a", "probe-b"} {
		c.checkObject(probe, "racer-volume", "/object-0", 2)
	}

	t.Log("independent second volume and removal")
	c.apply(cacheResource("second-volume", primarySite))
	c.startAlternateOrigins("second-volume")
	revision = c.converge(revision, "racer-volume", "second-volume")

	var first, second racerapi.ClusterCache
	if err := c.get("clustercache", "racer-volume", &first); err != nil {
		t.Fatal(err)
	}

	if err := c.get("clustercache", "second-volume", &second); err != nil {
		t.Fatal(err)
	}

	if first.UID == second.UID {
		t.Fatal("caches share identity")
	}

	for _, probe := range []string{"probe-a", "probe-b"} {
		c.checkObject(probe, "second-volume", "/object-0", 3)
		c.checkObject(probe, "racer-volume", "/object-0", 2)
	}

	c.must("delete", "clustercache/second-volume", "--wait=false")
	revision = c.converge(revision, "racer-volume")

	t.Log("controller leader replacement and newer revision allocation")

	leader := c.leader()
	c.must("delete", "pod", leader.Name, "--wait=false")
	c.await("new control-plane leader", func() error {
		pods, err := c.pods(controlSelector)
		if err != nil {
			return err
		}

		for _, p := range pods {
			if podReady(p) && p.UID != leader.UID {
				return nil
			}
		}

		return fmt.Errorf("no replacement serving leader")
	})
	// A fresh object proves origin access still works after leadership changes.
	c.checkObject("probe-a", "racer-volume", "/after-failover", 2)
	c.must("patch", "clustercache/racer-volume", "--type=merge", "-p", `{"spec":{"cacheGeneration":3}}`)
	revision = c.converge(revision, "racer-volume")
	c.checkSubscription()
	c.checkCAUnchanged(keys)
	t.Log("all controller replicas restart and reuse the CA")
	c.must("delete", "pods", "-l", controlSelector, "--wait=false")
	c.leader()
	c.must("patch", "clustercache/racer-volume", "--type=merge", "-p", `{"spec":{"cacheGeneration":4}}`)
	revision = c.converge(revision, "racer-volume")
	c.checkSubscription()
	c.checkCAUnchanged(keys)
	c.checkObject("probe-a", "racer-volume", "/after-controller-restart", 2)
	t.Log("dataplane replacement, stable bootstrap identity, and reactivation")

	pods, err := c.pods(dataplaneSelector)
	if err != nil {
		t.Fatal(err)
	}

	old := pods[0]
	identity := c.must("exec", old.Name, "-c", "dataplane", "--", "/bin/sh", "-c", "cat /bootstrap/identity")
	c.must("delete", "pod", old.Name, "--wait=false")

	var replacement core.Pod

	c.await("replacement dataplane on same node", func() error {
		pods, err := c.pods(dataplaneSelector)
		if err != nil {
			return err
		}

		for _, p := range pods {
			if p.Spec.NodeName == old.Spec.NodeName && p.UID != old.UID && podReady(p) {
				replacement = p
				return nil
			}
		}

		return fmt.Errorf("replacement not Ready")
	})
	// Pod UID is part of current selection even when its IP is reused.
	c.converge(revision, "racer-volume")

	got := c.must("exec", replacement.Name, "-c", "dataplane", "--", "/bin/sh", "-c", "cat /bootstrap/identity")
	if !bytes.Equal(identity, got) {
		t.Fatalf("bootstrap identity changed: %s -> %s", identity, got)
	}

	for _, probe := range []string{"probe-a", "probe-b"} {
		c.checkObject(probe, "racer-volume", "/after-restart", 2)
	}

	c.membershipChanges()
}

func (c *cluster) pods(selector string) ([]core.Pod, error) {
	b, err := c.kubectl(nil, "get", "pods", "-l", selector, "-o", "json")
	if err != nil {
		return nil, err
	}

	var list core.PodList
	decode(c.t, b, &list)

	return list.Items, nil
}

// Read back persisted material; initial provisioning belongs to the controller.
const (
	caSecretName       = "racer-ca"
	caStateKey         = "state.json"
	trustConfigMapName = "racer-trust"
	trustBundleKey     = "bundle.json"
)

// Only immutable root material is compared across failover. Rust's expiry
// watermarks, fence, and rotation nonce legitimately change.
type persistedRoot struct {
	Digest      string `json:"digest"`
	Certificate string `json:"certificate"`
	PrivateKey  string `json:"private_key"`
}

type persistedCA struct {
	Version     int             `json:"version"`
	Active      string          `json:"active"`
	Authorities []persistedRoot `json:"authorities"`
}

func (c *cluster) caSecrets() map[string]core.Secret {
	c.t.Helper()

	var secret core.Secret
	if err := c.get("secret", caSecretName, &secret); err != nil {
		c.t.Fatal(err)
	}

	var state persistedCA
	decode(c.t, secret.Data[caStateKey], &state)

	bundle := c.trustBundle()
	if secret.Type != core.SecretTypeOpaque || secret.UID == "" || state.Version != 5 || len(secret.Data) != 1 || len(secret.Data[caStateKey]) > 16*1024 || len(state.Authorities) == 0 || len(state.Authorities) > 2 || state.Active != bundle.Active {
		c.t.Fatal("invalid persisted CA")
	}

	for _, ca := range state.Authorities {
		if ca.PrivateKey == "" || !strings.Contains(bundle.Certificates, ca.Certificate) {
			c.t.Fatal("CA and public trust differ")
		}
	}

	return map[string]core.Secret{secret.Name: secret}
}

func (c *cluster) checkCAUnchanged(before map[string]core.Secret) {
	c.t.Helper()

	after := c.caSecrets()
	for name, old := range before {
		current := after[name]
		// Leadership fencing and issuer expiry watermarks legitimately change.
		var a, b persistedCA
		decode(c.t, old.Data[caStateKey], &a)
		decode(c.t, current.Data[caStateKey], &b)

		oldWire, _ := json.Marshal(a.Authorities)

		newWire, _ := json.Marshal(b.Authorities)
		if current.UID != old.UID || a.Active != b.Active || !bytes.Equal(oldWire, newWire) {
			c.t.Fatalf("controller restart/failover replaced %s", name)
		}
	}
}

func (c *cluster) rotateCA(before map[string]core.Secret) {
	pods, err := c.pods(dataplaneSelector)
	if err != nil {
		c.t.Fatal(err)
	}

	traffic := 0
	checkTraffic := func() {
		for _, probe := range []string{"probe-a", "probe-b"} {
			// Fresh metadata exercises authenticated forwarding; keep full-page
			// reads bounded to preserve slab headroom for later topology tests.
			r := c.fetch(probe, "HEAD", serviceURL("racer-volume", fmt.Sprintf("/rotation-metadata-%d", traffic)), "")
			if r.Status != 200 {
				c.t.Fatalf("rotation metadata: status=%d", r.Status)
			}

			c.checkObject(probe, "racer-volume", fmt.Sprintf("/rotation-%d", traffic%4), 1)
		}

		traffic++
	}

	if len(before) != 1 {
		c.t.Fatal("rotation requires the persisted CA")
	}

	old := c.trustBundle()
	c.must("annotate", "configmap/"+trustConfigMapName, racermeta.MetadataPrefix+"rotate-ca="+strconv.FormatInt(time.Now().UnixNano(), 10), "--overwrite")
	c.await("pending CA published before activation", func() error {
		checkTraffic()

		next := c.trustBundle()
		if next.Active != old.Active {
			c.t.Fatal("activated without observed overlap")
		}

		if next.Generation != old.Generation+1 || strings.Count(next.Certificates, "BEGIN CERTIFICATE") != 2 {
			return fmt.Errorf("pending CA not published")
		}

		return nil
	})
	leader := c.leader()
	c.must("delete", "pod", leader.Name, "--wait=false")
	c.awaitFor(12*time.Minute, "new issuer and live trust projection", func() error {
		checkTraffic()

		bundle := c.trustBundle()
		if bundle.Active == old.Active {
			return fmt.Errorf("waiting for timed publication overlap")
		}
		// Production leaves live for 24 hours. Old-root removal must wait for
		// their expiry; the bounded crosslanguage campaign uses short-lived leaves.
		if bundle.Generation != old.Generation+2 || strings.Count(bundle.Certificates, "BEGIN CERTIFICATE") != 2 {
			c.t.Fatal("old CA retired before leaf expiry")
		}

		for _, pod := range pods {
			r, err := c.request("probe-a", "GET", "http://"+pod.Status.PodIP+":9090/status", "")
			if err != nil {
				return err
			}

			var s status
			if r.Status != 200 || json.Unmarshal(r.Body, &s) != nil || s.TLS.TrustDigest != bundle.Digest() || s.TLS.Generation != bundle.Generation || s.TLS.Issuer != bundle.Active || s.TLS.InstalledWorkers != s.Workers || s.TLS.Error != nil {
				return fmt.Errorf("%s has not installed renewed identity", pod.Name)
			}
		}

		return nil
	})

	current, err := c.pods(dataplaneSelector)
	if err != nil {
		c.t.Fatal(err)
	}

	for _, old := range pods {
		i := slices.IndexFunc(current, func(p core.Pod) bool { return p.UID == old.UID })
		if i < 0 || current[i].Status.ContainerStatuses[0].RestartCount != old.Status.ContainerStatuses[0].RestartCount {
			c.t.Fatal("dataplane restarted during rotation")
		}
	}
}

func (c *cluster) trustBundle() racermeta.TrustBundle {
	c.t.Helper()

	var cm core.ConfigMap
	if err := c.get("configmap", trustConfigMapName, &cm); err != nil {
		c.t.Fatal(err)
	}

	bundle, err := racermeta.ParseTrustBundle([]byte(cm.Data[trustBundleKey]))
	if err != nil {
		c.t.Fatal(err)
	}

	return bundle
}

type status struct {
	Ready                     bool
	ActiveRevision            uint64
	CandidateRevision         uint64
	Workers, ActivatedWorkers int
	Rejected                  bool
	TLS                       struct {
		Generation          uint64
		Issuer, TrustDigest string
		InstalledWorkers    int
		Error               *string
	}
	Volumes []struct {
		ID    string
		Epoch uint64
		Ready bool
	}
}

func (c *cluster) converge(after uint64, volumes ...string) uint64 {
	c.t.Helper()

	var revision uint64

	c.await("all workers and ClusterCaches converged", func() error {
		pods, err := c.pods(dataplaneSelector)
		if err != nil {
			return err
		}

		if len(pods) != 2 || pods[0].Spec.NodeName == pods[1].Spec.NodeName {
			return fmt.Errorf("need two dataplanes on distinct workers: %+v", pods)
		}

		revision = 0

		for _, p := range pods {
			if !podReady(p) {
				return fmt.Errorf("dataplane %s not Ready", p.Name)
			}

			for _, init := range p.Status.InitContainerStatuses {
				if init.State.Terminated == nil || init.State.Terminated.ExitCode != 0 {
					return fmt.Errorf("bootstrap failed: %+v", init)
				}
			}

			r, err := c.request("probe-a", "GET", "http://"+p.Status.PodIP+":9090/status", "")
			if err != nil {
				return err
			}

			if r.Status != 200 {
				return fmt.Errorf("status: %d %s", r.Status, r.Body)
			}

			var s status
			decode(c.t, r.Body, &s)

			if !s.Ready || s.ActiveRevision <= after || s.Rejected || len(s.Volumes) != len(volumes) {
				return fmt.Errorf("%s: %s", p.Name, r.Body)
			}

			if s.ActiveRevision != s.CandidateRevision || s.Workers == 0 || s.ActivatedWorkers != s.Workers || s.TLS.TrustDigest == "" {
				return fmt.Errorf("coordinated activation incomplete: %s", r.Body)
			}

			for _, name := range volumes {
				var cache racerapi.ClusterCache
				if err := c.get("clustercache", name, &cache); err != nil {
					return err
				}

				found := false

				for _, v := range s.Volumes {
					if v.ID == string(cache.UID) && v.Ready && v.Epoch > 0 {
						found = true
					}
				}

				if !found {
					return fmt.Errorf("missing active volume %s: %s", name, r.Body)
				}
			}

			if revision != 0 && revision != s.ActiveRevision {
				return fmt.Errorf("dataplanes have different revisions")
			}

			revision = s.ActiveRevision
		}

		for _, name := range volumes {
			var cache racerapi.ClusterCache
			if err := c.get("clustercache", name, &cache); err != nil {
				return err
			}

			if cache.Status.ObservedGeneration != cache.Generation || !apiMeta.IsStatusConditionTrue(cache.Status.Conditions, "Ready") || cache.Status.Participants.Desired != 2 || cache.Status.Participants.Ready != 2 {
				return fmt.Errorf("cache status not converged: %+v", cache.Status)
			}

			cacheSocket, originSocket, err := racermeta.CacheSockets(racermeta.SocketRoot, string(cache.UID))
			if err != nil {
				return err
			}

			if cache.Status.CacheSocket != cacheSocket || cache.Status.OriginSocket != originSocket {
				return fmt.Errorf("cache %s socket status does not match UID %s: %+v", name, cache.UID, cache.Status)
			}
			// Verify each local origin independently of activation readiness.
			for _, probe := range []string{"probe-a", "probe-b"} {
				// A new target avoids a cached negative result hiding origin routing lag.
				target := fmt.Sprintf("/missing-routing-ready-%d-%s", revision, probe)

				r, err := c.request(probe, "HEAD", serviceURL(name, target), "")
				if err != nil {
					return err
				}

				if r.Status != 404 {
					return fmt.Errorf("%s routing not ready: %d", name, r.Status)
				}
			}
		}

		return nil
	})
	c.t.Logf("active revision %d, volumes %v", revision, volumes)

	return revision
}

func (c *cluster) fetch(probe, method, url, byteRange string) fixture.Response {
	c.t.Helper()

	r, err := c.request(probe, method, url, byteRange)
	if err != nil {
		c.t.Fatal(err)
	}

	return r
}

func serviceURL(service, target string) string {
	return "unix://" + service + target
}

func (c *cluster) checkObject(probe, service, target string, version int) {
	c.t.Helper()

	url := serviceURL(service, target)
	etag := fixture.ETag(version)

	h := c.fetch(probe, "HEAD", url, "")
	if h.Status != 200 || h.Header.Get("Content-Length") != strconv.Itoa(fixture.ObjectSize) || h.Header.Get("ETag") != etag || len(h.Body) != 0 {
		c.t.Fatalf("HEAD %s: %+v", url, h)
	}

	r := c.fetch(probe, "GET", url, "")
	if r.Status != 200 || r.Header.Get("ETag") != etag || !bytes.Equal(r.Body, fixture.Body(version)) {
		c.t.Fatalf("GET %s: status=%d etag=%s bytes=%d", url, r.Status, r.Header.Get("ETag"), len(r.Body))
	}

	r = c.fetch(probe, "GET", url, "bytes=123-1023")
	if r.Status != 206 || r.Header.Get("Content-Range") != "bytes 123-1023/16384" || !bytes.Equal(r.Body, fixture.Body(version)[123:1024]) {
		c.t.Fatalf("range %s: status=%d headers=%v bytes=%d", url, r.Status, r.Header, len(r.Body))
	}
}

func (c *cluster) hits() []fixture.Hit {
	var hits []fixture.Hit

	for _, name := range []string{"origin", "origin-b"} {
		var pod core.Pod
		if err := c.get("pod", name, &pod); err != nil {
			c.t.Fatal(err)
		}

		r := c.fetch("probe-a", "GET", "http://"+pod.Status.PodIP+":8080/hits", "")
		if r.Status != 200 {
			c.t.Fatalf("origin hits: %d", r.Status)
		}

		var local []fixture.Hit
		decode(c.t, r.Body, &local)
		hits = append(hits, local...)
	}

	return hits
}

func (c *cluster) setVersion(origin string, version int) {
	for _, name := range []string{origin, origin + "-b"} {
		var pod core.Pod
		if err := c.get("pod", name, &pod); err != nil {
			c.t.Fatal(err)
		}

		r := c.fetch("probe-a", "POST", fmt.Sprintf("http://%s:8080/version?value=%d", pod.Status.PodIP, version), "")
		if r.Status != 200 {
			c.t.Fatalf("set origin version: %d %s", r.Status, r.Body)
		}
	}
}

func (c *cluster) readsAndCaching() {
	targets := []string{"//object%2f?b=2&a=1&a=3", "/object%2F", "/object?", "/object/../raw", "/metadata", "/page"}
	for i := 0; i < 8; i++ {
		targets = append(targets, fmt.Sprintf("/object-%d", i))
	}

	for _, target := range targets {
		for _, probe := range []string{"probe-a", "probe-b"} {
			c.checkObject(probe, "racer-volume", target, 1)
		}
	}

	hits := c.hits()
	// Exactly one metadata and one page origin fetch per target, despite reads
	// through both nodes. Metadata and page keys have independent physical owners.
	for _, target := range targets {
		methods := map[string]int{}
		sources := map[string]bool{}

		for _, hit := range hits {
			if hit.Target == target {
				methods[hit.Method]++
				sources[hit.Source] = true
			}
		}

		if methods["HEAD"] != 1 || methods["GET"] != 1 || sources[""] || len(sources) == 0 || len(sources) > 2 {
			c.t.Fatalf("origin fetches for %s: methods=%v sources=%v", target, methods, sources)
		}
	}

	for _, probe := range []string{"probe-a", "probe-b"} {
		c.checkObject(probe, "racer-volume", "/object-0", 1)
	}

	if got := len(c.hits()); got != len(hits) {
		c.t.Fatalf("warm reads reached origin: before=%d after=%d", len(hits), got)
	}

	if c.peerRequests() == 0 {
		c.t.Fatal("no peer HTTP forwarding observed")
	}

	for _, probe := range []string{"probe-a", "probe-b"} {
		for _, method := range []string{"HEAD", "GET"} {
			r := c.fetch(probe, method, serviceURL("racer-volume", "/missing-"+probe+"-"+method), "")
			if r.Status != 404 {
				c.t.Fatalf("missing object: %d", r.Status)
			}
		}
	}
}

func (c *cluster) peerRequests() float64 {
	c.t.Helper()

	pods, err := c.pods(dataplaneSelector)
	if err != nil {
		c.t.Fatal(err)
	}

	var peerRequests float64

	for _, p := range pods {
		r := c.fetch("probe-a", "GET", "http://"+p.Status.PodIP+":9090/metrics", "")
		for _, line := range strings.Split(string(r.Body), "\n") {
			if strings.HasPrefix(line, "racer_dataplane_upstream_requests_total{") && strings.Contains(line, `destination="peer",transport="http"`) {
				fields := strings.Fields(line)
				if len(fields) == 2 {
					n, err := strconv.ParseFloat(fields[1], 64)
					if err != nil {
						c.t.Fatal(err)
					}

					peerRequests += n
				}
			}
		}
	}

	return peerRequests
}

func (c *cluster) leader() core.Pod {
	var leader core.Pod

	c.await("exactly one serving leader", func() error {
		// Ready standbys retain TLS identity; only the Service's routing label
		// identifies the replica currently serving leader-only requests.
		pods, err := c.pods(controlSelector + "," + racermeta.MetadataPrefix + "serving-leader=true")
		if err != nil {
			return err
		}

		count := 0

		for _, p := range pods {
			if podReady(p) {
				leader = p
				count++
			}
		}

		if count != 1 {
			return fmt.Errorf("%d Ready serving-leader pods", count)
		}

		return nil
	})

	return leader
}

func (c *cluster) subscriptionPath() string {
	pods, err := c.pods(dataplaneSelector)
	if err != nil {
		c.t.Fatal(err)
	}

	var node core.Node
	if err := c.get("node", pods[0].Spec.NodeName, &node); err != nil {
		c.t.Fatal(err)
	}

	return "/" + racermeta.UniverseIDForSite(primarySite) + "/" + racermeta.Identity("node", string(node.UID))
}

func (c *cluster) checkSubscription() {
	c.leader()
	path := c.subscriptionPath()
	base := "https://racer-controlplane:8443"
	// These probes have no enrolled identity or trusted Racer CA. TLS must
	// reject them before serving any control command.
	if _, err := c.request("probe-a", "GET", base+"/v3"+path, "", "X-Racer-Boot: "+strings.Repeat("01", 32), "X-Racer-Profile: 1"); err == nil {
		c.t.Fatal("unauthenticated TLS subscription succeeded")
	}
}

// Cluster lifecycle and deployment setup. Cleanup is registered before creation
// so a failed build or partial cluster still follows the same teardown path.

const (
	namespace         = "unbounded-system"
	primarySite       = "racer-a"
	controlSelector   = racermeta.MetadataPrefix + "component=racer-controlplane"
	dataplaneSelector = racermeta.DataplaneLabelKey + "=true"
)

type (
	images  struct{ control, data, fixture, operator string }
	cluster struct {
		t                           *testing.T
		name, dir, kubeconfig, root string
		images                      images
	}
)

func command(timeout time.Duration, input []byte, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	return commandContext(ctx, input, name, args...)
}

func commandContext(ctx context.Context, input []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = bytes.NewReader(input)
	// A descendant holding the output pipe must not defeat the command deadline.
	cmd.WaitDelay = 2 * time.Second

	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}

		return out, fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, out)
	}

	return out, nil
}

func buildImages(t *testing.T, root string) images {
	t.Helper()

	for _, tool := range []string{"docker", "kind", "kubectl", "python3", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("e2e requires %s: %v", tool, err)
		}
	}

	if out, err := command(15*time.Second, nil, "docker", "info"); err != nil {
		t.Fatalf("Docker unavailable: %v\n%s", err, out)
	}

	if os.Getenv("RACER_E2E_IMAGE_TAG") != "" {
		return images{
			prebuiltImage(t, "racer-controlplane"), prebuiltImage(t, "racer-dataplane"),
			prebuiltImage(t, "racer-fixture"), prebuiltImage(t, "unbounded-operator"),
		}
	}

	tag := fmt.Sprintf("e2e-%d", time.Now().UnixNano())
	im := images{"racer-controlplane:" + tag, "racer-dataplane:" + tag, "racer-fixture:" + tag, "unbounded-operator:" + tag}

	var built []string

	t.Cleanup(func() {
		if os.Getenv("RACER_E2E_KEEP") == "1" || len(built) == 0 {
			return
		}

		if _, err := command(30*time.Second, nil, "docker", append([]string{"image", "rm"}, built...)...); err != nil {
			t.Error(err)
		}
	})
	// Share one build budget so slow builds cannot each consume the suite timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// Snapshot tracked working-tree bytes without traversing unrelated worktrees
	// or root-owned scratch. New source files must be added to the Git index.
	buildContext := filepath.Join(t.TempDir(), "source")
	if _, err := commandContext(ctx, nil, "python3", filepath.Join(root, "hack/scripts/racer-source-context.py"), buildContext); err != nil {
		t.Fatal(err)
	}

	for _, build := range []struct{ image, containerfile string }{
		{im.control, "images/racer-controlplane/Containerfile"},
		{im.data, "images/racer-dataplane/Containerfile"},
		{im.fixture, "e2e/racer/fixture/Containerfile"},
		{im.operator, "images/unbounded-operator/Containerfile"},
	} {
		t.Logf("building %s", build.image)

		started := time.Now()

		if _, err := commandContext(ctx, nil, "docker", "build", "-t", build.image, "-f", filepath.Join(buildContext, build.containerfile), buildContext); err != nil {
			t.Fatal(err)
		}

		t.Logf("built %s in %s", build.image, time.Since(started))
		built = append(built, build.image)
	}

	return im
}

// CI builds from the current checkout with persistent layer caching. Fail on a
// missing image instead of silently rebuilding or pulling an unrelated image.
func prebuiltImage(t *testing.T, component string) string {
	t.Helper()

	image := component + ":" + os.Getenv("RACER_E2E_IMAGE_TAG")
	if _, err := command(15*time.Second, nil, "docker", "image", "inspect", image); err != nil {
		t.Fatalf("required prebuilt image %s: %v", image, err)
	}

	t.Logf("using prebuilt image %s", image)

	return image
}

func newCluster(t *testing.T, root string, im images) *cluster {
	t.Helper()

	base := os.Getenv("RACER_E2E_DIR")
	if base == "" {
		base = filepath.Join(root, "e2e", "racer", ".artifacts")
	}

	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}

	base, err := filepath.Abs(base)
	if err != nil {
		t.Fatal(err)
	}

	dir, err := os.MkdirTemp(base, "racer-kind-")
	if err != nil {
		t.Fatal(err)
	}

	c := &cluster{t: t, name: filepath.Base(dir), dir: dir, kubeconfig: filepath.Join(dir, "kubeconfig"), root: root, images: im}
	t.Logf("cluster %s; artifacts and cache: %s", c.name, dir)
	t.Cleanup(func() {
		if t.Failed() {
			c.diagnostics()
		}

		if os.Getenv("RACER_E2E_KEEP") == "1" {
			t.Logf("retained cluster: KUBECONFIG=%s; kind delete cluster --name %s", c.kubeconfig, c.name)
			return
		}

		if !t.Failed() {
			// Kubelet creates this bind-mounted child as root. Let the host test
			// user remove its contents after the node containers are deleted.
			for _, node := range []string{c.name + "-worker", c.name + "-worker2"} {
				if _, err := command(15*time.Second, nil, "docker", "exec", node, "chmod", "0777", "/var/lib/racer-parent/cache"); err != nil {
					t.Error(err)
				}
			}
		}

		if _, err := command(time.Minute, nil, "kind", "delete", "cluster", "--name", c.name, "--kubeconfig", c.kubeconfig); err != nil {
			t.Error(err)
			return
		}

		if !t.Failed() {
			if err := os.RemoveAll(dir); err != nil {
				t.Error(err)
			}
		} else {
			t.Logf("failure artifacts retained in %s", dir)
		}
	})

	config := "kind: Cluster\napiVersion: kind.x-k8s.io/v1alpha4\nnodes:\n- role: control-plane\n"

	for i := 0; i < 2; i++ {
		cache := filepath.Join(dir, fmt.Sprintf("cache-%d", i))
		if err := os.Mkdir(cache, 0o755); err != nil {
			t.Fatal(err)
		}
		// Bind only the ext4 parent: kubelet must create the cache child itself.
		config += fmt.Sprintf("- role: worker\n  extraMounts:\n  - hostPath: %q\n    containerPath: /var/lib/racer-parent\n", cache)
	}

	configPath := filepath.Join(dir, "kind.yaml")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	image := os.Getenv("RACER_E2E_NODE_IMAGE")
	if image == "" {
		image = "kindest/node:v1.33.1"
	}

	if _, err := command(5*time.Minute, nil, "kind", "create", "cluster", "--retain", "--name", c.name, "--image", image, "--kubeconfig", c.kubeconfig, "--config", configPath, "--wait", "120s"); err != nil {
		t.Fatal(err)
	}

	if _, err := command(3*time.Minute, nil, "kind", "load", "docker-image", "--name", c.name, im.control, im.data, im.fixture, im.operator); err != nil {
		t.Fatal(err)
	}

	return c
}

func (c *cluster) kubectl(input []byte, args ...string) ([]byte, error) {
	return command(20*time.Second, input, "kubectl", append([]string{"--kubeconfig", c.kubeconfig, "--request-timeout=10s", "-n", namespace}, args...)...)
}

func (c *cluster) must(args ...string) []byte {
	c.t.Helper()

	b, err := c.kubectl(nil, args...)
	if err != nil {
		c.t.Fatal(err)
	}

	return b
}

func (c *cluster) apply(value any) {
	c.t.Helper()

	if cache, ok := value.(*racerapi.ClusterCache); ok {
		if err := validateFixtureCache(cache); err != nil {
			c.t.Fatal(err)
		}
	}

	b, err := json.Marshal(value)
	if err != nil {
		c.t.Fatal(err)
	}

	if _, err := c.kubectl(b, "apply", "-f", "-"); err != nil {
		c.t.Fatal(err)
	}
}

func (c *cluster) get(kind, name string, value any) error {
	args := []string{"get", kind, "-o", "json"}
	if name != "" {
		args = append(args, name)
	}

	b, err := c.kubectl(nil, args...)
	if err != nil {
		return err
	}

	return json.Unmarshal(b, value)
}

func (c *cluster) await(description string, check func() error) {
	c.t.Helper()
	c.awaitFor(90*time.Second, description, check)
}

func (c *cluster) awaitFor(timeout time.Duration, description string, check func() error) {
	c.t.Helper()
	// Coordinated retirement and Pod replacement can naturally take 30–60 seconds.
	deadline := time.Now().Add(timeout)

	var err error
	for time.Now().Before(deadline) {
		if err = check(); err == nil {
			return
		}

		time.Sleep(time.Second)
	}

	c.t.Fatalf("timed out waiting for %s: %v", description, err)
}

// Transform the shipping examples instead of maintaining a duplicate deployment.
func (c *cluster) manifest(name string, mutate func(string, []byte) any) {
	c.t.Helper()

	f, err := os.Open(filepath.Join(c.root, "e2e", "racer", "examples", name+".yaml"))
	if err != nil {
		c.t.Fatal(err)
	}
	defer f.Close()

	decoder := yaml.NewYAMLOrJSONDecoder(f, 4096)

	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err == io.EOF {
			break
		} else if err != nil {
			c.t.Fatal(err)
		}

		if len(raw) == 0 || string(raw) == "null" {
			continue
		}

		var typ meta.TypeMeta
		if err := json.Unmarshal(raw, &typ); err != nil {
			c.t.Fatal(err)
		}

		c.apply(mutate(typ.Kind, raw))
	}
}

func (c *cluster) deploy() {
	c.installOperator()

	for _, node := range []string{c.name + "-worker", c.name + "-worker2"} {
		c.must("label", "node", node, racermeta.SiteLabelKey+"="+primarySite)
	}

	c.apply(testSite(primarySite))
	c.apply(cacheResource("racer-volume", primarySite))

	c.fixturePod("origin", c.name+"-worker", "origin")
	c.fixturePod("origin-b", c.name+"-worker2", "origin")

	c.fixturePod("probe-a", c.name+"-worker", "probe")
	c.fixturePod("probe-b", c.name+"-worker2", "probe")
	c.await("origin and probe readiness", func() error {
		for _, name := range []string{"origin", "origin-b", "probe-a", "probe-b"} {
			var p core.Pod
			if err := c.get("pod", name, &p); err != nil {
				return err
			}

			if !podReady(p) {
				return fmt.Errorf("%s is not Ready", name)
			}
		}

		return nil
	})
	c.manifest("volume", func(_ string, raw []byte) any {
		var s racerapi.ClusterCache
		decode(c.t, raw, &s)

		return &s
	})
}

func (c *cluster) fixturePod(name, node, app string, caches ...string) {
	args := []string{"serve"}

	if app == "origin" {
		caches = []string{"racer-volume"}
	}

	for _, name := range caches {
		var cache racerapi.ClusterCache
		if err := c.get("clustercache", name, &cache); err != nil {
			c.t.Fatal(err)
		}

		if _, _, err := racermeta.CacheSockets(racermeta.SocketRoot, string(cache.UID)); err != nil {
			c.t.Fatal(err)
		}

		args = append(args, string(cache.UID))
	}

	c.apply(&core.Pod{TypeMeta: meta.TypeMeta{APIVersion: "v1", Kind: "Pod"}, ObjectMeta: meta.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{"app": app}}, Spec: core.PodSpec{
		NodeName: node, SecurityContext: &core.PodSecurityContext{RunAsUser: ptr.To(int64(65532)), RunAsGroup: ptr.To(int64(65532)), RunAsNonRoot: ptr.To(true)},
		Volumes: []core.Volume{{Name: "sockets", VolumeSource: core.VolumeSource{HostPath: &core.HostPathVolumeSource{Path: racermeta.SocketRoot, Type: ptr.To(core.HostPathDirectory)}}}},
		Containers: []core.Container{{
			Name: "fixture", Image: c.images.fixture, ImagePullPolicy: core.PullNever, Args: args,
			Env:            []core.EnvVar{{Name: "NODE_NAME", ValueFrom: &core.EnvVarSource{FieldRef: &core.ObjectFieldSelector{FieldPath: "spec.nodeName"}}}},
			VolumeMounts:   []core.VolumeMount{{Name: "sockets", MountPath: racermeta.SocketRoot}},
			ReadinessProbe: &core.Probe{ProbeHandler: core.ProbeHandler{HTTPGet: &core.HTTPGetAction{Path: "/healthz", Port: intPort(8080)}}, PeriodSeconds: 1},
		}},
	}})
}

func (c *cluster) startAlternateOrigins(cache string) {
	c.must("delete", "pod", "origin-alt", "origin-alt-b", "--ignore-not-found=true", "--wait=true")
	c.fixturePod("origin-alt", c.name+"-worker", "origin-alt", cache)
	c.fixturePod("origin-alt-b", c.name+"-worker2", "origin-alt", cache)
	c.must("wait", "--for=condition=Ready", "pod/origin-alt", "pod/origin-alt-b", "--timeout=90s")
	c.setVersion("origin-alt", 3)
}

func (c *cluster) request(probe, method, url, byteRange string, headers ...string) (fixture.Response, error) {
	// Resolve the current resource after creation, including same-name recreation.
	if strings.HasPrefix(url, "unix://") {
		name, target, ok := strings.Cut(strings.TrimPrefix(url, "unix://"), "/")
		if !ok {
			return fixture.Response{}, fmt.Errorf("invalid cache URL %q", url)
		}

		var cache racerapi.ClusterCache
		if err := c.get("clustercache", name, &cache); err != nil {
			return fixture.Response{}, err
		}

		if _, _, err := racermeta.CacheSockets(racermeta.SocketRoot, string(cache.UID)); err != nil {
			return fixture.Response{}, err
		}

		url = serviceURL(string(cache.UID), "/"+target)
	}

	b, err := c.kubectl(nil, append([]string{"exec", probe, "--", "/fixture", "request", method, url, byteRange}, headers...)...)
	if err != nil {
		return fixture.Response{}, err
	}

	var r fixture.Response

	err = json.Unmarshal(b, &r)

	return r, err
}

func decode(t *testing.T, raw []byte, into any) {
	t.Helper()

	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatal(err)
	}
}

func podReady(p core.Pod) bool {
	if p.DeletionTimestamp != nil {
		return false
	}

	for _, condition := range p.Status.Conditions {
		if condition.Type == core.PodReady && condition.Status == core.ConditionTrue {
			return true
		}
	}

	return false
}

func (c *cluster) diagnostics() {
	for _, item := range []struct {
		name string
		args []string
	}{
		{"resources.yaml", []string{"get", "pods,services,clustercaches,daemonsets,deployments,configmaps,leases", "-o", "yaml"}},
		{"events.txt", []string{"get", "events", "--sort-by=.metadata.creationTimestamp"}},
		{"describe.txt", []string{"describe", "pods"}},
		{"sites.yaml", []string{"get", "sites.unbounded-cloud.io", "-o", "yaml"}},
		{"nodes.yaml", []string{"get", "nodes", "-o", "yaml"}},
	} {
		b, _ := c.kubectl(nil, item.args...)
		_ = os.WriteFile(filepath.Join(c.dir, item.name), b, 0o600)
	}

	var pods core.PodList
	if c.get("pods", "", &pods) == nil {
		for _, p := range pods.Items {
			for _, container := range append(p.Spec.InitContainers, p.Spec.Containers...) {
				for _, previous := range []bool{false, true} {
					b, _ := c.kubectl(nil, "logs", p.Name, "-c", container.Name, fmt.Sprintf("--previous=%t", previous))
					_ = os.WriteFile(filepath.Join(c.dir, fmt.Sprintf("%s-%s-previous-%t.log", p.Name, container.Name, previous)), b, 0o600)
				}
			}

			if p.Labels[racermeta.DataplaneLabelKey] == "true" && p.Status.PodIP != "" {
				for _, path := range []string{"status", "metrics"} {
					r, err := c.request("probe-a", "GET", "http://"+p.Status.PodIP+":9090/"+path, "")
					if err == nil {
						_ = os.WriteFile(filepath.Join(c.dir, p.Name+"-"+path), r.Body, 0o600)
					}
				}
			}
		}
	}

	_, _ = command(30*time.Second, nil, "kind", "export", "logs", filepath.Join(c.dir, "kind-logs"), "--name", c.name)
}

func repository(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate repository")
	}

	return filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
}
