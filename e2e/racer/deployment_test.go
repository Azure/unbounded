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
	discovery "k8s.io/api/discovery/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/yaml"

	"github.com/Azure/unbounded/e2e/racer/fixture"
	racermeta "github.com/Azure/unbounded/internal/racer"
	"github.com/Azure/unbounded/internal/racer/pki"
)

func intPort(port int) intstr.IntOrString { return intstr.FromInt(port) }

func TestDeployment(t *testing.T) {
	root := repository(t)
	im := buildImages(t, root)
	c := newCluster(t, root, im)
	c.deploy()
	t.Log("bootstrap, service routing, and subscription")

	revision := c.converge(0, "racer-volume")
	c.checkSubscription()
	keys := c.caSecrets()

	t.Log("staged CA rotation with continuous traffic and leader failover")
	c.rotateCA(keys)
	keys = c.caSecrets() // Persistence checks start after deliberate rotation.
	c.converge(revision-1, "racer-volume")
	t.Log("HEAD, GET, range, missing objects, cache hits, and peer forwarding")
	c.readsAndCaching()
	t.Log("origin Service recreation updates numeric endpoint without changing identity")

	var origin core.Service
	if err := c.get("service", "origin", &origin); err != nil {
		t.Fatal(err)
	}

	oldIP := origin.Spec.ClusterIP

	c.must("delete", "service/origin")
	// Occupy the old address so recreation necessarily exercises an IP change.
	c.apply(&core.Service{TypeMeta: meta.TypeMeta{APIVersion: "v1", Kind: "Service"}, ObjectMeta: meta.ObjectMeta{Name: "old-origin-address", Namespace: namespace}, Spec: core.ServiceSpec{ClusterIP: oldIP, Ports: []core.ServicePort{{Port: 8080}}}})
	origin.ObjectMeta = meta.ObjectMeta{Name: "origin", Namespace: namespace}
	origin.Spec.ClusterIP = ""
	origin.Spec.ClusterIPs = nil
	origin.Status = core.ServiceStatus{}
	c.apply(&origin)
	revision = c.converge(revision, "racer-volume")

	before := len(c.hits())
	for _, probe := range []string{"probe-a", "probe-b"} {
		c.checkObject(probe, "racer-volume", "/object-0", 1)
	}

	if len(c.hits()) != before {
		t.Fatal("origin IP change invalidated warm cache")
	}

	c.checkObject("probe-a", "racer-volume", "/after-origin-recreation", 1)
	t.Log("cache-generation update")
	c.setVersion("origin", 2)
	c.must("annotate", "service/racer-volume", racermeta.CacheGenerationAnnotationKey+"=2", "--overwrite")

	revision = c.converge(revision, "racer-volume")
	for _, probe := range []string{"probe-a", "probe-b"} {
		c.checkObject(probe, "racer-volume", "/object-0", 2)
	}

	t.Log("independent second volume and removal")
	c.setVersion("origin-alt", 3)
	c.apply(volumeService("second-volume", "origin-alt", primarySite))
	revision = c.converge(revision, "racer-volume", "second-volume")

	var first, second core.Service
	if err := c.get("service", "racer-volume", &first); err != nil {
		t.Fatal(err)
	}

	if err := c.get("service", "second-volume", &second); err != nil {
		t.Fatal(err)
	}

	if first.Spec.Ports[0].TargetPort == second.Spec.Ports[0].TargetPort {
		t.Fatal("volumes share listener")
	}

	for _, probe := range []string{"probe-a", "probe-b"} {
		c.checkObject(probe, "second-volume", "/object-0", 3)
		c.checkObject(probe, "racer-volume", "/object-0", 2)
	}

	c.must("delete", "service/second-volume", "--wait=false")
	revision = c.converge(revision, "racer-volume")

	t.Log("controller leader replacement and durable revision continuity")

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
	c.must("annotate", "service/racer-volume", racermeta.CacheGenerationAnnotationKey+"=3", "--overwrite")
	revision = c.converge(revision, "racer-volume")
	c.checkSubscription()
	c.checkCAUnchanged(keys)
	t.Log("all controller replicas restart and reuse the CA")
	c.must("delete", "pods", "-l", controlSelector, "--wait=false")
	c.leader()
	c.must("annotate", "service/racer-volume", racermeta.CacheGenerationAnnotationKey+"=4", "--overwrite")
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
	// A process restart need not change topology revision if the Pod IP is reused.
	c.converge(revision-1, "racer-volume")

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
func (c *cluster) caSecrets() map[string]core.Secret {
	c.t.Helper()

	var secret core.Secret
	if err := c.get("secret", pki.SecretName, &secret); err != nil {
		c.t.Fatal(err)
	}

	var state struct {
		Active      string
		Authorities []struct{ Digest, Certificate, PrivateKey string }
	}
	decode(c.t, secret.Data[pki.StateKey], &state)

	bundle := c.trustBundle()
	if secret.Type != core.SecretTypeOpaque || len(state.Authorities) == 0 || state.Active != bundle.Active {
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
		// Leadership fencing and enrolled member records legitimately change.
		var a, b struct {
			Active      string
			Authorities json.RawMessage
		}
		decode(c.t, old.Data[pki.StateKey], &a)
		decode(c.t, current.Data[pki.StateKey], &b)

		var oldRoots, newRoots []struct{ Certificate, PrivateKey, Digest string }
		decode(c.t, a.Authorities, &oldRoots)
		decode(c.t, b.Authorities, &newRoots)

		oldWire, _ := json.Marshal(oldRoots)

		newWire, _ := json.Marshal(newRoots)
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
	c.must("annotate", "configmap/"+pki.ConfigMapName, pki.RotationAnnotation+"="+strconv.FormatInt(time.Now().UnixNano(), 10), "--overwrite")
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
	c.awaitFor(5*time.Minute, "new issuer and live trust projection", func() error {
		checkTraffic()

		bundle := c.trustBundle()
		if bundle.Active == old.Active {
			return fmt.Errorf("waiting for verified overlap barriers")
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

func (c *cluster) trustBundle() pki.TrustBundle {
	c.t.Helper()

	var cm core.ConfigMap
	if err := c.get("configmap", pki.ConfigMapName, &cm); err != nil {
		c.t.Fatal(err)
	}

	bundle, err := pki.ParseBundle([]byte(cm.Data[pki.BundleKey]))
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

	c.await("all workers and EndpointSlices converged", func() error {
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
				found := false

				for _, v := range s.Volumes {
					if v.ID == namespace+"/"+name && v.Ready && v.Epoch > 0 {
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
			var svc core.Service
			if err := c.get("service", name, &svc); err != nil {
				return err
			}

			port, err := strconv.Atoi(svc.Annotations[racermeta.AllocatedPortAnnotationKey])
			if err != nil || port < 10000 || port > 29999 || svc.Spec.Ports[0].TargetPort != intPort(port) || svc.Spec.InternalTrafficPolicy == nil || *svc.Spec.InternalTrafficPolicy != core.ServiceInternalTrafficPolicyLocal {
				return fmt.Errorf("service routing not reconciled: %+v", svc)
			}

			wantStatus := "Published"
			if !strings.HasPrefix(svc.Annotations[racermeta.StatusAnnotationKey], wantStatus) {
				return fmt.Errorf("service status: %s", svc.Annotations[racermeta.StatusAnnotationKey])
			}

			b, err := c.kubectl(nil, "get", "endpointslices", "-l", "kubernetes.io/service-name="+name, "-o", "json")
			if err != nil {
				return err
			}

			var slices discovery.EndpointSliceList
			decode(c.t, b, &slices)

			ready := map[string]bool{}

			for _, slice := range slices.Items {
				if len(slice.Ports) != 1 || slice.Ports[0].Port == nil || *slice.Ports[0].Port != int32(port) {
					return fmt.Errorf("%s EndpointSlice target port not reconciled", name)
				}

				for _, endpoint := range slice.Endpoints {
					if endpoint.Conditions.Ready != nil && *endpoint.Conditions.Ready && endpoint.NodeName != nil {
						for _, addr := range endpoint.Addresses {
							ready[*endpoint.NodeName+"/"+addr] = true
						}
					}
				}
			}

			for _, p := range pods {
				if !ready[p.Spec.NodeName+"/"+p.Status.PodIP] {
					return fmt.Errorf("%s lacks Ready endpoint on %s", name, p.Spec.NodeName)
				}
			}
			// API convergence precedes kube-proxy rule installation. Verify the
			// actual Service path on both nodes before starting traffic assertions.
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
	return "http://" + service + "." + namespace + ".svc" + target
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
	r := c.fetch("probe-a", "GET", "http://origin:8080/hits", "")
	if r.Status != 200 {
		c.t.Fatalf("origin hits: %d", r.Status)
	}

	var hits []fixture.Hit
	decode(c.t, r.Body, &hits)

	return hits
}

func (c *cluster) setVersion(origin string, version int) {
	r := c.fetch("probe-a", "POST", fmt.Sprintf("http://%s:8080/version?value=%d", origin, version), "")
	if r.Status != 200 {
		c.t.Fatalf("set origin version: %d %s", r.Status, r.Body)
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
	// through both nodes. Both fetches must come from the same physical owner.
	for _, target := range targets {
		methods := map[string]int{}
		sources := map[string]bool{}

		for _, hit := range hits {
			if hit.Target == target {
				methods[hit.Method]++
				sources[hit.Source] = true
			}
		}

		if methods["HEAD"] != 1 || methods["GET"] != 1 || len(sources) != 1 {
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
		pods, err := c.pods(controlSelector)
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
			return fmt.Errorf("%d Ready controller pods", count)
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

	for _, tool := range []string{"docker", "kind", "kubectl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("e2e requires %s: %v", tool, err)
		}
	}

	if out, err := command(15*time.Second, nil, "docker", "info"); err != nil {
		t.Fatalf("Docker unavailable: %v\n%s", err, out)
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

	for _, build := range []struct{ image, component string }{
		{im.control, "racer-controlplane"},
		{im.data, "racer-dataplane"},
		{im.fixture, "racer-fixture"},
		{im.operator, "unbounded-operator"},
	} {
		t.Logf("building %s", build.image)

		if _, err := commandContext(ctx, nil, "docker", "build", "-t", build.image, "-f", filepath.Join(root, "images", build.component, "Containerfile"), root); err != nil {
			t.Fatal(err)
		}

		built = append(built, build.image)
	}

	return im
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

		if _, err := command(time.Minute, nil, "kind", "delete", "cluster", "--name", c.name); err != nil {
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

	if service, ok := value.(*core.Service); ok {
		if err := validateFixtureVolume(service); err != nil {
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

	for _, name := range []string{"origin", "origin-alt"} {
		c.fixturePod(name, c.name+"-worker", name)
		c.apply(&core.Service{TypeMeta: meta.TypeMeta{APIVersion: "v1", Kind: "Service"}, ObjectMeta: meta.ObjectMeta{Name: name, Namespace: namespace}, Spec: core.ServiceSpec{Selector: map[string]string{"app": name}, Ports: []core.ServicePort{{Port: 8080}}}})
	}

	c.fixturePod("probe-a", c.name+"-worker", "probe")
	c.fixturePod("probe-b", c.name+"-worker2", "probe")
	c.await("origin and probe readiness", func() error {
		for _, name := range []string{"origin", "origin-alt", "probe-a", "probe-b"} {
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
		var s core.Service
		decode(c.t, raw, &s)
		delete(s.Annotations, racermeta.ListenerPortAnnotationKey) // Exercise allocation and targetPort patching.
		s.Spec.Ports[0].TargetPort.IntVal = 12345

		return &s
	})
}

func (c *cluster) fixturePod(name, node, app string) {
	c.apply(&core.Pod{TypeMeta: meta.TypeMeta{APIVersion: "v1", Kind: "Pod"}, ObjectMeta: meta.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{"app": app}}, Spec: core.PodSpec{NodeName: node, Containers: []core.Container{{Name: "fixture", Image: c.images.fixture, ImagePullPolicy: core.PullNever, Args: []string{"serve"}, ReadinessProbe: &core.Probe{ProbeHandler: core.ProbeHandler{HTTPGet: &core.HTTPGetAction{Path: "/healthz", Port: intPort(8080)}}, PeriodSeconds: 1}}}}})
}

func (c *cluster) request(probe, method, url, byteRange string, headers ...string) (fixture.Response, error) {
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
		{"resources.yaml", []string{"get", "pods,services,endpointslices,daemonsets,deployments,configmaps,leases", "-o", "yaml"}},
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
