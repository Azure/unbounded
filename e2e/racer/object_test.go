//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// TestRacerObjectVLLM exercises both production object modes and the real vLLM
// plugin across an operator-managed, two-worker Racer cache on a private cluster.
func TestRacerObjectVLLM(t *testing.T) {
	root := repository(t)
	im := buildImages(t, root)
	object := buildTestImage(t, root, "racer-object")
	azure := buildTestImage(t, root, "racer-object-azure")
	client := buildTestImage(t, root, "racer-object-client")
	c := newCluster(t, root, im)
	c.deploy()
	c.converge(0, "racer-volume")

	// The large client image is needed only on workers, never the control plane.
	for _, worker := range []string{"worker", "worker2"} {
		if _, err := command(10*time.Minute, nil, "kind", "load", "docker-image", "--name", c.name,
			"--nodes", c.name+"-"+worker, object, azure, client); err != nil {
			t.Fatal(err)
		}
	}

	c.apply(&core.Pod{
		TypeMeta:   meta.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: meta.ObjectMeta{Name: "fake-azure", Namespace: namespace},
		Spec: core.PodSpec{NodeName: c.name + "-worker", Containers: []core.Container{{
			Name: "azure", Image: azure, ImagePullPolicy: core.PullNever,
			ReadinessProbe: &core.Probe{ProbeHandler: core.ProbeHandler{HTTPGet: &core.HTTPGetAction{Path: "/healthz", Port: intPort(8080)}}, PeriodSeconds: 1},
		}}},
	})
	c.must("wait", "--for=condition=Ready", "pod/fake-azure", "--timeout=90s")

	var pod core.Pod
	if err := c.get("pod", "fake-azure", &pod); err != nil {
		t.Fatal(err)
	}

	endpoint := "http://" + pod.Status.PodIP + ":8080"
	c.apply(&core.ConfigMap{
		TypeMeta:   meta.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: meta.ObjectMeta{Name: "object-config", Namespace: namespace},
		Data:       map[string]string{"objects.json": fmt.Sprintf(`{"azure_endpoint":%q,"objects":[{"bucket":"models","key":"model.safetensors","container":"weights","blob":"immutable/model.safetensors"}]}`, endpoint)},
	})
	c.apply(cacheResource("object-cache", primarySite))

	for _, worker := range []string{"worker", "worker2"} {
		p := c.objectPod("object-backend-"+worker, worker)
		p.Spec.Containers = []core.Container{objectContainer(object, "backend")}
		p.Spec.Containers[0].Args = append(p.Spec.Containers[0].Args, "--azure-auth=anonymous")
		c.apply(p)
	}

	c.awaitFor(2*time.Minute, "object cache activation", func() error {
		_, err := c.kubectl(nil, "wait", "--for=condition=Ready", "p2pcache/object-cache", "--timeout=1s")
		return err
	})

	beforePeers := c.peerRequests()
	if hits := c.azureHits(endpoint); len(hits) != 0 {
		t.Fatalf("Azure already warm: %+v", hits)
	}

	c.loadObjectVLLM(client, object, "worker")
	cold := c.azureHits(endpoint)
	methods, ranges := map[string]int{}, map[string]int{}

	var etag string

	for _, hit := range cold {
		if hit.Target != "/weights/immutable/model.safetensors" {
			t.Fatalf("unexpected Azure request: %+v", hit)
		}

		methods[hit.Method]++
		if hit.Method == "GET" {
			ranges[hit.Range]++
			if hit.IfMatch == "" || etag != "" && hit.IfMatch != etag {
				t.Fatalf("Azure range is not pinned to one ETag: %+v", cold)
			}

			etag = hit.IfMatch
		}
	}

	if len(cold) != 3 || methods["HEAD"] != 1 || methods["GET"] != 2 ||
		ranges["bytes=0-4194303"] != 1 || ranges["bytes=4194304-4198567"] != 1 {
		t.Fatalf("cold Azure ledger: %+v", cold)
	}

	t.Logf("cold Azure ledger: %+v", cold)

	if r := c.fetch("probe-a", "POST", endpoint+"/offline", ""); r.Status != 200 {
		t.Fatalf("disable Azure: %d %s", r.Status, r.Body)
	}

	// A fresh process on the other worker must succeed with blob reads disabled.
	c.loadObjectVLLM(client, object, "worker2")

	if warm := c.azureHits(endpoint); !reflect.DeepEqual(cold, warm) {
		t.Fatalf("warm load reached disabled Azure: cold=%+v warm=%+v", cold, warm)
	}

	if c.peerRequests() <= beforePeers {
		t.Fatal("object loads did not exercise peer HTTP forwarding")
	}

	t.Log("PASS: exact vLLM tensors and inference on both workers; warm load with Azure disabled; peer forwarding observed")
}

