// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
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
func (b *Bootstrap) Authenticate(ctx context.Context, r *http.Request) (NodeIdentity, error) {
	if err := ctx.Err(); err != nil {
		return NodeIdentity{}, err
	}

	if b.Client == nil || b.APIReader == nil {
		return NodeIdentity{}, wire.Unavailable
	}

	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return NodeIdentity{}, wire.Unauthenticated
	}

	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.ContainsAny(token, " \t\r\n,") || len(token) > b.Config.Limits.HeaderBytes {
		return NodeIdentity{}, wire.Unauthenticated
	}

	review := &authv1.TokenReview{Spec: authv1.TokenReviewSpec{Token: token, Audiences: []string{wire.TokenAudience}}}
	if err := b.Client.Create(ctx, review); err != nil {
		return NodeIdentity{}, wire.Unavailable
	}

	status := review.Status
	if !status.Authenticated || status.Error != "" || !slices.Contains(status.Audiences, wire.TokenAudience) {
		return NodeIdentity{}, wire.Unauthenticated
	}

	if status.User.Username != "system:serviceaccount:"+b.Config.Namespace+":"+b.Config.DataplaneServiceAccount {
		return NodeIdentity{}, wire.Forbidden
	}
	// TokenReview authenticates the token. Its JWT expiration is used only to
	// shorten authorization, never to establish identity or extend validity.
	expires, err := tokenExpiration(token)
	if err != nil {
		return NodeIdentity{}, err
	}

	podName, podUID := singleExtra(status.User, "pod-name"), singleExtra(status.User, "pod-uid")
	if podName == "" || podUID == "" || status.User.UID == "" {
		return NodeIdentity{}, wire.Unauthenticated
	}

	var pod corev1.Pod
	if err := b.APIReader.Get(ctx, client.ObjectKey{Namespace: b.Config.Namespace, Name: podName}, &pod); err != nil {
		return NodeIdentity{}, authorizationError(err)
	}

	if string(pod.UID) != podUID {
		return NodeIdentity{}, wire.Forbidden
	}

	if err := authorizePod(ctx, b.APIReader, b.Config, &pod, status.User.UID); err != nil {
		return NodeIdentity{}, err
	}

	var node corev1.Node
	if err := b.APIReader.Get(ctx, client.ObjectKey{Name: pod.Spec.NodeName}, &node); err != nil {
		return NodeIdentity{}, authorizationError(err)
	}

	if !authorizedNode(&node) {
		return NodeIdentity{}, wire.Forbidden
	}
	// Newer API servers return node binding extras. When present they must
	// agree, but older servers' Pod-bound TokenReviews need not include them.
	for key, want := range map[string]string{"node-name": node.Name, "node-uid": string(node.UID)} {
		if _, present := status.User.Extra["authentication.kubernetes.io/"+key]; present && singleExtra(status.User, key) != want {
			return NodeIdentity{}, wire.Forbidden
		}
	}

	if err := ctx.Err(); err != nil {
		return NodeIdentity{}, err
	}

	if !time.Now().Before(expires) {
		return NodeIdentity{}, wire.Unauthenticated
	}

	return NodeIdentity{cluster: b.Config.Cluster, node: wire.NodeID(node.UID), expires: expires}, nil
}

func singleExtra(user authv1.UserInfo, key string) string {
	values := user.Extra["authentication.kubernetes.io/"+key]
	if len(values) != 1 {
		return ""
	}

	return values[0]
}

func tokenExpiration(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, wire.Unauthenticated
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, wire.Unauthenticated
	}

	var claims struct {
		Expiration int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Expiration <= 0 {
		return time.Time{}, wire.Unauthenticated
	}

	expires := time.Unix(claims.Expiration, 0)
	if !time.Now().Before(expires) {
		return time.Time{}, wire.Unauthenticated
	}

	return expires, nil
}

// Enroll validates CSR proof of possession and binds the issued identity to the
// token, not caller-provided SANs. Every issuance uses a token, including renewal.
// Retries correlate by enrollment ID; there is no persistent receipt ledger.
func (b *Bootstrap) Enroll(ctx context.Context, r *http.Request, request wire.BootstrapRequest) (wire.BootstrapResponse, error) {
	identity, err := b.Authenticate(ctx, r)
	if err != nil {
		return wire.BootstrapResponse{}, err
	}

	if b.Issuer == nil {
		return wire.BootstrapResponse{}, wire.Unavailable
	}

	ctx, cancel := context.WithDeadline(ctx, identity.expires)
	defer cancel()

	return b.Issuer.Issue(ctx, identity, request)
}
