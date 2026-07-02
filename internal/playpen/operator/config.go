// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"time"

	"github.com/Azure/unbounded/internal/playpen/meta"
	"github.com/Azure/unbounded/internal/playpen/runner"
)

const (
	AnnotationRedfishCertPEM           = meta.AnnotationRedfishCertPEM
	AnnotationIdempotencyKeyHash       = meta.AnnotationIdempotencyKeyHash
	AnnotationRequestHash              = meta.AnnotationRequestHash
	AnnotationClaimedAt                = meta.AnnotationClaimedAt
	AnnotationExternalClientInternalIP = meta.AnnotationExternalClientInternalIP
	AnnotationVXLANRemoteAddress       = meta.AnnotationVXLANRemoteAddress
	AnnotationRunnerNetworkReady       = meta.AnnotationRunnerNetworkReady
	AnnotationControlPlaneKubeconfig   = meta.AnnotationControlPlaneKubeconfig
	AnnotationControlPlaneGuestServer  = meta.AnnotationControlPlaneGuestServer

	LabelAllocated         = meta.LabelAllocated
	LabelArchitecture      = meta.LabelArchitecture
	LabelResourceType      = meta.LabelResourceType
	LabelKubernetesVersion = meta.LabelKubernetesVersion

	ArchitectureAMD64 = meta.ArchitectureAMD64
	ArchitectureARM64 = meta.ArchitectureARM64

	ResourceTypeRunner       = meta.ResourceTypeRunner
	ResourceTypeControlPlane = meta.ResourceTypeControlPlane
)

type Config struct {
	ListenAddr                         string
	Namespace                          string
	ServiceName                        string
	TLSSecretName                      string
	RunnerNamespace                    string
	RunnerLabelSelector                string
	RunnerImage                        string
	RunnerImagePullPolicy              string
	RunnerServiceAccountName           string
	RunnerAMD64Count                   int
	RunnerARM64Count                   int
	RunnerRequireKVM                   bool
	RunnerControlPlaneToleration       bool
	ExternalClientSite                 string
	ControlPlaneCount                  int
	ControlPlaneVersions               []string
	ControlPlaneImage                  string
	ControlPlaneServiceAccountName     string
	ControlPlaneAPIServerHostPortStart int32
	ControlPlaneAPIServerHostPortEnd   int32
	PlaypenTTL                         time.Duration
	ReconcileInterval                  time.Duration
	Runner                             runner.Config
}

func DefaultConfig() Config {
	runnerCfg := runner.DefaultConfig()

	return Config{
		ListenAddr:                         ":8443",
		Namespace:                          "playpen",
		ServiceName:                        "playpen-operator",
		TLSSecretName:                      "playpen-operator-tls",
		RunnerNamespace:                    "playpen",
		RunnerLabelSelector:                "app.kubernetes.io/name=playpen-runner",
		RunnerImage:                        "ghcr.io/azure/playpen:latest",
		RunnerImagePullPolicy:              "Always",
		RunnerServiceAccountName:           "playpen-runner",
		RunnerAMD64Count:                   1,
		RunnerARM64Count:                   1,
		RunnerRequireKVM:                   true,
		RunnerControlPlaneToleration:       false,
		ExternalClientSite:                 "playpen-external",
		ControlPlaneCount:                  0,
		ControlPlaneVersions:               []string{"v1.33.0"},
		ControlPlaneImage:                  "rancher/k3s:{version}-k3s1",
		ControlPlaneServiceAccountName:     "playpen-control-plane",
		ControlPlaneAPIServerHostPortStart: 16443,
		ControlPlaneAPIServerHostPortEnd:   16499,
		PlaypenTTL:                         time.Hour,
		ReconcileInterval:                  30 * time.Second,
		Runner:                             runnerCfg,
	}
}
