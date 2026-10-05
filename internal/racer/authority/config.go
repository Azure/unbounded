// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package authority

import (
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/racer/wire"
)

// Config is copied by New. It contains authority policy, not listener, manager,
// filesystem, network replication, or HTTP admission settings.
type Config struct {
	Cluster                   wire.ClusterID
	Namespace                 string
	DataplaneServiceAccount   string
	ControllerServiceAccount  string
	DaemonSetName             string
	CredentialsSecretName     string
	VersionConfigMapName      string
	InstallationConfigMapName string
	Rotation                  RotationPolicy
	CertificateLifetime       time.Duration
	SnapshotMaxAge            time.Duration
	MaxTokenBytes             int
}

// Dependencies are captured at construction. Reader must bypass informer caches.
// Writer needs only ordinary Kubernetes writes, including TokenReview creation.
type Dependencies struct {
	Reader client.Reader
	Writer client.Writer
	Now    func() time.Time
}

func (c Config) effective() Config {
	if c.CertificateLifetime == 0 {
		c.CertificateLifetime = wire.CertificateLifetime
	}

	if c.SnapshotMaxAge == 0 {
		c.SnapshotMaxAge = 30 * time.Second
	}

	return c
}

func (c Config) Validate() error {
	c = c.effective()
	if !wire.ValidUUID(string(c.Cluster)) || len(validation.IsDNS1123Label(c.Namespace)) != 0 || c.SnapshotMaxAge < time.Second {
		return wire.InvalidRequest
	}

	for _, name := range []string{c.VersionConfigMapName, c.InstallationConfigMapName, c.DaemonSetName, c.CredentialsSecretName, c.DataplaneServiceAccount} {
		if len(validation.IsDNS1123Subdomain(name)) != 0 {
			return fmt.Errorf("resource name: %w", wire.InvalidRequest)
		}
	}

	if c.VersionConfigMapName == c.InstallationConfigMapName {
		return wire.InvalidRequest
	}

	lifetime := c.CertificateLifetime
	if lifetime < 2*time.Minute || lifetime > wire.CertificateLifetime || lifetime%time.Second != 0 {
		return wire.InvalidRequest
	}

	if c.Rotation.PrepareFor <= 0 || c.Rotation.Interval < c.Rotation.PrepareFor || c.Rotation.RetainFor < lifetime || c.Rotation.Interval > 365*24*time.Hour || c.Rotation.RetainFor > 365*24*time.Hour {
		return wire.InvalidRequest
	}

	return nil
}

// Private engines freeze once for whitebox fixtures. New owns their inputs.
type frozenConfig struct {
	once  sync.Once
	value Config
}

func (f *frozenConfig) get(input *Config) Config {
	f.once.Do(func() { f.value = input.effective() })
	return f.value
}
