// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"net/http"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/racer/wire"
)

type Bootstrap struct {
	Client    client.Client
	APIReader client.Reader
	Config    Config
	Issuer    *Issuer
}

// Authenticate performs TokenReview for racer-control, checks the live bound Pod
// UID and authorized ServiceAccount/workload, and resolves its assigned Node UID.
// Token contents, CSR contents, and requested names are not authority on their own.
func (*Bootstrap) Authenticate(_ context.Context, _ *http.Request) (NodeIdentity, error) {
	return NodeIdentity{}, pending("bootstrap.authenticate")
}

// Enroll validates CSR proof of possession and binds the issued identity to the
// token, not caller-provided SANs. Every issuance uses a token, including renewal.
// Retries correlate by enrollment ID; there is no persistent receipt ledger.
func (*Bootstrap) Enroll(_ context.Context, _ *http.Request, _ wire.BootstrapRequest) (wire.BootstrapResponse, error) {
	return wire.BootstrapResponse{}, pending("bootstrap.enroll")
}
