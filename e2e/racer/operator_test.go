//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	rbac "k8s.io/api/rbac/v1"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	machina "github.com/Azure/unbounded/api/machina/v1alpha3"
	netapi "github.com/Azure/unbounded/api/net/v1alpha1"
	racerapi "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
	"github.com/Azure/unbounded/internal/operator/component"
	operatornet "github.com/Azure/unbounded/internal/operator/components/net"
	operatorracer "github.com/Azure/unbounded/internal/operator/components/racer"
	"github.com/Azure/unbounded/internal/operator/override"
	racermeta "github.com/Azure/unbounded/internal/racer"
)

func (c *cluster) installOperator() {
	c.t.Helper()

	output := c.renderOperator()
	// Use the shipping operator RBAC and bootstrap. Its startup installs the
	// current embedded CRDs, including the Site schema used below.
	for _, name := range []string{"00-namespace", "01-serviceaccount", "02-rbac", "03-configmap", "04-deployment"} {
		b, err := os.ReadFile(filepath.Join(output, name+".yaml"))
		if err != nil {
			c.t.Fatal(err)
		}

		if name == "04-deployment" {
			c.apply(c.operatorDeployment(b))
		} else if _, err := c.kubectl(b, "apply", "-f", "-"); err != nil {
			c.t.Fatal(err)
		}

		if name == "00-namespace" {
			// Seed overrides before the manager starts its informers. Creating a
			// Site while the ConfigMap watch is still catching up could briefly
			// install net without the parking overrides and replace kind's CNI.
			c.installOverrides()
		}
	}

	c.awaitFor(5*time.Minute, "real operator and current Site CRD", func() error {
		pods, err := c.pods("app.kubernetes.io/name=unbounded-operator")
		if err != nil {
			return err
		}

		if len(pods) != 1 || !podReady(pods[0]) {
			return fmt.Errorf("operator is not Ready")
		}

		_, err = c.kubectl(nil, "get", "crd", "sites.unbounded-cloud.io")

		return err
	})
}

func (c *cluster) renderOperator() string {
	c.t.Helper()

	output := filepath.Join(c.dir, "operator")
	if err := render.Render(filepath.Join(c.root, "deploy", "unbounded-operator"), output, map[string]string{
		"Namespace": namespace, "OperatorImage": c.images.operator,
		"APIServerEndpoint": "https://kubernetes.default.svc:443", "ReapLegacyResources": "false",
	}); err != nil {
		c.t.Fatal(err)
	}

	return output
}

func (c *cluster) operatorDeployment(b []byte) *apps.Deployment {
	c.t.Helper()

	var deployment apps.Deployment
	if err := yaml.Unmarshal(b, &deployment); err != nil {
		c.t.Fatal(err)
	}

	deployment.Spec.Template.Spec.Containers[0].ImagePullPolicy = core.PullNever

	return &deployment
}

func TestOperatorInstallation(t *testing.T) {
	c := &cluster{t: t, root: repository(t), dir: t.TempDir(), images: images{operator: "unbounded-operator:test"}}
	dir := c.renderOperator()
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(dir, name+".yaml"))
		if err != nil {
			t.Fatal(err)
		}

		return b
	}

	d := c.operatorDeployment(read("04-deployment"))
	if d.Namespace != namespace || d.Spec.Template.Spec.ServiceAccountName != "unbounded-operator" || d.Spec.Template.Spec.Containers[0].Image != c.images.operator || d.Spec.Template.Spec.Containers[0].ImagePullPolicy != core.PullNever {
		t.Fatalf("operator installation identity/image mismatch: %+v", d)
	}

	var config core.ConfigMap
	if err := yaml.Unmarshal(read("03-configmap"), &config); err != nil {
		t.Fatal(err)
	}

	if config.Namespace != namespace || config.Data["UNBOUNDED_REAP_LEGACY_RESOURCES"] != "false" || config.Data["UNBOUNDED_API_SERVER_ENDPOINT"] == "" {
		t.Fatalf("invalid operator configuration: %+v", config)
	}

	decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(read("02-rbac"))), 4096)
	bound := false

	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}

		if len(raw) == 0 || string(raw) == "null" {
			continue
		}

		var binding rbac.ClusterRoleBinding
		decode(t, raw, &binding)

		if binding.Kind == "ClusterRoleBinding" {
			bound = binding.RoleRef.Name == "unbounded-operator" && len(binding.Subjects) == 1 && binding.Subjects[0].Name == d.Spec.Template.Spec.ServiceAccountName && binding.Subjects[0].Namespace == namespace
		}
	}

	if !bound {
		t.Fatal("shipping operator role is not bound to the installed identity")
	}
}

