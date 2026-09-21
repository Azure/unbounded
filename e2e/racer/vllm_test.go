//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	racermeta "github.com/Azure/unbounded/internal/racer"
)

// TestVLLMS3 loads real safetensors via vLLM's S3 iterator into CPU parameters.
// Independent clients enter through different nodes; only Racer shares state.
func TestVLLMS3(t *testing.T) {
	root := repository(t)
	im := buildImages(t, root)
	origin := buildVLLMImage(t, root, "origin")
	client := buildVLLMImage(t, root, "client")
	c := newCluster(t, root, im)
	c.deploy()

	revision := c.converge(0, "racer-volume")
	if _, err := command(90*time.Second, nil, "kind", "load", "docker-image", "--name", c.name, origin); err != nil {
		t.Fatal(err)
	}

	c.apply(&core.Pod{
		TypeMeta:   meta.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: meta.ObjectMeta{Name: "s3-origin", Namespace: namespace, Labels: map[string]string{"app": "s3-origin"}},
		Spec: core.PodSpec{NodeName: c.name + "-worker", Containers: []core.Container{{
			Name: "origin", Image: origin, ImagePullPolicy: core.PullNever,
			ReadinessProbe: &core.Probe{ProbeHandler: core.ProbeHandler{HTTPGet: &core.HTTPGetAction{Path: "/healthz", Port: intPort(8080)}}, PeriodSeconds: 1},
		}}},
	})
	c.apply(&core.Service{TypeMeta: meta.TypeMeta{APIVersion: "v1", Kind: "Service"}, ObjectMeta: meta.ObjectMeta{Name: "s3-origin", Namespace: namespace}, Spec: core.ServiceSpec{Selector: map[string]string{"app": "s3-origin"}, Ports: []core.ServicePort{{Port: 8080}}}})
	c.await("S3 origin ready", func() error {
		var p core.Pod
		if err := c.get("pod", "s3-origin", &p); err != nil {
			return err
		}

		if !podReady(p) {
			return fmt.Errorf("S3 origin not Ready")
		}

		return nil
	})
	c.must("annotate", "service/racer-volume", racermeta.OriginServiceAnnotationKey+"=s3-origin", racermeta.OriginPortAnnotationKey+"=8080", "--overwrite")
	c.converge(revision, "racer-volume")

	var svc core.Service
	if err := c.get("service", "racer-volume", &svc); err != nil {
		t.Fatal(err)
	}

	beforePeers := c.peerRequests()
	if hits := c.s3Hits(); len(hits) != 0 {
		t.Fatalf("S3 origin already warm: %+v", hits)
	}

	t.Log("cold vLLM load through the first worker")
	c.loadVLLM(client, "worker", svc.Spec.ClusterIP)
	cold := c.s3Hits()
	methods, sources, ranges := map[string]int{}, map[string]bool{}, map[string]int{}

	for _, hit := range cold {
		if hit.Target != "/models/model.safetensors" {
			t.Fatalf("unexpected S3 object read: %+v", hit)
		}

		methods[hit.Method]++

		sources[hit.Source] = true
		if hit.Method == "GET" {
			ranges[hit.Range]++
		}
	}
	// The ~4 MiB checkpoint spans multiple cache pages. vLLM's header and
	// tensor range reads should share one metadata lookup and one owner.
	if len(cold) != 3 || methods["HEAD"] != 1 || methods["GET"] != 2 || len(sources) != 1 ||
		ranges["bytes=0-4194303"] != 1 || ranges["bytes=4194304-4198567"] != 1 {
		t.Fatalf("cold S3 reads: %+v", cold)
	}

	t.Logf("cold S3 reads: %+v", cold)
	t.Log("warm vLLM load through the other worker, with a fresh client")
	c.loadVLLM(client, "worker2", svc.Spec.ClusterIP)

	if warm := c.s3Hits(); !reflect.DeepEqual(cold, warm) {
		t.Fatalf("warm load reached S3: cold=%+v warm=%+v", cold, warm)
	}

	if c.peerRequests() <= beforePeers {
		t.Fatal("vLLM loads did not exercise peer HTTP forwarding")
	}
}

func buildVLLMImage(t *testing.T, root, role string) string {
	t.Helper()

	image := fmt.Sprintf("racer-vllm-%s:e2e-%d", role, time.Now().UnixNano())
	t.Logf("building %s", image)

	if _, err := command(15*time.Minute, nil, "docker", "build", "-t", image, "-f", filepath.Join(root, "images", "racer-vllm-"+role, "Containerfile"), root); err != nil {
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

type s3Hit struct {
	Method, Target, Source, Range string
}

func (c *cluster) s3Hits() []s3Hit {
	c.t.Helper()

	r := c.fetch("probe-a", "GET", "http://s3-origin:8080/hits", "")
	if r.Status != 200 {
		c.t.Fatalf("S3 origin ledger: %d %s", r.Status, r.Body)
	}

	var hits []s3Hit
	decode(c.t, r.Body, &hits)

	return hits
}

func (c *cluster) loadVLLM(image, worker, serviceIP string) {
	c.t.Helper()
	name := c.name + "-vllm-" + worker
	// Share the node's network namespace to exercise the Local Service on
	// that node without importing the large CPU vLLM image into every node.
	// Fresh containers have no shared Hugging Face or local weight cache.
	defer func() {
		_, _ = command(15*time.Second, nil, "docker", "rm", "-f", name)
	}()

	out, err := command(2*time.Minute, nil, "docker", "run", "--rm", "--name", name,
		"--network", "container:"+c.name+"-"+worker,
		"-e", "AWS_ENDPOINT_URL=http://"+serviceIP,
		"-e", "AWS_ACCESS_KEY_ID=e2e", "-e", "AWS_SECRET_ACCESS_KEY=e2e",
		"-e", "AWS_DEFAULT_REGION=us-east-1", "-e", "AWS_EC2_METADATA_DISABLED=true",
		"-e", "RUNAI_STREAMER_S3_USE_VIRTUAL_ADDRESSING=0",
		"-e", "RUNAI_STREAMER_LOG_TO_STDERR=1",
		"-e", "RUNAI_STREAMER_CONCURRENCY=2", "-e", "RUNAI_STREAMER_MEMORY_LIMIT=16777216",
		"-e", "OMP_NUM_THREADS=1", image)
	if writeErr := os.WriteFile(filepath.Join(c.dir, "vllm-"+worker+".log"), out, 0o600); writeErr != nil {
		c.t.Error(writeErr)
	}

	if err != nil {
		c.t.Fatal(err)
	}

	c.t.Logf("vLLM on %s: %s", worker, out)
}
