// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"crypto/tls"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/racer/wire"
)

// NodeIdentity is verified output, never populated from an untrusted request.
type NodeIdentity struct {
	cluster wire.ClusterID
	node    wire.NodeID
	expires time.Time
}

func (i NodeIdentity) Node() wire.NodeID       { return i.node }
func (i NodeIdentity) Cluster() wire.ClusterID { return i.cluster }
func (i NodeIdentity) Expires() time.Time      { return i.expires }

// Issuer accesses a controller-only Secret. Its private key is never projected
// into dataplane Pods or included in a response or diagnostic.
type Issuer struct {
	APIReader client.Reader
	Config    Config
}

func (*Issuer) Issue(_ context.Context, _ NodeIdentity, _ wire.BootstrapRequest) (wire.BootstrapResponse, error) {
	return wire.BootstrapResponse{}, pending("issuer.issue")
}

// AuthenticateCertificate requires a verified chain, the client-auth usage,
// cluster-scoped Node URI SAN, current validity, and authorization. Recheck on
// every poll: an existing TLS connection must not bypass certificate expiry.
func AuthenticateCertificate(_ context.Context, _ client.Reader, _ Config, _ *tls.ConnectionState) (NodeIdentity, error) {
	return NodeIdentity{}, pending("certificates.authenticate")
}