func (c *cluster) installOverrides() {
	c.apply(&core.ConfigMap{
		TypeMeta:   meta.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: meta.ObjectMeta{Name: override.ConfigMapName, Namespace: namespace},
		Data:       map[string]string{"racer-e2e.yaml": c.overrideDocument()},
	})
}

func (c *cluster) overrideDocument() string {
	// Keep kind's CNI: net is an unconditional cluster component, so park its
	// workloads using supported overrides. Other components are disabled on Site.
	// The Racer main container keeps shipping startup, Unconfined, capabilities,
	// Guaranteed CPU/memory, and memlock policy. Its one shard uses the minimum
	// supported 512 MiB sparse slab, leaving capacity for the full read campaign.
	return fmt.Sprintf(`apiVersion: %s
overrides:
  - component: net
    kind: Deployment
    patch:
      spec:
        replicas: 0
  - component: net
    kind: DaemonSet
    patch:
      spec:
        template:
          spec:
            nodeSelector:
              e2e.unbounded-cloud.io/parked: "true"
  - component: racer-controlplane
    kind: Deployment
    patch:
      spec:
        template:
          spec:
            containers:
              - name: controller
                image: %s
                imagePullPolicy: Never
                resources:
                  requests:
                    cpu: 100m
                    memory: 128Mi
  - component: racer-dataplane
    kind: DaemonSet
    patch:
      spec:
        template:
          spec:
            volumes:
              - name: e2e-cache
                hostPath:
                  path: /var/lib/racer-parent/cache
                  type: DirectoryOrCreate
            initContainers:
              - name: bootstrap
                image: %s
                imagePullPolicy: Never
            containers:
              - name: dataplane
                image: %s
                imagePullPolicy: Never
                volumeMounts:
                  - name: e2e-cache
                    mountPath: /e2e-cache
                env:
                  - name: RACER_SLAB_PATH
                    value: /e2e-cache/cache.slab
                  - name: RACER_SLAB_SIZE
                    value: "536870912"
`, override.APIVersion, c.images.control, c.images.control, c.images.data)
}

