// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

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
)

const (
	bootstrapTokenAlphabet   = "abcdefghijklmnopqrstuvwxyz0123456789"
	DefaultBootstrapTokenTTL = 24 * time.Hour
)

// ErrBootstrapTokenNotFound is returned by GetBootstrapToken when no matching
// bootstrap token secret exists in the cluster.
var ErrBootstrapTokenNotFound = errors.New("bootstrap token not found")

type BootstrapToken struct {
	ID        string
	Secret    string
	Labels    map[string]string
	ExpiresAt time.Time
}

func (t *BootstrapToken) WithLabel(key, value string) *BootstrapToken {
	if t.Labels == nil {
		t.Labels = make(map[string]string)
	}

	t.Labels[key] = value

	return t
}

func (t *BootstrapToken) String() string {
	return fmt.Sprintf("%s.%s", t.ID, strings.Repeat("x", len(t.Secret)))
}

func NewBootstrapToken() (*BootstrapToken, error) {
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

	return &BootstrapToken{ID: id, Secret: secret, ExpiresAt: time.Now().UTC().Add(DefaultBootstrapTokenTTL)}, nil
}

func ApplyBootstrapToken(ctx context.Context, kubeCli kubernetes.Interface, fieldManager string, token *BootstrapToken) error {
	ao := metav1.ApplyOptions{
		FieldManager: fieldManager,
	}

	expiresAt := token.ExpiresAt
	if expiresAt.IsZero() {
		expiresAt = time.Now().UTC().Add(DefaultBootstrapTokenTTL)
	}

	s := v1.Secret(bootstrapTokenName(token), metav1.NamespaceSystem).
		WithType(corev1.SecretTypeBootstrapToken).
		WithLabels(token.Labels).
		WithData(map[string][]byte{
			"auth-extra-groups":              []byte("system:bootstrappers:unbounded-agent-daemons"),
			"expiration":                     []byte(expiresAt.UTC().Format(time.RFC3339)),
			"token-id":                       []byte(token.ID),
			"token-secret":                   []byte(token.Secret),
			"usage-bootstrap-authentication": []byte("true"),
			"usage-bootstrap-signing":        []byte("true"),
		})

	return ApplySecret(ctx, kubeCli, s, ao)
}

func GetBootstrapTokenForSite(ctx context.Context, kubeCli kubernetes.Interface, siteName string) (*BootstrapToken, error) {
	l, err := kubeCli.CoreV1().Secrets(metav1.NamespaceSystem).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("unbounded-cloud.io/site=%s", siteName),
	})
	if err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}

	var newest *corev1.Secret

	for i := range l.Items {
		secret := &l.Items[i]

		if !ValidBootstrapTokenSecretForSite(secret, siteName, time.Now()) {
			continue
		}

		if newest == nil || secret.CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = secret
		}
	}

	if newest == nil {
		return nil, fmt.Errorf("bootstrap token not found for site %q: %w", siteName, ErrBootstrapTokenNotFound)
	}

	return &BootstrapToken{
		ID:        string(newest.Data["token-id"]),
		Secret:    string(newest.Data["token-secret"]),
		Labels:    newest.Labels,
		ExpiresAt: bootstrapTokenExpiration(newest),
	}, nil
}

// ValidBootstrapTokenSecretForSite reports whether secret contains a usable,
// unexpired bootstrap token scoped to siteName.
func ValidBootstrapTokenSecretForSite(secret *corev1.Secret, siteName string, now time.Time) bool {
	if secret == nil || secret.Type != corev1.SecretTypeBootstrapToken {
		return false
	}

	if secret.Labels["unbounded-cloud.io/site"] != siteName {
		return false
	}

	if strings.TrimSpace(string(secret.Data["token-id"])) == "" || strings.TrimSpace(string(secret.Data["token-secret"])) == "" {
		return false
	}

	return !isExpiredBootstrapToken(secret, now)
}

// BootstrapTokenSecretName returns the Kubernetes Secret name for a token ID.
func BootstrapTokenSecretName(tokenID string) string {
	return fmt.Sprintf("bootstrap-token-%s", tokenID)
}

// BootstrapTokenExpiration returns the expiration encoded in a bootstrap token
// Secret. A missing or invalid expiration returns the zero time.
func BootstrapTokenExpiration(secret *corev1.Secret) time.Time {
	return bootstrapTokenExpiration(secret)
}

func isExpiredBootstrapToken(secret *corev1.Secret, now time.Time) bool {
	expiresAt := bootstrapTokenExpiration(secret)

	return !expiresAt.IsZero() && !now.Before(expiresAt)
}

func bootstrapTokenExpiration(secret *corev1.Secret) time.Time {
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

func bootstrapTokenName(tok *BootstrapToken) string {
	return BootstrapTokenSecretName(tok.ID)
}

func generateRandomString(length int) (string, error) {
	result := make([]byte, length)
	for i := 0; i < length; i++ {
		num, err := rand.Int(rand.Reader, big.NewInt(int64(len(bootstrapTokenAlphabet))))
		if err != nil {
			return "", err
		}

		result[i] = bootstrapTokenAlphabet[num.Int64()]
	}

	return string(result), nil
}
