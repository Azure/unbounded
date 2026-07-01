// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"time"

	"github.com/Azure/unbounded/internal/playpen/meta"
	"github.com/Azure/unbounded/internal/playpen/runner"
)

const (
	AnnotationClientWireGuardPublicKey = meta.AnnotationClientWireGuardPublicKey
	AnnotationServerWireGuardPublicKey = meta.AnnotationServerWireGuardPublicKey
	AnnotationRedfishCertPEM           = meta.AnnotationRedfishCertPEM
	AnnotationIdempotencyKeyHash       = meta.AnnotationIdempotencyKeyHash
	AnnotationRequestHash              = meta.AnnotationRequestHash
	AnnotationClaimedAt                = meta.AnnotationClaimedAt

	LabelAllocated    = meta.LabelAllocated
	LabelArchitecture = meta.LabelArchitecture

	ArchitectureAMD64 = meta.ArchitectureAMD64
	ArchitectureARM64 = meta.ArchitectureARM64
)

type Config struct {
	ListenAddr                   string
	Namespace                    string
	ServiceName                  string
	TLSSecretName                string
	RunnerNamespace              string
	RunnerLabelSelector          string
	RunnerImage                  string
	RunnerImagePullPolicy        string
	RunnerServiceAccountName     string
	RunnerAMD64Count             int
	RunnerARM64Count             int
	RunnerWireGuardHostPortStart int32
	RunnerWireGuardHostPortEnd   int32
	RunnerRequireKVM             bool
	RunnerControlPlaneToleration bool
	PlaypenTTL                   time.Duration
	ReconcileInterval            time.Duration
	Runner                       runner.Config
}

func DefaultConfig() Config {
	runnerCfg := runner.DefaultConfig()

	return Config{
		ListenAddr:                   ":8443",
		Namespace:                    "playpen",
		ServiceName:                  "playpen-operator",
		TLSSecretName:                "playpen-operator-tls",
		RunnerNamespace:              "playpen",
		RunnerLabelSelector:          "app.kubernetes.io/name=playpen-runner",
		RunnerImage:                  "ghcr.io/azure/playpen:latest",
		RunnerImagePullPolicy:        "Always",
		RunnerServiceAccountName:     "playpen-runner",
		RunnerAMD64Count:             1,
		RunnerARM64Count:             1,
		RunnerWireGuardHostPortStart: 51820,
		RunnerWireGuardHostPortEnd:   51899,
		RunnerRequireKVM:             true,
		RunnerControlPlaneToleration: false,
		PlaypenTTL:                   time.Hour,
		ReconcileInterval:            30 * time.Second,
		Runner:                       runnerCfg,
	}
}
