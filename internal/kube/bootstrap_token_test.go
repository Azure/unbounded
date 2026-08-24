// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestApplyBootstrapTokenAddsExpiration(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset()
	token := &BootstrapToken{ID: "abc123", Secret: "0123456789abcdef"}

	err := ApplyBootstrapToken(ctx, client, "test", token)
	require.NoError(t, err)

	secret, err := client.CoreV1().Secrets(metav1.NamespaceSystem).Get(ctx, "bootstrap-token-abc123", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, "system:bootstrappers:unbounded-agent-daemons", string(secret.Data["auth-extra-groups"]))
	require.NotEmpty(t, secret.Data["expiration"])
	expiresAt, err := time.Parse(time.RFC3339, string(secret.Data["expiration"]))
	require.NoError(t, err)
	require.True(t, expiresAt.After(time.Now()))
}

func TestGetBootstrapTokenForSiteIgnoresExpiredTokens(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset(
		bootstrapSecret("old123", "oldsecret0000000", "site-a", time.Now().Add(-time.Hour)),
		bootstrapSecret("new123", "newsecret0000000", "site-a", time.Now().Add(time.Hour)),
	)

	token, err := GetBootstrapTokenForSite(ctx, client, "site-a")
	require.NoError(t, err)
	require.Equal(t, "new123", token.ID)
}

func TestValidBootstrapTokenSecretForSite(t *testing.T) {
	now := time.Now()
	valid := bootstrapSecret("abc123", "0123456789abcdef", "site-a", now.Add(time.Hour))
	wrongType := valid.DeepCopy()
	wrongType.Type = corev1.SecretTypeOpaque
	emptyID := valid.DeepCopy()
	emptyID.Data["token-id"] = nil
	emptySecret := valid.DeepCopy()
	emptySecret.Data["token-secret"] = []byte(" ")

	tests := map[string]struct {
		secret *corev1.Secret
		site   string
		want   bool
	}{
		"valid":      {secret: valid.DeepCopy(), site: "site-a", want: true},
		"wrong site": {secret: valid.DeepCopy(), site: "site-b"},
		"expired":    {secret: bootstrapSecret("abc123", "0123456789abcdef", "site-a", now.Add(-time.Hour)), site: "site-a"},
		"wrong type": {secret: wrongType, site: "site-a"},
		"empty id":   {secret: emptyID, site: "site-a"},
		"empty secret": {
			secret: emptySecret,
			site:   "site-a",
		},
		"nil": {site: "site-a"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tt.want, ValidBootstrapTokenSecretForSite(tt.secret, tt.site, now))
		})
	}
}

func bootstrapSecret(id, secret, site string, expiresAt time.Time) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "bootstrap-token-" + id,
			Namespace:         metav1.NamespaceSystem,
			CreationTimestamp: metav1.NewTime(expiresAt),
			Labels: map[string]string{
				"unbounded-cloud.io/site": site,
			},
		},
		Type: corev1.SecretTypeBootstrapToken,
		Data: map[string][]byte{
			"token-id":     []byte(id),
			"token-secret": []byte(secret),
			"expiration":   []byte(expiresAt.UTC().Format(time.RFC3339)),
		},
	}
}