func buildTestImage(t *testing.T, root, component string) string {
	t.Helper()

	if os.Getenv("RACER_E2E_IMAGE_TAG") != "" {
		return prebuiltImage(t, component)
	}

	image := fmt.Sprintf("%s:e2e-%d", component, time.Now().UnixNano())
	t.Logf("building %s", image)

	containerfile := filepath.Join(root, "images", component, "Containerfile")
	if component == "racer-object-client" {
		containerfile = filepath.Join(root, "e2e", "racer", "fixtures", "object-client", "Containerfile")
	}

	if _, err := command(15*time.Minute, nil, "docker", "build", "-t", image, "-f", containerfile, root); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if os.Getenv("RACER_E2E_KEEP") != "1" {
			if _, err := command(30*time.Second, nil, "docker", "image", "rm", image); err != nil {
				t.Error(err)
			}
		}
	})

	return image
}

type azureHit struct {
	Method, Target, Range string
	IfMatch               string `json:"if_match"`
}

func (c *cluster) azureHits(endpoint string) []azureHit {
	c.t.Helper()

	r := c.fetch("probe-a", "GET", endpoint+"/hits", "")
	if r.Status != 200 {
		c.t.Fatalf("Azure ledger: %d %s", r.Status, r.Body)
	}

	var hits []azureHit
	decode(c.t, r.Body, &hits)

	return hits
}

func (c *cluster) objectPod(name, worker string) *core.Pod {
	return &core.Pod{
		TypeMeta:   meta.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: meta.ObjectMeta{Name: name, Namespace: namespace},
		Spec: core.PodSpec{
			NodeName: c.name + "-" + worker, RestartPolicy: core.RestartPolicyNever,
			SecurityContext: &core.PodSecurityContext{RunAsUser: ptr.To(int64(65532)), RunAsGroup: ptr.To(int64(65532))},
			Volumes: []core.Volume{
				{Name: "sockets", VolumeSource: core.VolumeSource{HostPath: &core.HostPathVolumeSource{Path: "/dev/racer/object-cache", Type: ptr.To(core.HostPathDirectory)}}},
				{Name: "config", VolumeSource: core.VolumeSource{ConfigMap: &core.ConfigMapVolumeSource{LocalObjectReference: core.LocalObjectReference{Name: "object-config"}}}},
			},
		},
	}
}

func objectContainer(image, mode string) core.Container {
	socket := "cache"
	if mode == "backend" {
		socket = "origin"
	}

	return core.Container{
		Name: mode, Image: image, ImagePullPolicy: core.PullNever,
		Args:         []string{mode, "--config=/config/objects.json", "--socket=/dev/racer/object-cache/" + socket, "--concurrency=4", "--timeout=30s"},
		VolumeMounts: []core.VolumeMount{{Name: "sockets", MountPath: "/dev/racer/object-cache"}, {Name: "config", MountPath: "/config", ReadOnly: true}},
	}
}

func (c *cluster) loadObjectVLLM(client, object, worker string) {
	c.t.Helper()
	p := c.objectPod("object-vllm-"+worker, worker)
	frontend := objectContainer(object, "frontend")
	frontend.RestartPolicy = ptr.To(core.ContainerRestartPolicyAlways)
	p.Spec.InitContainers = []core.Container{frontend}
	p.Spec.Containers = []core.Container{{
		Name: "client", Image: client, ImagePullPolicy: core.PullNever,
		Env: []core.EnvVar{
			{Name: "AWS_ENDPOINT_URL", Value: "http://127.0.0.1:8000"},
			{Name: "AWS_ACCESS_KEY_ID", Value: "local"},
			{Name: "AWS_SECRET_ACCESS_KEY", Value: "local"},
			{Name: "AWS_DEFAULT_REGION", Value: "us-east-1"},
			{Name: "AWS_EC2_METADATA_DISABLED", Value: "true"},
			{Name: "RUNAI_STREAMER_S3_USE_VIRTUAL_ADDRESSING", Value: "0"},
			{Name: "RUNAI_STREAMER_MEMORY_LIMIT", Value: "16777216"},
			{Name: "OMP_NUM_THREADS", Value: "1"},
			{Name: "VLLM_PLUGINS", Value: "racer_object"},
			{Name: "HOME", Value: "/tmp"},
		},
	}}
	c.apply(p)
	c.awaitFor(3*time.Minute, "racer-object vLLM client termination", func() error {
		var current core.Pod
		if err := c.get("pod", p.Name, &current); err != nil {
			return err
		}

		for _, status := range current.Status.ContainerStatuses {
			if status.Name == "client" && status.State.Terminated != nil {
				return nil
			}
		}

		return fmt.Errorf("vLLM phase %s", current.Status.Phase)
	})

	for _, container := range []string{"client", "frontend"} {
		out := c.must("logs", p.Name, "-c", container)
		if err := os.WriteFile(filepath.Join(c.dir, p.Name+"-"+container+".log"), out, 0o600); err != nil {
			c.t.Fatal(err)
		}

		c.t.Logf("%s/%s: %s", p.Name, container, out)

		if container == "client" && !strings.Contains(string(out), `"loaded": ["bias", "weight"]`) {
			c.t.Fatal("vLLM did not report verified weights")
		}
	}

	var current core.Pod
	if err := c.get("pod", p.Name, &current); err != nil {
		c.t.Fatal(err)
	}

	for _, status := range current.Status.ContainerStatuses {
		if status.Name == "client" && status.State.Terminated.ExitCode != 0 {
			c.t.Fatalf("vLLM failed: %+v", status.State.Terminated)
		}
	}
}
