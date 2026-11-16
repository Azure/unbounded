// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

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

func TestGetBootstrapTokenForSiteIgnoresInvalidExpiration(t *testing.T) {
	ctx := context.Background()

	for name, mutate := range map[string]func(*corev1.Secret){
		"missing": func(secret *corev1.Secret) {
			delete(secret.Data, "expiration")
		},
		"unparsable": func(secret *corev1.Secret) {
			secret.Data["expiration"] = []byte("not-a-time")
		},
	} {
		t.Run(name, func(t *testing.T) {
			invalid := bootstrapSecret("bad123", "badsecret0000000", "site-a", time.Now().Add(time.Hour))
			mutate(invalid)

			_, err := GetBootstrapTokenForSite(ctx, fake.NewClientset(invalid), "site-a")
			require.ErrorIs(t, err, ErrBootstrapTokenNotFound)

			valid := bootstrapSecret("ok1234", "oksecret00000000", "site-a", time.Now().Add(time.Hour))
			token, err := GetBootstrapTokenForSite(ctx, fake.NewClientset(invalid, valid), "site-a")
			require.NoError(t, err)
			require.Equal(t, "ok1234", token.ID)
		})
	}
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
	allocation := valid.DeepCopy()
	allocation.Labels["unbounded-cloud.io/allocation-uid"] = "alloc-uid"
	allocationEmptyLabel := valid.DeepCopy()
	allocationEmptyLabel.Labels["unbounded-cloud.io/allocation-uid"] = ""
	missingExpiration := valid.DeepCopy()
	delete(missingExpiration.Data, "expiration")

	unparsableExpiration := valid.DeepCopy()
	unparsableExpiration.Data["expiration"] = []byte("not-a-time")

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
		"nil":                          {site: "site-a"},
		"allocation token":             {secret: allocation, site: "site-a"},
		"allocation label empty value": {secret: allocationEmptyLabel, site: "site-a"},
		"missing expiration":           {secret: missingExpiration, site: "site-a"},
		"unparsable expiration":        {secret: unparsableExpiration, site: "site-a"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tt.want, ValidBootstrapTokenSecretForSite(tt.secret, tt.site, now))
		})
	}
}

func TestGetBootstrapTokenForSiteIgnoresAllocationTokens(t *testing.T) {
	ctx := context.Background()
	allocation := bootstrapSecret("alloc1", "allocsecret00000", "site-a", time.Now().Add(2*time.Hour))
	allocation.Labels["unbounded-cloud.io/allocation-uid"] = "alloc-uid"
	client := fake.NewClientset(
		bootstrapSecret("site12", "sitesecret000000", "site-a", time.Now().Add(time.Hour)),
		allocation,
	)

	token, err := GetBootstrapTokenForSite(ctx, client, "site-a")
	require.NoError(t, err)
	require.Equal(t, "site12", token.ID)

	onlyAllocation := fake.NewClientset(allocation.DeepCopy())
	_, err = GetBootstrapTokenForSite(ctx, onlyAllocation, "site-a")
	require.ErrorIs(t, err, ErrBootstrapTokenNotFound)
}

func TestBootstrapTokenSecretData(t *testing.T) {
	expiresAt := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)

	data := BootstrapTokenSecretData(&BootstrapToken{ID: "abc123", Secret: "0123456789abcdef", ExpiresAt: expiresAt})
	require.Equal(t, map[string][]byte{
		"auth-extra-groups":              []byte("system:bootstrappers:unbounded-agent-daemons"),
		"expiration":                     []byte("2026-04-28T12:00:00Z"),
		"token-id":                       []byte("abc123"),
		"token-secret":                   []byte("0123456789abcdef"),
		"usage-bootstrap-authentication": []byte("true"),
		"usage-bootstrap-signing":        []byte("true"),
	}, data)
}

func TestBootstrapTokenExpirationAcceptsTimezoneLessUTC(t *testing.T) {
	secret := bootstrapSecret("abc123", "0123456789abcdef", "site-a", time.Now().Add(time.Hour))
	secret.Data["expiration"] = []byte("2026-08-25T15:48:24")

	require.Equal(t, time.Date(2026, 8, 25, 15, 48, 24, 0, time.UTC), bootstrapTokenExpiration(secret))
}

func TestBootstrapTokenWithInvalidExpirationIsExpired(t *testing.T) {
	for name, mutate := range map[string]func(*corev1.Secret){
		"missing": func(secret *corev1.Secret) {
			delete(secret.Data, "expiration")
		},
		"unparsable": func(secret *corev1.Secret) {
			secret.Data["expiration"] = []byte("not-a-time")
		},
	} {
		t.Run(name, func(t *testing.T) {
			secret := bootstrapSecret("abc123", "0123456789abcdef", "site-a", time.Now().Add(time.Hour))
			mutate(secret)

			require.True(t, bootstrapTokenExpiration(secret).IsZero())
			require.True(t, isExpiredBootstrapToken(secret, time.Now()))
		})
	}
}

func TestBootstrapTokenExpirationFutureAndPast(t *testing.T) {
	now := time.Now()
	future := bootstrapSecret("fut123", "future0000000000", "site-a", now.Add(time.Hour))
	past := bootstrapSecret("pas123", "past000000000000", "site-a", now.Add(-time.Hour))

	require.False(t, isExpiredBootstrapToken(future, now))
	require.False(t, isExpiredBootstrapToken(future, now.Add(30*time.Minute)))
	require.True(t, isExpiredBootstrapToken(future, now.Add(time.Hour)))
	require.True(t, isExpiredBootstrapToken(past, now))
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
