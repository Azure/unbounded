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
	"strings"
	"testing"
	"time"

	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	rbac "k8s.io/api/rbac/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	machina "github.com/Azure/unbounded/api/machina/v1alpha3"
	netapi "github.com/Azure/unbounded/api/net/v1alpha1"
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
	// Guaranteed CPU/memory, and memlock policy. Only its sparse slab is smaller.
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
                    value: "134217728"
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
	for _, add := range []func(*runtime.Scheme) error{core.AddToScheme, apps.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}

	env := &component.Env{Namespace: namespace, Client: fake.NewClientBuilder().WithScheme(scheme).Build()}

	control, _, err := operatorracer.NewControlPlane().Plan(t.Context(), env, []machina.Site{*site})
	if err != nil {
		t.Fatal(err)
	}

	data, _, err := operatorracer.NewDataplane().Plan(t.Context(), env, site)
	if err != nil {
		t.Fatal(err)
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

		main := ds.Spec.Template.Spec.Containers[0]

		wantCommand := strings.Join([]string{
			"ulimit -l 262144",
			". /bootstrap/identity",
			`export RACER_CONTROL_PLANE_URL="https://racer-controlplane.` + namespace + `.svc:8443/v3/$RACER_UNIVERSE/$RACER_NODE"`,
			"exec /usr/local/bin/racer-dataplane",
		}, "\n")
		if main.Image != c.images.data || main.SecurityContext.SeccompProfile.Type != core.SeccompProfileTypeUnconfined || strings.Join(main.Command, " ") != "/bin/sh -ec" || len(main.Args) != 1 || main.Args[0] != wantCommand {
			t.Fatal("fixture lost the shipping runtime startup")
		}

		for _, container := range []core.Container{main, ds.Spec.Template.Spec.InitContainers[0]} {
			if container.Resources.Requests.Cpu().String() != "3" || container.Resources.Limits.Cpu().String() != "3" || container.Resources.Requests.Memory().String() != "4Gi" || container.Resources.Limits.Memory().String() != "4Gi" {
				t.Fatal("fixture changed shipping Guaranteed CPU/memory resources")
			}
		}
	}
}

func testSite(name string) *machina.Site {
	disabled := machina.SiteComponentSpec{Enabled: ptr.To(false)}

	return &machina.Site{
		TypeMeta:   meta.TypeMeta{APIVersion: machina.GroupVersion.String(), Kind: "Site"},
		ObjectMeta: meta.ObjectMeta{Name: name},
		Spec: machina.SiteSpec{
			NodeCidrs:          []string{"172.18.0.0/16"},
			PodCidrAssignments: []netapi.PodCidrAssignment{{CidrBlocks: []string{"10.244.0.0/16"}}},
			ManageCniPlugin:    ptr.To(false),
			Components: machina.SiteComponents{
				Machina:        &machina.MachinaComponentSpec{SiteComponentSpec: disabled},
				Metalman:       &machina.MetalmanComponentSpec{SiteComponentSpec: disabled},
				Gantry:         &machina.GantryComponentSpec{SiteComponentSpec: disabled},
				TokenRefresher: &machina.TokenRefresherComponentSpec{SiteComponentSpec: disabled},
				Racer:          &machina.RacerComponentSpec{SiteComponentSpec: machina.SiteComponentSpec{Enabled: ptr.To(true)}},
			},
		},
	}
}

