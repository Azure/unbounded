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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ErrBootstrapTokenNotFound is returned by GetBootstrapToken when no matching
// bootstrap token secret exists in the cluster.
var ErrBootstrapTokenNotFound = errors.New("bootstrap token not found")

const (
	alphanumeric = "abcdefghijklmnopqrstuvwxyz0123456789"
)

type BootstrapToken struct {
	ID     string
	Secret string
}

func (t BootstrapToken) String() string {
	return fmt.Sprintf("%s.%s", t.ID, t.Secret)
}

func GetBootstrapToken(ctx context.Context, kubeCli kubernetes.Interface) (*BootstrapToken, error) {
	listOptions := metav1.ListOptions{
		LabelSelector: "unbounded-cloud.io/default-bootstrap-token=true",
	}

	secrets, err := kubeCli.CoreV1().Secrets(metav1.NamespaceSystem).List(ctx, listOptions)
	if err != nil {
		return nil, fmt.Errorf("failed to list secrets: %w", err)
	}

	for _, secret := range secrets.Items {
		if secret.Type == "bootstrap.kubernetes.io/token" {
			if !strings.HasPrefix(secret.Name, "bootstrap-token-") {
				continue
			}

			tokenID := strings.TrimPrefix(secret.Name, "bootstrap-token-")

			// Extract token secret from secret data
			tokenSecretBytes, ok := secret.Data["token-secret"]
			if !ok {
				return nil, fmt.Errorf("bootstrap token secret missing token-secret field: %s", secret.Name)
			}

			return &BootstrapToken{
				ID:     tokenID,
				Secret: string(tokenSecretBytes),
			}, nil
		}
	}

	return nil, ErrBootstrapTokenNotFound
}

func GenerateBootstrapIDAndToken() (string, string, error) {
	// Generate 6-character token ID
	tokenID, err := generateRandomString(6)
	if err != nil {
		return "", "", fmt.Errorf("failed to generate token ID: %w", err)
	}

	// Generate 16-character token secret
	tokenSecret, err := generateRandomString(16)
	if err != nil {
		return "", "", fmt.Errorf("failed to generate token secret: %w", err)
	}

	return tokenID, tokenSecret, nil
}

func BootstrapTokenManifest(id, token string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: bootstrap-token-%[1]s
  namespace: kube-system
  labels:
    unbounded-cloud.io/default-bootstrap-token: "true"
type: bootstrap.kubernetes.io/token
stringData:
  token-id: "%[1]s"
  token-secret: "%[2]s"
  usage-bootstrap-authentication: "true"
  usage-bootstrap-signing: "true"
  auth-extra-groups: "system:bootstrappers:kubeadm:default-node-token"`, id, token)
}

func generateRandomString(length int) (string, error) {
	result := make([]byte, length)
	for i := 0; i < length; i++ {
		num, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphanumeric))))
		if err != nil {
			return "", err
		}

		result[i] = alphanumeric[num.Int64()]
	}

	return string(result), nil
}
