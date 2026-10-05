// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import "github.com/Azure/unbounded/internal/racer/authority"

func (c Config) authorityConfig() authority.Config {
	return authority.Config{
		Cluster: c.Cluster, Namespace: c.Namespace,
		DataplaneServiceAccount: c.DataplaneServiceAccount, ControllerServiceAccount: c.ControllerServiceAccount,
		DaemonSetName: c.DaemonSetName, CredentialsSecretName: c.CredentialsSecretName,
		VersionConfigMapName: c.VersionConfigMapName, InstallationConfigMapName: c.InstallationConfigMapName,
		Rotation: c.Rotation, CertificateLifetime: c.CertificateLifetime, SnapshotMaxAge: c.SnapshotMaxAge,
		MaxTokenBytes: c.Limits.HeaderBytes,
	}
}

type (
	NodeIdentity      = authority.NodeIdentity
	PublicationHandle = authority.PublicationHandle
)

func (s *Server) servingAuthority() *authority.Authority { return s.authority }