// Validate the actual fixture against the real constructors and override
// pipeline even on hosts where kind cannot boot. This does not replace live e2e.
func TestOperatorFixturePlan(t *testing.T) {
	c := &cluster{images: images{control: "racer-controlplane:test", data: "racer-dataplane:test"}}

	entries, problems, err := override.Parse(map[string]string{"e2e.yaml": c.overrideDocument()})
	if err != nil || len(problems) != 0 {
		t.Fatalf("parse fixture overrides: %v %v", err, problems)
	}

	if problems := override.Validate(entries); len(problems) != 0 {
		t.Fatalf("validate fixture overrides: %v", problems)
	}

	site := testSite(primarySite)

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{core.AddToScheme, apps.AddToScheme, racerapi.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}

	cache := cacheResource("racer-volume", primarySite)
	cache.UID = "racer-volume-uid"
	env := &component.Env{Namespace: namespace, Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(cache).Build()}

	control, _, err := operatorracer.NewControlPlane().Plan(t.Context(), env, []machina.Site{*site})
	if err != nil {
		t.Fatal(err)
	}

	data, _, err := operatorracer.NewDataplane().Plan(t.Context(), env, []machina.Site{*site})
	if err != nil {
		t.Fatal(err)
	}

	// Snapshot before applying overrides. TestShippingDataplaneProfile owns the
	// exact startup policy; this fixture must preserve the entire shipping script.
	var shipping *core.Container

	for _, op := range data.Operations {
		if op.Object.GetKind() != "DaemonSet" {
			continue
		}

		b, err := json.Marshal(op.Object)
		if err != nil {
			t.Fatal(err)
		}

		var ds apps.DaemonSet
		decode(t, b, &ds)

		if shipping != nil || len(ds.Spec.Template.Spec.Containers) != 1 {
			t.Fatal("expected one shipping dataplane DaemonSet with one main container")
		}

		shipping = ds.Spec.Template.Spec.Containers[0].DeepCopy()
	}

	if shipping == nil {
		t.Fatal("shipping dataplane DaemonSet missing")
	}

	control.Operations = append(control.Operations, data.Operations...)

	netPlan, _, err := operatornet.New().Plan(t.Context(), env, []machina.Site{*site})
	if err != nil {
		t.Fatal(err)
	}

	control.Operations = append(control.Operations, netPlan.Operations...)

	report := override.Apply(control, entries, []string{site.Name})
	if report.Failed() || len(report.Workloads) != 4 {
		t.Fatalf("apply fixture overrides: %+v", report)
	}

	dataplaneChecked := false

	for _, op := range control.Operations {
		if op.Component == "net" {
			b, err := json.Marshal(op.Object)
			if err != nil {
				t.Fatal(err)
			}

			switch op.Object.GetKind() {
			case "Deployment":
				var d apps.Deployment
				decode(t, b, &d)

				if ptr.Deref(d.Spec.Replicas, 1) != 0 {
					t.Fatal("net controller was not parked")
				}
			case "DaemonSet":
				var d apps.DaemonSet
				decode(t, b, &d)

				if d.Spec.Template.Spec.NodeSelector["e2e.unbounded-cloud.io/parked"] != "true" {
					t.Fatal("net node agent was not parked")
				}
			}

			continue
		}

		if op.Object.GetKind() != "DaemonSet" {
			continue
		}

		b, err := json.Marshal(op.Object)
		if err != nil {
			t.Fatal(err)
		}

		var ds apps.DaemonSet
		decode(t, b, &ds)

		if dataplaneChecked || len(ds.Spec.Template.Spec.Containers) != 1 || len(ds.Spec.Template.Spec.InitContainers) != 1 {
			t.Fatal("expected one fixture dataplane DaemonSet with main and bootstrap containers")
		}

		dataplaneChecked = true
		main := ds.Spec.Template.Spec.Containers[0]

		if main.Image != c.images.data || main.SecurityContext == nil || main.SecurityContext.SeccompProfile == nil || main.SecurityContext.SeccompProfile.Type != core.SeccompProfileTypeUnconfined {
			t.Fatal("fixture lost the shipping runtime image or seccomp profile")
		}

		if !slices.Equal(main.Command, shipping.Command) || !slices.Equal(main.Args, shipping.Args) {
			t.Fatalf("fixture changed shipping startup: command = %q, want %q; args = %q, want %q", main.Command, shipping.Command, main.Args, shipping.Args)
		}

		for name, value := range map[string]string{"RACER_SLAB_SIZE": "536870912", "RACER_SHARDS": "1"} {
			index := slices.IndexFunc(main.Env, func(env core.EnvVar) bool { return env.Name == name })
			if index < 0 || main.Env[index].Value != value || main.Env[index].ValueFrom != nil {
				t.Fatalf("fixture needs one supported 512 MiB shard: %s must be %s", name, value)
			}
		}

		for _, container := range []core.Container{main, ds.Spec.Template.Spec.InitContainers[0]} {
			if container.Resources.Requests.Cpu().String() != "3" || container.Resources.Limits.Cpu().String() != "3" || container.Resources.Requests.Memory().String() != "4Gi" || container.Resources.Limits.Memory().String() != "4Gi" {
				t.Fatal("fixture changed shipping Guaranteed CPU/memory resources")
			}
		}
	}

	if !dataplaneChecked {
		t.Fatal("fixture dataplane DaemonSet missing")
	}
}

