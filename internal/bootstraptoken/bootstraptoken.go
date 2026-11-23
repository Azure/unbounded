// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package bootstraptoken defines the Kubernetes bootstrap tokens Unbounded
// agents join a cluster with: how a token is generated, named and stored, which
// Site it belongs to, and the group it authenticates as. kubectl,
// token-refresher and machine-ops create these tokens, and the agent and
// machina's CSR approver depend on their format, so every component uses this
// package instead of its own copy.
package bootstraptoken

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/Azure/unbounded/api/machina/v1alpha3"
)

const (
	alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

	// DefaultTTL is how long a new token is valid.
	DefaultTTL = 24 * time.Hour

	// Group is the extra group every token adds to the identity it
	// authenticates. machina approves an agent daemon's client certificate
	// request only when the requester is in this group.
	Group = "system:bootstrappers:unbounded-agent-daemons"
)

// ErrNotFound is returned by ForSite when no usable token exists for the Site.
var ErrNotFound = errors.New("bootstrap token not found")

// Token is a bootstrap token and the metadata stored with it.
type Token struct {
	ID        string
	Secret    string
	Labels    map[string]string
	ExpiresAt time.Time
}

// WithLabel sets a label on the token's Secret and returns the token.
func (t *Token) WithLabel(key, value string) *Token {
	if t.Labels == nil {
		t.Labels = make(map[string]string)
	}

	t.Labels[key] = value

	return t
}

// String returns the token with its secret masked.
func (t *Token) String() string {
	return fmt.Sprintf("%s.%s", t.ID, strings.Repeat("x", len(t.Secret)))
}

// New returns a random token that expires DefaultTTL from now.
func New() (*Token, error) {
	// Generate 6-character token ID
	id, err := generateRandomString(6)
	if err != nil {
		return nil, fmt.Errorf("failed to generate bootstrap token ID: %w", err)
	}

	// Generate 16-character token secret
	secret, err := generateRandomString(16)
	if err != nil {
		return nil, fmt.Errorf("failed to generate bootstrap token secret: %w", err)
	}

	return &Token{ID: id, Secret: secret, ExpiresAt: time.Now().UTC().Add(DefaultTTL)}, nil
}

// Apply creates or updates the token's Secret in kube-system with server-side
// apply.
func Apply(ctx context.Context, kubeCli kubernetes.Interface, fieldManager string, token *Token) error {
	ao := metav1.ApplyOptions{
		FieldManager: fieldManager,
	}

	name := SecretName(token.ID)
	s := v1.Secret(name, metav1.NamespaceSystem).
		WithType(corev1.SecretTypeBootstrapToken).
		WithLabels(token.Labels).
		WithData(SecretData(token))

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		_, err := kubeCli.CoreV1().Secrets(metav1.NamespaceSystem).Apply(ctx, s, ao)
		return err
	}); err != nil {
		return fmt.Errorf("applying secret %q: %w", name+"/"+metav1.NamespaceSystem, err)
	}

	return nil
}

// SecretData returns the data of the token's Secret. A zero ExpiresAt expires
// DefaultTTL from now.
func SecretData(token *Token) map[string][]byte {
	expiresAt := token.ExpiresAt
	if expiresAt.IsZero() {
		expiresAt = time.Now().UTC().Add(DefaultTTL)
	}

	return map[string][]byte{
		"auth-extra-groups":              []byte(Group),
		"expiration":                     []byte(expiresAt.UTC().Format(time.RFC3339)),
		"token-id":                       []byte(token.ID),
		"token-secret":                   []byte(token.Secret),
		"usage-bootstrap-authentication": []byte("true"),
		"usage-bootstrap-signing":        []byte("true"),
	}
}

// ForSite returns the newest usable token for siteName. It returns ErrNotFound
// when there is none.
func ForSite(ctx context.Context, kubeCli kubernetes.Interface, siteName string) (*Token, error) {
	l, err := kubeCli.CoreV1().Secrets(metav1.NamespaceSystem).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("unbounded-cloud.io/site=%s", siteName),
	})
	if err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}

	var newest *corev1.Secret

	for i := range l.Items {
		secret := &l.Items[i]

		if !IsValidForSite(secret, siteName, time.Now()) {
			continue
		}

		if newest == nil || secret.CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = secret
		}
	}

	if newest == nil {
		return nil, fmt.Errorf("bootstrap token not found for site %q: %w", siteName, ErrNotFound)
	}

	return &Token{
		ID:        string(newest.Data["token-id"]),
		Secret:    string(newest.Data["token-secret"]),
		Labels:    newest.Labels,
		ExpiresAt: expiration(newest),
	}, nil
}

// IsValidForSite reports whether secret contains a usable, unexpired bootstrap
// token scoped to siteName. A MachineAllocation's token carries the site label
// too, but belongs to that allocation alone and is revoked with it, so a Secret
// with the allocation UID label is never a site token.
func IsValidForSite(secret *corev1.Secret, siteName string, now time.Time) bool {
	if secret == nil || secret.Type != corev1.SecretTypeBootstrapToken {
		return false
	}

	if secret.Labels["unbounded-cloud.io/site"] != siteName {
		return false
	}

	if _, allocation := secret.Labels[v1alpha3.LabelAllocationUID]; allocation {
		return false
	}

	if strings.TrimSpace(string(secret.Data["token-id"])) == "" || strings.TrimSpace(string(secret.Data["token-secret"])) == "" {
		return false
	}

	return !isExpired(secret, now)
}

// SecretName returns the Kubernetes Secret name for a token ID.
func SecretName(tokenID string) string {
	return fmt.Sprintf("bootstrap-token-%s", tokenID)
}

// Expiration returns the expiration encoded in a bootstrap token Secret. A
// missing or invalid expiration returns the zero time.
func Expiration(secret *corev1.Secret) time.Time {
	return expiration(secret)
}

func isExpired(secret *corev1.Secret, now time.Time) bool {
	expiresAt := expiration(secret)

	return !expiresAt.IsZero() && !now.Before(expiresAt)
}

func expiration(secret *corev1.Secret) time.Time {
	raw := strings.TrimSpace(string(secret.Data["expiration"]))
	if raw == "" {
		return time.Time{}
	}

	expiresAt, err := time.Parse(time.RFC3339, raw)
	if err == nil {
		return expiresAt
	}

	// Some bootstrap token producers omit the RFC3339 timezone. Kubernetes
	// control-plane timestamps are UTC, so interpret that legacy form as UTC.
	expiresAt, err = time.ParseInLocation("2006-01-02T15:04:05", raw, time.UTC)
	if err == nil {
		return expiresAt
	}

	return time.Time{}
}

func generateRandomString(length int) (string, error) {
	result := make([]byte, length)
	for i := 0; i < length; i++ {
		num, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}

		result[i] = alphabet[num.Int64()]
	}

	return string(result), nil
}
