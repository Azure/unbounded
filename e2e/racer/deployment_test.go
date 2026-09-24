//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/utils/ptr"

	machina "github.com/Azure/unbounded/api/machina/v1alpha3"
	netapi "github.com/Azure/unbounded/api/net/v1alpha1"
	racerapi "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
	"github.com/Azure/unbounded/internal/operator/override"
	racermeta "github.com/Azure/unbounded/internal/racer"
)

const namespace = "unbounded-system"

// TestRacer exercises production enrollment and configuration followed by SDK
// cold, warm, and cross-page reads on one kind worker.
func TestRacer(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))

	base := os.Getenv("RACER_E2E_DIR")
	if base == "" {
		base = filepath.Join(root, "e2e/racer/.artifacts")
	}

	base, err := filepath.Abs(base)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}

	dir, err := os.MkdirTemp(base, "racer-kind-")
	if err != nil {
		t.Fatal(err)
	}

	c := &cluster{t: t, root: root, dir: dir, name: filepath.Base(dir)}
	c.ctx, c.cancel = context.WithTimeout(context.Background(), 4*time.Minute)
	c.kubeconfig = filepath.Join(dir, "kubeconfig")
	t.Cleanup(c.cleanup)

	tag := os.Getenv("RACER_E2E_IMAGE_TAG")
	if tag == "" {
		tag = "racer-e2e"
	}

	c.tag = tag

	t.Log("checking prebuilt images (make e2e-racer-build)")

	for _, name := range []string{"racer-controlplane", "racer-dataplane", "unbounded-operator", "racer-fixture"} {
		c.run(5*time.Second, nil, "docker", "image", "inspect", name+":"+tag)
	}

	cache := filepath.Join(dir, "cache")
	if err := os.Mkdir(cache, 0o777); err != nil {
		t.Fatal(err)
	}
	// The dataplane drops DAC override, so permit writes independent of host UID
	// and umask on this disposable bind mount.
	if err := os.Chmod(cache, 0o777); err != nil {
		t.Fatal(err)
	}

	config := fmt.Sprintf("kind: Cluster\napiVersion: kind.x-k8s.io/v1alpha4\nnodes:\n- role: control-plane\n- role: worker\n  extraMounts:\n  - hostPath: %q\n    containerPath: /var/lib/racer\n", cache)

	configPath := filepath.Join(dir, "kind.yaml")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	nodeImage := os.Getenv("RACER_E2E_NODE_IMAGE")
	if nodeImage == "" {
		nodeImage = "kindest/node:v1.33.1"
	}

	c.created = true

	t.Log("creating kind cluster")
	c.run(90*time.Second, nil, "kind", "create", "cluster", "--name", c.name, "--image", nodeImage, "--kubeconfig", c.kubeconfig, "--config", configPath, "--wait", "60s")
	t.Log("loading images")
	c.run(45*time.Second, nil, "kind", "load", "docker-image", "--name", c.name, "racer-controlplane:"+tag, "racer-dataplane:"+tag, "unbounded-operator:"+tag, "racer-fixture:"+tag)
	t.Log("installing operator and Racer")
	c.install()
	c.await("cache activation", func() error {
		var cache racerapi.ClusterCache

		b, err := c.kubectl(nil, "get", "clustercache", "smoke", "-o", "json")
		if err != nil {
			return err
		}

		if err := json.Unmarshal(b, &cache); err != nil {
			return err
		}

		if cache.Status.ObservedGeneration != cache.Generation || !apiMeta.IsStatusConditionTrue(cache.Status.Conditions, "Ready") || cache.Status.Participants.Desired != 1 || cache.Status.Participants.Ready != 1 {
			return fmt.Errorf("cache not active: %+v", cache.Status)
		}

		return nil
	})
	c.apply(&core.Pod{
		TypeMeta: meta.TypeMeta{APIVersion: "v1", Kind: "Pod"}, ObjectMeta: meta.ObjectMeta{Name: "sdk", Namespace: namespace},
		Spec: core.PodSpec{
			NodeName: c.name + "-worker", RestartPolicy: core.RestartPolicyNever,
			SecurityContext: &core.PodSecurityContext{RunAsUser: ptr.To(int64(65532)), RunAsGroup: ptr.To(int64(65532)), RunAsNonRoot: ptr.To(true)},
			Volumes:         []core.Volume{{Name: "sockets", VolumeSource: core.VolumeSource{HostPath: &core.HostPathVolumeSource{Path: racermeta.SocketRoot, Type: ptr.To(core.HostPathDirectory)}}}},
			Containers:      []core.Container{{Name: "sdk", Image: "racer-fixture:" + tag, ImagePullPolicy: core.PullNever, VolumeMounts: []core.VolumeMount{{Name: "sockets", MountPath: racermeta.SocketRoot}}}},
		},
	})
	c.await("SDK cold/warm/range verification", func() error {
		b, err := c.kubectl(nil, "get", "pod", "sdk", "-o", "json")
		if err != nil {
			return err
		}

		var pod core.Pod
		if err := json.Unmarshal(b, &pod); err != nil {
			return err
		}

		if pod.Status.Phase == core.PodFailed {
			t.Fatalf("SDK failed: %s", c.must("logs", "sdk"))
		}

		if pod.Status.Phase != core.PodSucceeded {
			return fmt.Errorf("SDK pod is %s", pod.Status.Phase)
		}

		return nil
	})
	t.Log(string(c.must("logs", "sdk")))
}

