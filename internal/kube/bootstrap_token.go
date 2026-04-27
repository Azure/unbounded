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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	bootstrapTokenAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
)

// ErrBootstrapTokenNotFound is returned by GetBootstrapToken when no matching
// bootstrap token secret exists in the cluster.
var ErrBootstrapTokenNotFound = errors.New("bootstrap token not found")

type BootstrapToken struct {
	ID     string
	Secret string
	Labels map[string]string
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

	return &BootstrapToken{ID: id, Secret: secret}, nil
}

func ApplyBootstrapToken(ctx context.Context, kubeCli kubernetes.Interface, fieldManager string, token *BootstrapToken) error {
	ao := metav1.ApplyOptions{
		FieldManager: fieldManager,
	}

	s := v1.Secret(bootstrapTokenName(token), metav1.NamespaceSystem).
		WithType(corev1.SecretTypeBootstrapToken).
		WithLabels(token.Labels).
		WithData(map[string][]byte{
			"auth-extra-groups":              []byte("system:bootstrappers:kubeadm:default-node-token"),
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

		if secret.Type != corev1.SecretTypeBootstrapToken {
			continue
		}

		if _, ok := secret.Data["token-id"]; !ok {
			continue
		}

		if _, ok := secret.Data["token-secret"]; !ok {
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
		ID:     string(newest.Data["token-id"]),
		Secret: string(newest.Data["token-secret"]),
		Labels: newest.Labels,
	}, nil
}

func bootstrapTokenName(tok *BootstrapToken) string {
	return fmt.Sprintf("bootstrap-token-%s", tok.ID)
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