func volumeService(name, origin, site string) *core.Service {
	return &core.Service{
		TypeMeta: meta.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: meta.ObjectMeta{Name: name, Namespace: namespace, Annotations: map[string]string{
			racermeta.UniverseKey:                racermeta.UniverseForSite(site),
			racermeta.OriginServiceAnnotationKey: origin, racermeta.OriginPortAnnotationKey: "8080", racermeta.SlotCountAnnotationKey: "64",
		}},
		Spec: core.ServiceSpec{Selector: map[string]string{racermeta.DataplaneLabelKey: "true", racermeta.UniverseKey: racermeta.UniverseForSite(site)}, Ports: []core.ServicePort{{Port: 80, TargetPort: intPort(12345)}}},
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
	c.apply(volumeService("site-b-volume", "origin-alt", secondSite))
	c.must("label", "node", node, racermeta.SiteLabelKey+"="+secondSite, "--overwrite")
	c.awaitMembership(map[string]string{c.name + "-worker": primarySite, node: secondSite})
	c.awaitVolume("probe-a", "racer-volume", "/site-isolation", 2)
	c.awaitVolume("probe-b", "site-b-volume", "/site-isolation", 3)
	c.checkSiteVolumeIsolation(primarySite, "racer-volume")
	c.checkSiteVolumeIsolation(secondSite, "site-b-volume")

	// Disable a Site through the API and prove the operator removes its owned
	// DaemonSet while the shared control plane and CA survive.
	c.must("patch", "sites.unbounded-cloud.io", secondSite, "--type=merge", "-p", `{"spec":{"components":{"racer":{"enabled":false}}}}`)
	c.awaitMembership(map[string]string{c.name + "-worker": primarySite})
	c.await("disabled Site DaemonSet removed", func() error {
		b, err := c.kubectl(nil, "get", "daemonset", operatorracer.SiteDaemonSetName(secondSite), "--ignore-not-found", "-o", "name")
		if err != nil {
			return err
		}

		if len(b) != 0 {
			return fmt.Errorf("disabled Site still has a DaemonSet: %s", b)
		}

		return nil
	})
	c.leader()
	c.checkCAUnchanged(keys)
	c.must("delete", "service/site-b-volume")
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
			if !exists || seen[pod.Spec.NodeName] || !podReady(pod) || pod.Labels[racermeta.UniverseKey] != racermeta.UniverseForSite(site) {
				return fmt.Errorf("unexpected/unready dataplane %s on %s: %v", pod.Name, pod.Spec.NodeName, pod.Labels)
			}

			seen[pod.Spec.NodeName] = true

			var ds apps.DaemonSet
			if err := c.get("daemonset", operatorracer.SiteDaemonSetName(site), &ds); err != nil {
				return err
			}

			var owner machina.Site
			if err := c.get("sites.unbounded-cloud.io", site, &owner); err != nil {
				return err
			}

			if len(ds.OwnerReferences) != 1 || ds.OwnerReferences[0].UID != owner.UID {
				return fmt.Errorf("%s is not owned by Site %s", ds.Name, site)
			}
		}

		return nil
	})
}

func (c *cluster) awaitVolume(probe, volume, target string, version int) {
	c.awaitFor(3*time.Minute, "live Service path "+volume, func() error {
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
		pods, err := c.pods(dataplaneSelector + "," + racermeta.UniverseKey + "=" + racermeta.UniverseForSite(site))
		if err != nil {
			return err
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

		if r.Status != 200 || !s.Ready || s.Rejected || s.TLS.TrustDigest == "" || s.ActiveRevision != s.CandidateRevision || s.ActivatedWorkers != s.Workers || len(s.Volumes) != 1 || s.Volumes[0].ID != namespace+"/"+volume || !s.Volumes[0].Ready {
			return fmt.Errorf("unexpected %s configuration: %s", site, r.Body)
		}

		var svc core.Service
		if err := c.get("service", volume, &svc); err != nil {
			return err
		}

		if svc.Annotations[racermeta.UniverseIDAnnotationKey] != racermeta.UniverseIDForSite(site) || !strings.HasPrefix(svc.Annotations[racermeta.StatusAnnotationKey], "Published") {
			return fmt.Errorf("Service universe mismatch: %v", svc.Annotations)
		}

		b, err := c.kubectl(nil, "get", "endpointslices", "-l", "kubernetes.io/service-name="+volume, "-o", "json")
		if err != nil {
			return err
		}

		var endpoints discovery.EndpointSliceList
		decode(c.t, b, &endpoints)

		ready := 0

		for _, slice := range endpoints.Items {
			for _, endpoint := range slice.Endpoints {
				if !ptr.Deref(endpoint.Conditions.Ready, false) {
					continue
				}

				if ptr.Deref(endpoint.NodeName, "") != pods[0].Spec.NodeName || len(endpoint.Addresses) != 1 || endpoint.Addresses[0] != pods[0].Status.PodIP {
					return fmt.Errorf("%s has an endpoint outside Site %s: %+v", volume, site, endpoint)
				}

				ready++
			}
		}

		if ready != 1 {
			return fmt.Errorf("%s has %d Ready endpoints, want 1", volume, ready)
		}

		return nil
	})
}