type cluster struct {
	t                                *testing.T
	root, dir, name, kubeconfig, tag string
	ctx                              context.Context
	cancel                           context.CancelFunc
	created                          bool
}

func command(parent context.Context, timeout time.Duration, input []byte, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = bytes.NewReader(input)
	cmd.WaitDelay = 2 * time.Second

	b, err := cmd.CombinedOutput()
	if err != nil {
		return b, fmt.Errorf("%s %v: %w\n%s", name, args, err, b)
	}

	return b, nil
}

func (c *cluster) run(timeout time.Duration, input []byte, name string, args ...string) []byte {
	c.t.Helper()

	b, err := command(c.ctx, timeout, input, name, args...)
	if err != nil {
		c.t.Fatal(err)
	}

	return b
}

func (c *cluster) kubectl(input []byte, args ...string) ([]byte, error) {
	return command(c.ctx, 10*time.Second, input, "kubectl", append([]string{"--kubeconfig", c.kubeconfig, "--request-timeout=8s", "-n", namespace}, args...)...)
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

	b, err := json.Marshal(value)
	if err != nil {
		c.t.Fatal(err)
	}

	if _, err := c.kubectl(b, "apply", "-f", "-"); err != nil {
		c.t.Fatal(err)
	}
}

func (c *cluster) await(description string, check func() error) {
	c.t.Helper()
	c.t.Log("waiting for " + description)

	deadline := time.Now().Add(60 * time.Second)

	var err error
	for time.Now().Before(deadline) {
		if err = check(); err == nil {
			return
		}

		if c.ctx.Err() != nil {
			break
		}

		time.Sleep(time.Second)
	}

	c.t.Fatalf("waiting for %s: %v", description, err)
}

