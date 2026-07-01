// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"time"

	"github.com/Azure/unbounded/internal/playpen/runner"
)

const (
	AnnotationClientWireGuardPublicKey = "playpen.unbounded-cloud.io/client-wireguard-public-key"
	AnnotationIdempotencyKeyHash       = "playpen.unbounded-cloud.io/idempotency-key-hash"
	AnnotationRequestHash              = "playpen.unbounded-cloud.io/request-hash"
	AnnotationClaimedAt                = "playpen.unbounded-cloud.io/claimed-at"

	LabelAllocated = "playpen.unbounded-cloud.io/allocated"
)

type Config struct {
	ListenAddr                       string
	Namespace                        string
	TLSSecretName                    string
	RunnerNamespace                  string
	RunnerLabelSelector              string
	RunnerWireGuardSecretName        string
	RunnerWireGuardPrivateKeyDataKey string
	PlaypenTTL                       time.Duration
	ReconcileInterval                time.Duration
	Runner                           runner.Config
}

func DefaultConfig() Config {
	runnerCfg := runner.DefaultConfig()
	runnerCfg.WireGuard.ClientPublicKeyFile = "/etc/playpen/claim/client-public-key"

	return Config{
		ListenAddr:                       ":8443",
		Namespace:                        "playpen",
		TLSSecretName:                    "playpen-operator-tls",
		RunnerNamespace:                  "playpen",
		RunnerLabelSelector:              "app.kubernetes.io/name=playpen-runner",
		RunnerWireGuardSecretName:        "playpen-runner-wireguard",
		RunnerWireGuardPrivateKeyDataKey: "privatekey",
		PlaypenTTL:                       time.Hour,
		ReconcileInterval:                30 * time.Second,
		Runner:                           runnerCfg,
	}
}