func testSite(name string) *machina.Site {
	disabled := machina.SiteComponentSpec{Enabled: ptr.To(false)}

	return &machina.Site{
		TypeMeta:   meta.TypeMeta{APIVersion: machina.GroupVersion.String(), Kind: "Site"},
		ObjectMeta: meta.ObjectMeta{Name: name, Labels: map[string]string{"kubernetes.io/metadata.name": name}},
		Spec: machina.SiteSpec{
			NodeCidrs:          []string{"172.18.0.0/16"},
			PodCidrAssignments: []netapi.PodCidrAssignment{{CidrBlocks: []string{"10.244.0.0/16"}}},
			ManageCniPlugin:    ptr.To(false),
			Components: machina.SiteComponents{
				Machina:        &machina.MachinaComponentSpec{SiteComponentSpec: disabled},
				Metalman:       &machina.MetalmanComponentSpec{SiteComponentSpec: disabled},
				Gantry:         &machina.GantryComponentSpec{SiteComponentSpec: disabled},
				TokenRefresher: &machina.TokenRefresherComponentSpec{SiteComponentSpec: disabled},
			},
		},
	}
}

func cacheResource(name, site string) *racerapi.ClusterCache {
	return &racerapi.ClusterCache{
		TypeMeta:   meta.TypeMeta{APIVersion: racerapi.GroupVersion.String(), Kind: "ClusterCache"},
		ObjectMeta: meta.ObjectMeta{Name: name},
		Spec:       racerapi.ClusterCacheSpec{SiteSelector: meta.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": site}}, CacheGeneration: 1, MaxCandidateAttempts: 3},
	}
}

func (c *cluster) membershipChanges() {
	c.t.Log("live exclusion, re-enrollment, and independent Site universes")
	keys := c.caSecrets()
	node := c.name + "-worker2"
	c.must("label", "node", node, racermeta.ExcludeLabelKey+"=true", "--overwrite")
	c.awaitMembership(map[string]string{c.name + "-worker": primarySite})
	c.awaitVolume("probe-a", "racer-volume", "/after-exclusion", 2)
	c.checkSiteVolumeIsolation(primarySite, "racer-volume")
	c.must("label", "node", node, racermeta.ExcludeLabelKey+"-")
	c.converge(0, "racer-volume")
	c.awaitVolume("probe-b", "racer-volume", "/after-reenrollment", 2)

	const secondSite = "racer-b"
	c.apply(testSite(secondSite))
	c.apply(cacheResource("site-b-volume", secondSite))
	c.startAlternateOrigins("site-b-volume")
	c.must("label", "node", node, racermeta.SiteLabelKey+"="+secondSite, "--overwrite")
	c.awaitMembership(map[string]string{c.name + "-worker": primarySite, node: secondSite})
	c.awaitVolume("probe-a", "racer-volume", "/site-isolation", 2)
	c.awaitVolume("probe-b", "site-b-volume", "/site-isolation", 3)
	c.checkSiteVolumeIsolation(primarySite, "racer-volume")
	c.checkSiteVolumeIsolation(secondSite, "site-b-volume")

	// Site metadata changes do not remove runtime participation.
	c.must("annotate", "sites.unbounded-cloud.io", secondSite, "e2e.unbounded-cloud.io/updated=true")
	c.awaitMembership(map[string]string{c.name + "-worker": primarySite, node: secondSite})
	c.checkSiteVolumeIsolation(secondSite, "site-b-volume")
	c.leader()
	c.checkCAUnchanged(keys)
	c.must("delete", "clustercache/site-b-volume")
	c.must("label", "node", node, racermeta.SiteLabelKey+"="+primarySite, "--overwrite")
	c.converge(0, "racer-volume")
	c.awaitVolume("probe-b", "racer-volume", "/after-site-return", 2)
	c.checkSubscription()
}

func (c *cluster) awaitMembership(want map[string]string) {
	c.awaitFor(3*time.Minute, "Site dataplane membership", func() error {
		pods, err := c.pods(dataplaneSelector)
		if err != nil {
			return err
		}

		if len(pods) != len(want) {
			return fmt.Errorf("got %d dataplanes, want %d", len(pods), len(want))
		}

		seen := map[string]bool{}

		for _, pod := range pods {
			site, exists := want[pod.Spec.NodeName]
			if !exists || seen[pod.Spec.NodeName] || !podReady(pod) {
				return fmt.Errorf("unexpected/unready dataplane %s on %s: %v", pod.Name, pod.Spec.NodeName, pod.Labels)
			}

			seen[pod.Spec.NodeName] = true

			var ds apps.DaemonSet
			if err := c.get("daemonset", "racer-dataplane", &ds); err != nil {
				return err
			}

			var node core.Node
			if err := c.get("node", pod.Spec.NodeName, &node); err != nil {
				return err
			}

			if len(ds.OwnerReferences) != 0 || len(pod.OwnerReferences) != 1 || pod.OwnerReferences[0].UID != ds.UID || racermeta.NodeSite(&node) != site {
				return fmt.Errorf("%s has unexpected singleton ownership or Site", pod.Name)
			}

			identity, err := c.kubectl(nil, "exec", pod.Name, "-c", "dataplane", "--", "/bin/sh", "-c", "cat /bootstrap/identity")
			if err != nil || !strings.Contains(string(identity), "export RACER_UNIVERSE="+racermeta.UniverseIDForSite(site)+"\n") {
				return fmt.Errorf("%s has stale bootstrap identity: %s (%v)", pod.Name, identity, err)
			}
		}

		return nil
	})
}

func (c *cluster) awaitVolume(probe, volume, target string, version int) {
	c.awaitFor(3*time.Minute, "local Unix cache path "+volume, func() error {
		r, err := c.request(probe, "HEAD", serviceURL(volume, target), "")
		if err != nil {
			return err
		}

		if r.Status != 200 {
			return fmt.Errorf("%s returned %d", volume, r.Status)
		}

		return nil
	})
	c.checkObject(probe, volume, target, version)
}

func (c *cluster) checkSiteVolumeIsolation(site, volume string) {
	c.await("isolated TLS configuration for "+site, func() error {
		all, err := c.pods(dataplaneSelector)
		if err != nil {
			return err
		}

		var pods []core.Pod

		for _, pod := range all {
			var node core.Node
			if err := c.get("node", pod.Spec.NodeName, &node); err != nil {
				return err
			}

			if racermeta.NodeSite(&node) == site {
				pods = append(pods, pod)
			}
		}

		if len(pods) != 1 {
			return fmt.Errorf("expected one member of %s", site)
		}

		var node core.Node
		if err := c.get("node", pods[0].Spec.NodeName, &node); err != nil {
			return err
		}

		identity, err := c.kubectl(nil, "exec", pods[0].Name, "-c", "dataplane", "--", "/bin/sh", "-c", "cat /bootstrap/identity")
		if err != nil {
			return err
		}

		for _, expected := range []string{
			"export RACER_UNIVERSE=" + racermeta.UniverseIDForSite(site) + "\n",
			"export RACER_NODE=" + racermeta.Identity("node", string(node.UID)) + "\n",
		} {
			if !strings.Contains(string(identity), expected) {
				return fmt.Errorf("bootstrap identity does not match Site/Node: %s", identity)
			}
		}

		r, err := c.request("probe-a", "GET", "http://"+pods[0].Status.PodIP+":9090/status", "")
		if err != nil {
			return err
		}

		var s status
		if err := json.Unmarshal(r.Body, &s); err != nil {
			return err
		}

		var cache racerapi.ClusterCache
		if err := c.get("clustercache", volume, &cache); err != nil {
			return err
		}

		if r.Status != 200 || !s.Ready || s.Rejected || s.TLS.TrustDigest == "" || s.ActiveRevision != s.CandidateRevision || s.ActivatedWorkers != s.Workers || len(s.Volumes) != 1 || s.Volumes[0].ID != string(cache.UID) || !s.Volumes[0].Ready {
			return fmt.Errorf("unexpected %s configuration: %s", site, r.Body)
		}

		if cache.Status.ObservedGeneration != cache.Generation || !apiMeta.IsStatusConditionTrue(cache.Status.Conditions, "Ready") || cache.Status.Participants.Desired != 1 || cache.Status.Participants.Ready != 1 {
			return fmt.Errorf("%s has not converged: %+v", volume, cache.Status)
		}

		return nil
	})
}