func (c *cluster) install() {
	output := filepath.Join(c.dir, "operator")
	if err := render.Render(filepath.Join(c.root, "deploy/unbounded-operator"), output, map[string]string{
		"Namespace": namespace, "OperatorImage": "unbounded-operator:" + c.tag,
		"APIServerEndpoint": "https://kubernetes.default.svc:443", "ReapLegacyResources": "false",
	}); err != nil {
		c.t.Fatal(err)
	}

	for _, name := range []string{"00-namespace", "01-serviceaccount", "02-rbac", "03-configmap", "04-deployment"} {
		path := filepath.Join(output, name+".yaml")
		if name == "04-deployment" {
			b, err := os.ReadFile(path)
			if err != nil {
				c.t.Fatal(err)
			}

			var deployment apps.Deployment
			if err := yaml.Unmarshal(b, &deployment); err != nil {
				c.t.Fatal(err)
			}

			deployment.Spec.Template.Spec.Containers[0].ImagePullPolicy = core.PullNever
			c.apply(&deployment)
		} else {
			c.must("apply", "-f", path)
		}

		if name == "00-namespace" {
			// Park the unconditional net component before the operator starts so
			// kind retains its CNI. Racer uses shipping security and automatic tuning.
			c.apply(&core.ConfigMap{TypeMeta: meta.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, ObjectMeta: meta.ObjectMeta{Name: override.ConfigMapName, Namespace: namespace}, Data: map[string]string{"racer-e2e.yaml": fmt.Sprintf(`apiVersion: %s
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
        replicas: 1
        template:
          spec:
            containers:
              - name: controller
                image: racer-controlplane:%s
                imagePullPolicy: Never
                env:
                  - name: RUST_LOG
                    value: racer_controlplane=debug
  - component: racer-dataplane
    kind: DaemonSet
    patch:
      spec:
        template:
          spec:
            initContainers:
              - name: bootstrap
                image: racer-controlplane:%s
                imagePullPolicy: Never
            containers:
              - name: dataplane
                image: racer-dataplane:%s
                imagePullPolicy: Never
`, override.APIVersion, c.tag, c.tag, c.tag)}})
		}
	}

	c.await("Site CRD", func() error { _, err := c.kubectl(nil, "get", "crd", "sites.unbounded-cloud.io"); return err })
	c.must("label", "node", c.name+"-worker", racermeta.SiteLabelKey+"=smoke")

	disabled := machina.SiteComponentSpec{Enabled: ptr.To(false)}
	c.apply(&machina.Site{
		TypeMeta: meta.TypeMeta{APIVersion: machina.GroupVersion.String(), Kind: "Site"}, ObjectMeta: meta.ObjectMeta{Name: "smoke", Labels: map[string]string{"kubernetes.io/metadata.name": "smoke"}},
		Spec: machina.SiteSpec{
			NodeCidrs: []string{"172.18.0.0/16"}, PodCidrAssignments: []netapi.PodCidrAssignment{{CidrBlocks: []string{"10.244.0.0/16"}}}, ManageCniPlugin: ptr.To(false),
			Components: machina.SiteComponents{
				Machina: &machina.MachinaComponentSpec{SiteComponentSpec: disabled}, Metalman: &machina.MetalmanComponentSpec{SiteComponentSpec: disabled},
				Gantry: &machina.GantryComponentSpec{SiteComponentSpec: disabled}, TokenRefresher: &machina.TokenRefresherComponentSpec{SiteComponentSpec: disabled},
			},
		},
	})
	c.apply(&racerapi.ClusterCache{TypeMeta: meta.TypeMeta{APIVersion: racerapi.GroupVersion.String(), Kind: "ClusterCache"}, ObjectMeta: meta.ObjectMeta{Name: "smoke"}, Spec: racerapi.ClusterCacheSpec{SiteSelector: meta.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "smoke"}}, CacheGeneration: 1, MaxCandidateAttempts: 3}})
}

func (c *cluster) cleanup() {
	c.cancel()

	c.ctx, c.cancel = context.WithTimeout(context.Background(), 20*time.Second)
	defer c.cancel()

	c.t.Log("cleaning up kind cluster")

	if c.created {
		if c.t.Failed() {
			for name, args := range map[string][]string{
				"resources.yaml": {"get", "pods,deployments,daemonsets,clustercaches,sites", "-o", "yaml"},
				"events.txt":     {"get", "events", "--sort-by=.metadata.creationTimestamp"},
				"pods.txt":       {"describe", "pods"},
			} {
				b, _ := c.kubectl(nil, args...)
				_ = os.WriteFile(filepath.Join(c.dir, name), b, 0o600)
			}

			_, _ = command(c.ctx, 10*time.Second, nil, "kind", "export", "logs", filepath.Join(c.dir, "logs"), "--name", c.name)
			c.t.Logf("diagnostics: %s", c.dir)
		}

		c.cancel()

		c.ctx, c.cancel = context.WithTimeout(context.Background(), 25*time.Second)
		defer c.cancel()
		// Kubelet creates root-owned cache files on the host bind mount.
		_, _ = command(c.ctx, 5*time.Second, nil, "docker", "exec", c.name+"-worker", "chmod", "-R", "a+rwX", "/var/lib/racer")
		if _, err := command(c.ctx, 15*time.Second, nil, "kind", "delete", "cluster", "--name", c.name, "--kubeconfig", c.kubeconfig); err != nil {
			c.t.Error(err)
		}
	}

	if !c.t.Failed() {
		if err := os.RemoveAll(c.dir); err != nil {
			c.t.Error(err)
		}
	}
}
