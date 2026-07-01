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
	AnnotationIdempotencyKeyHash       = meta.AnnotationIdempotencyKeyHash
	AnnotationRequestHash              = meta.AnnotationRequestHash
	AnnotationClaimedAt                = meta.AnnotationClaimedAt

	LabelAllocated = meta.LabelAllocated
)

type Config struct {
	ListenAddr          string
	Namespace           string
	TLSSecretName       string
	RunnerNamespace     string
	RunnerLabelSelector string
	PlaypenTTL          time.Duration
	ReconcileInterval   time.Duration
	Runner              runner.Config
}

func DefaultConfig() Config {
	runnerCfg := runner.DefaultConfig()

	return Config{
		ListenAddr:          ":8443",
		Namespace:           "playpen",
		TLSSecretName:       "playpen-operator-tls",
		RunnerNamespace:     "playpen",
		RunnerLabelSelector: "app.kubernetes.io/name=playpen-runner",
		PlaypenTTL:          time.Hour,
		ReconcileInterval:   30 * time.Second,
		Runner:              runnerCfg,
	}
}
