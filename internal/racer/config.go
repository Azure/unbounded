// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"time"

	"github.com/Azure/unbounded/internal/racer/wire"
)

type Config struct {
	Cluster                 wire.ClusterID
	Namespace               string
	ControlAddress          string
	MetricsAddress          string
	ProbeAddress            string
	ControlURL              string
	TLSCertificateFile      string
	TLSPrivateKeyFile       string
	BootstrapTrustConfigMap string
	DataplaneImage          string
	PeerPort                uint16
	DataplaneServiceAccount string
	DaemonSetName           string
	IssuerSecretName        string
	KeyringSecretName       string
	VersionConfigMapName    string
	Limits                  Limits
	Rotation                RotationPolicy
}

type Limits struct {
	MaxPolls               int
	MaxConcurrentWrites    int
	MaxConcurrentBootstrap int
	HeaderBytes            int
	WriteTimeout           time.Duration
	ShutdownTimeout        time.Duration
}

// LoadConfig deliberately prevents the scaffold executable from starting.
func LoadConfig() (Config, error) { return Config{}, pending("config.load") }

func (Config) Validate() error { return pending("config.validate") }
