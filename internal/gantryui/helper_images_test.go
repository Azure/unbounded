// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantryui

import (
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestGHCRHelpersPreserveACRBaseBoundary(t *testing.T) {
	image := "ghcr.io/vpatelsj/gantry-ui@sha256:" + strings.Repeat("a", 64)
	cfg := testBuildConfig()

	cfg.Image, cfg.ImagePullSecret = image, "ghcr-pull"
	if err := cfg.Validate("example.azurecr.io"); err != nil {
		t.Fatal(err)
	}

	cfg.Base = image
	if err := cfg.Validate("example.azurecr.io"); err == nil {
		t.Fatal("foreign runtime base accepted")
	}

	cfg.Base = testBuildConfig().Base
	for _, invalid := range []string{"ghcr.io/user/image:latest", "https://ghcr.io/user/image", image + "bad"} {
		cfg.Image = invalid
		if err := cfg.Validate("example.azurecr.io"); err == nil {
			t.Fatalf("invalid helper image accepted: %s", invalid)
		}
	}

	cfg.Image = image

	cfg.ImagePullSecret = "Invalid!"
	if err := cfg.Validate("example.azurecr.io"); err == nil {
		t.Fatal("invalid secret accepted")
	}
}

func TestHelperPullSecretReachesEveryJob(t *testing.T) {
	spec := testReclaimSpec()
	spec.Image = "ghcr.io/vpatelsj/gantry-ui@sha256:" + strings.Repeat("b", 64)
	spec.ImagePullSecret = "ghcr-pull"

	cfg := ReclaimConfig{Image: spec.Image, ServiceAccount: spec.ServiceAccount, ImagePullSecret: spec.ImagePullSecret}
	if err := cfg.Validate("example.azurecr.io"); err != nil {
		t.Fatal(err)
	}

	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}

	coordinator, err := reclaimCoordinatorJob(spec)
	if err != nil {
		t.Fatal(err)
	}

	worker := reclaimWorkerJob(&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: reclaimJobName, UID: "owner"}}, spec, corev1.Node{}, nil)
	builderCfg := testBuildConfig()
	builderCfg.Image = spec.Image
	builderCfg.ImagePullSecret = spec.ImagePullSecret

	builder, err := imageBuildJob(BuildSpec{BuildRequest: BuildRequest{Layers: 1, LayerMB: 1}}, *builderCfg)
	if err != nil {
		t.Fatal(err)
	}

	for _, job := range []*batchv1.Job{coordinator, worker, builder} {
		secrets := job.Spec.Template.Spec.ImagePullSecrets
		if len(secrets) != 1 || secrets[0].Name != "ghcr-pull" {
			t.Fatalf("helper missing pull secret: %+v", secrets)
		}
	}
}
