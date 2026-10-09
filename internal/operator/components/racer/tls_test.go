// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/operator/component"
)

func verifyServing(t *testing.T, secret *corev1.Secret, trust []byte, now time.Time) error {
	t.Helper()

	pair, err := tls.X509KeyPair(secret.Data[corev1.TLSCertKey], secret.Data[corev1.TLSPrivateKeyKey])
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	require.NoError(t, err)

	roots, intermediates := x509.NewCertPool(), x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(trust))

	for _, der := range pair.Certificate[1:] {
		cert, err := x509.ParseCertificate(der)
		require.NoError(t, err)
		intermediates.AddCert(cert)
	}

	_, err = leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, CurrentTime: now, DNSName: controllerName + ".custom-system.svc"})

	return err
}

func stateOf(t *testing.T, secret *corev1.Secret) tlsState {
	t.Helper()

	var state tlsState
	require.NoError(t, json.Unmarshal(secret.Data[tlsStateKey], &state))

	return state
}

func TestTLSProjectionOrdersAndBoundedRotation(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	secret, err := newTLS("custom-system", now)
	require.NoError(t, err)
	unrelated, err := newTLS("custom-system", now)
	require.NoError(t, err)

	roots := [][]byte{secret.Data["ca.crt"]}

	for generation := 1; generation <= 8; generation++ {
		at := now.Add(time.Duration(generation) * caRotationInterval)
		unchanged, err := renewTLS(secret, "custom-system", at.Add(-time.Second))
		require.NoError(t, err)
		require.Nil(t, unchanged)

		next, err := renewTLS(secret, "custom-system", at)
		require.NoError(t, err)
		require.NotNil(t, next)
		require.NotEqual(t, secret.Data["ca.key"], next.Data["ca.key"])
		require.Len(t, stateOf(t, next).Previous, min(generation, maxPreviousCAs))

		for _, leader := range []*corev1.Secret{secret, next} {
			for _, follower := range []*corev1.Secret{secret, next} {
				require.NoError(t, verifyServing(t, leader, follower.Data[caBundleKey], at))
				require.Error(t, verifyServing(t, unrelated, follower.Data[caBundleKey], at))
			}
		}

		for i, root := range roots {
			err := verifyServing(t, next, root, at)
			if i >= generation-maxPreviousCAs {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		}

		roots = append(roots, next.Data["ca.crt"])
		secret = next
	}
}

func TestTLSClockSkew(t *testing.T) {
	createdAt := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	original, err := newTLS("custom-system", createdAt)
	require.NoError(t, err)
	rotated, err := renewTLS(original, "custom-system", createdAt.Add(caRotationInterval))
	require.NoError(t, err)
	require.NotNil(t, rotated)

	for _, generation := range []struct {
		name   string
		secret *corev1.Secret
	}{
		{name: "initial", secret: original},
		{name: "rotated", secret: rotated},
	} {
		t.Run(generation.name, func(t *testing.T) {
			for _, tc := range []struct {
				name    string
				skew    time.Duration
				wantErr bool
			}{
				{name: "same-clock"},
				{name: "one-second-behind", skew: time.Second},
				{name: "inside-backdating-window", skew: time.Hour - time.Second},
				{name: "at-not-before", skew: time.Hour},
				{name: "before-not-before", skew: time.Hour + time.Second, wantErr: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					secret := generation.secret.DeepCopy()
					bindRuntime(secret, "test-installation")
					secret.UID = "test-tls"
					env := testEnv(t, secret)
					plan := component.NewPlan()
					now := stateOf(t, secret).CreatedAt.Add(-tc.skew)
					stored, err := planTLSAt(t.Context(), env, plan, "test-installation", false, now)
					require.Zero(t, plan.Len())

					if tc.wantErr {
						require.ErrorContains(t, err, "certificates are not yet valid")
						require.Nil(t, stored)

						return
					}

					require.NoError(t, err)
					require.NotNil(t, stored)
					require.Equal(t, secret.Data, stored.Data)
					require.NoError(t, verifyServing(t, stored, original.Data["ca.crt"], now))
				})
			}
		})
	}
}

func TestTLSCorruptionAndExpiredCAFailsClosed(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	original, err := newTLS("custom-system", now)
	require.NoError(t, err)
	rotated, err := renewTLS(original, "custom-system", now.Add(caRotationInterval))
	require.NoError(t, err)

	for _, key := range []string{tlsStateKey, previousCAKey, "ca.key", "ca.crt", corev1.TLSCertKey, corev1.TLSPrivateKeyKey} {
		t.Run(key, func(t *testing.T) {
			secret := rotated.DeepCopy()
			bindRuntime(secret, "test-installation")
			secret.UID = "test-tls"
			delete(secret.Data, key)
			env := testEnv(t, secret)

			plan := component.NewPlan()
			for _, at := range []time.Time{now.Add(caRotationInterval), now.Add(caRotationInterval + caLifetime)} {
				value, err := planTLSAt(t.Context(), env, plan, "test-installation", false, at)
				require.Error(t, err)
				require.Nil(t, value)
				require.Zero(t, plan.Len())
			}
		})
	}

	for _, corruption := range []string{"overlap", "version", "chain", "marker", "trailing", "created-before-ca", "created-after-ca"} {
		t.Run(corruption, func(t *testing.T) {
			secret := rotated.DeepCopy()
			state := stateOf(t, secret)

			switch corruption {
			case "overlap":
				state.Previous[0].RetireAt = state.Previous[0].RetireAt.Add(day)
				require.NoError(t, writeTLSState(secret, state))
			case "version":
				state.Version = 2
				require.NoError(t, writeTLSState(secret, state))
			case "chain":
				secret.Data[corev1.TLSCertKey] = original.Data[corev1.TLSCertKey]
			case "marker":
				delete(secret.Annotations, tlsStateAnnotation)
			case "trailing":
				secret.Data[tlsStateKey] = append(secret.Data[tlsStateKey], []byte(" {}")...)
			case "created-before-ca":
				state.CreatedAt = state.CreatedAt.Add(-time.Second)
				require.NoError(t, writeTLSState(secret, state))
			case "created-after-ca":
				state.CreatedAt = state.CreatedAt.Add(time.Second)
				require.NoError(t, writeTLSState(secret, state))
			}

			for _, at := range []time.Time{now.Add(caRotationInterval), now.Add(caRotationInterval + caLifetime)} {
				_, err := renewTLS(secret, "custom-system", at)
				require.Error(t, err)
			}
		})
	}

	_, err = renewTLS(original, "other-system", now.Add(caLifetime))
	require.Error(t, err)
	_, err = renewTLS(original, "other-system", now)
	require.Error(t, err)
}

func TestTLSPersistBeforeTrustAndCAS(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	env := testEnv(t)

	first, second := component.NewPlan(), component.NewPlan()
	for _, plan := range []*component.Plan{first, second} {
		secret, err := planTLSAt(t.Context(), env, plan, "test-installation", true, now)
		require.NoError(t, err)
		require.Nil(t, secret)
		require.Len(t, plan.Operations, 1)
	}

	persist(t, env, first)
	persist(t, env, second)
	winner, err := planTLSAt(t.Context(), env, component.NewPlan(), "test-installation", false, now)
	require.NoError(t, err)
	require.NotNil(t, winner)

	first, second = component.NewPlan(), component.NewPlan()

	at := now.Add(caRotationInterval)
	for _, plan := range []*component.Plan{first, second} {
		secret, err := planTLSAt(t.Context(), env, plan, "test-installation", false, at)
		require.NoError(t, err)
		require.Nil(t, secret)
	}

	persist(t, env, first)
	result, err := env.Execute(t.Context(), second)
	require.NoError(t, err)
	require.Len(t, result.Deferred, 1)

	restart := component.NewPlan()
	stored, err := planTLSAt(t.Context(), env, restart, "test-installation", false, at)
	require.NoError(t, err)
	require.Zero(t, restart.Len())
	require.NoError(t, verifyServing(t, stored, winner.Data["ca.crt"], at))
}

func TestTLSReadFailuresAndMissingState(t *testing.T) {
	for _, scenario := range []string{"read", "trust-read", "retained-trust", "established-missing"} {
		t.Run(scenario, func(t *testing.T) {
			env := testEnv(t)
			if scenario == "retained-trust" {
				require.NoError(t, env.Client.Create(t.Context(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: trustName, Namespace: env.Namespace}}))
			}

			if scenario == "read" || scenario == "trust-read" {
				env.APIReader = interceptor.NewClient(env.Client.(client.WithWatch), interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if scenario == "read" || key.Name == trustName {
							return errors.New("unavailable")
						}

						return c.Get(ctx, key, obj, opts...)
					},
				})
			}

			plan := component.NewPlan()
			_, err := planTLSAt(t.Context(), env, plan, "test-installation", scenario != "established-missing", time.Now())
			require.Error(t, err)
			require.Zero(t, plan.Len())
		})
	}
}

func TestRetainedTLSMaintenanceDoesNotRepairController(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)
	persist(t, env, planPass(t, env))
	require.NoError(t, env.Client.Delete(t.Context(), cacheObject("cache")))

	secret := &corev1.Secret{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, tlsName), secret))
	now := stateOf(t, secret).CreatedAt
	deployment := &appsv1.Deployment{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, controllerName), deployment))
	require.NoError(t, env.Client.Delete(t.Context(), deployment))

	for week := 1; week <= 8; week++ {
		at := now.Add(time.Duration(week) * caRotationInterval)
		old := secret.DeepCopy()

		for _, kind := range []string{"Secret", "ConfigMap"} {
			plan, result, err := planAt(t.Context(), env, at)
			require.NoError(t, err)

			if kind == "Secret" {
				require.Equal(t, "TLSMaintenance", result.Reason)
			} else {
				require.Equal(t, component.ReasonDisabled, result.Reason)
			}

			require.Len(t, plan.Operations, 1)
			require.Equal(t, kind, plan.Operations[0].Object.GetKind())
			persist(t, env, plan)
		}

		plan, result, err := planAt(t.Context(), env, at)
		require.NoError(t, err)
		require.Zero(t, plan.Len())
		require.Equal(t, time.Hour, result.RequeueAfter)
		require.NoError(t, env.Client.Get(t.Context(), objectKey(env, tlsName), secret))
		require.NoError(t, verifyServing(t, secret, old.Data["ca.crt"], at))
		require.NoError(t, verifyServing(t, old, secret.Data[caBundleKey], at))
		require.True(t, apierrors.IsNotFound(env.Client.Get(t.Context(), objectKey(env, controllerName), deployment)))
	}

	require.NoError(t, env.Client.Delete(t.Context(), secret))
	plan, _, err := planAt(t.Context(), env, now.Add(9*caRotationInterval))
	require.EqualError(t, err, "established Racer serving Secret missing; restore it")
	require.Nil(t, plan)
	require.True(t, apierrors.IsNotFound(env.Client.Get(t.Context(), objectKey(env, tlsName), &corev1.Secret{})))
}

func TestRetainedTLSMissingSecretBeforeEstablishment(t *testing.T) {
	for _, tc := range []struct {
		name   string
		phases int
	}{
		{name: "unestablished", phases: 0},
		{name: "reserved-claim", phases: 1},
		{name: "staged-marker", phases: 2},
		{name: "consumed-claim-pending-marker", phases: 3},
		{name: "fresh-marker", phases: 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := testEnv(t)
			for range tc.phases {
				plan, _ := identityPlan(t, env)
				persist(t, env, plan)
			}

			plan, result, err := planAt(t.Context(), env, time.Now())
			require.NoError(t, err)
			require.Zero(t, plan.Len())
			require.Equal(t, component.ReasonDisabled, result.Reason)
			require.Zero(t, result.RequeueAfter)
			require.True(t, apierrors.IsNotFound(env.Client.Get(t.Context(), objectKey(env, tlsName), &corev1.Secret{})))
		})
	}
}

func TestTLSUncertainWritesAndForbiddenRotation(t *testing.T) {
	for _, scenario := range []string{"create-response-lost", "patch-response-lost", "patch-forbidden", "expired-patch-response-lost", "expired-patch-forbidden"} {
		t.Run(scenario, func(t *testing.T) {
			now := time.Now().UTC().Truncate(time.Second)
			env := testEnv(t)

			fresh := scenario == "create-response-lost"
			if !fresh {
				age := caRotationInterval
				if scenario == "expired-patch-response-lost" || scenario == "expired-patch-forbidden" {
					age = caLifetime
				}

				secret, err := newTLS(env.Namespace, now.Add(-age))
				require.NoError(t, err)
				bindRuntime(secret, "test-installation")
				require.NoError(t, env.Client.Create(t.Context(), secret))
			}

			plan := component.NewPlan()
			value, err := planTLSAt(t.Context(), env, plan, "test-installation", fresh, now)
			require.NoError(t, err)
			require.Nil(t, value)

			executor := *env
			executor.Client = interceptor.NewClient(env.Client.(client.WithWatch), interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					require.NoError(t, c.Create(ctx, obj, opts...))
					return errors.New("lost create response")
				},
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if scenario == "patch-forbidden" || scenario == "expired-patch-forbidden" {
						return apierrors.NewForbidden(corev1.Resource("secrets"), tlsName, errors.New("denied"))
					}

					require.NoError(t, c.Patch(ctx, obj, patch, opts...))

					return errors.New("lost patch response")
				},
			})
			result, err := executor.Execute(t.Context(), plan)
			require.NoError(t, err)
			require.Error(t, result.Err())

			restart := component.NewPlan()
			value, err = planTLSAt(t.Context(), env, restart, "test-installation", false, now)
			require.NoError(t, err)

			if scenario == "patch-forbidden" || scenario == "expired-patch-forbidden" {
				require.Nil(t, value)
				require.Len(t, restart.Operations, 1)
			} else {
				require.NotNil(t, value)
				require.Zero(t, restart.Len())
			}
		})
	}
}

func TestDerivedBundleAndRetainedTrustRepair(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)
	require.NoError(t, env.Client.Delete(t.Context(), cacheObject("cache")))

	secret := &corev1.Secret{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, tlsName), secret))
	now := stateOf(t, secret).CreatedAt
	original := secret.DeepCopy()
	delete(secret.Data, caBundleKey)
	require.NoError(t, env.Client.Update(t.Context(), secret))
	plan, _, err := planAt(t.Context(), env, now)
	require.NoError(t, err)
	require.Len(t, plan.Operations, 1)
	require.Equal(t, tlsName, plan.Operations[0].Object.GetName())
	persist(t, env, plan)
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, tlsName), secret))
	require.Equal(t, original.Data, secret.Data)

	trust := &corev1.ConfigMap{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, trustName), trust))
	trust.Data["ca.crt"] = "stale"
	trust.Data["admin"] = "preserve"
	require.NoError(t, env.Client.Update(t.Context(), trust))
	persist(t, env, planPass(t, env))
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, trustName), trust))
	require.Equal(t, "preserve", trust.Data["admin"])
	require.Equal(t, string(servingRoots(secret)), trust.Data["ca.crt"])
	require.NoError(t, env.Client.Delete(t.Context(), trust))
	plan = planPass(t, env)
	require.Len(t, plan.Operations, 1)
	require.Equal(t, component.OpCreateIfAbsent, plan.Operations[0].Kind)
	persist(t, env, plan)
	require.Zero(t, planPass(t, env).Len())
}

func legacyTLS(t *testing.T, now time.Time) *corev1.Secret {
	t.Helper()

	secret, err := newTLS("custom-system", now)
	require.NoError(t, err)
	caPair, err := tls.X509KeyPair(secret.Data["ca.crt"], secret.Data["ca.key"])
	require.NoError(t, err)
	ca, err := singleCertificate(secret.Data["ca.crt"])
	require.NoError(t, err)

	ca.NotAfter = now.Add(legacyCALifetime)
	key := caPair.PrivateKey.(*ecdsa.PrivateKey)
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, ca.PublicKey, key)
	require.NoError(t, err)

	secret.Data["ca.crt"] = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	leaf, err := singleCertificate(secret.Data[corev1.TLSCertKey])
	require.NoError(t, err)

	leaf.NotAfter = now.Add(14 * day)
	der, err = x509.CreateCertificate(rand.Reader, leaf, ca, leaf.PublicKey, key)
	require.NoError(t, err)

	secret.Data[corev1.TLSCertKey] = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	require.NoError(t, writeTLSState(secret, tlsState{Version: 1, CreatedAt: now}))

	return secret
}

func TestTLSExpiredCAReissue(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, legacy := range []bool{false, true} {
		original, err := newTLS("custom-system", now)
		require.NoError(t, err)

		if legacy {
			original = legacyTLS(t, now)
		}

		ca, err := singleCertificate(original.Data["ca.crt"])
		require.NoError(t, err)

		for _, at := range []time.Time{ca.NotAfter.Add(-time.Second), ca.NotAfter, ca.NotAfter.Add(365 * day)} {
			next, err := renewTLS(original, "custom-system", at)
			require.NoError(t, err)
			require.NotNil(t, next)
			require.NotEqual(t, original.Data["ca.key"], next.Data["ca.key"])
			require.NoError(t, verifyServing(t, next, next.Data[caBundleKey], at))

			if at.Before(ca.NotAfter) {
				require.NoError(t, verifyServing(t, next, original.Data["ca.crt"], at))
			} else {
				require.Empty(t, stateOf(t, next).Previous)
				require.Error(t, verifyServing(t, next, original.Data["ca.crt"], at))
			}

			unchanged, err := renewTLS(next, "custom-system", at)
			require.NoError(t, err)
			require.Nil(t, unchanged)
		}
	}
}

func TestTLSLegacyRotationMigration(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	previous := legacyTLS(t, now)
	current := legacyTLS(t, now.Add(7*day))
	parent, err := singleCertificate(previous.Data["ca.crt"])
	require.NoError(t, err)
	child, err := singleCertificate(current.Data["ca.crt"])
	require.NoError(t, err)
	pair, err := tls.X509KeyPair(previous.Data["ca.crt"], previous.Data["ca.key"])
	require.NoError(t, err)
	cross, err := crossSign(child, parent, pair.PrivateKey.(*ecdsa.PrivateKey))
	require.NoError(t, err)
	state := stateOf(t, current)
	state.Previous = []previousCA{{Root: previous.Data["ca.crt"], Cross: cross, RetireAt: now.Add(21 * day)}}
	require.NoError(t, writeTLSState(current, state))

	for _, at := range []time.Time{now.Add(14 * day), now.Add(60 * day)} {
		next, err := renewTLS(current, "custom-system", at)
		require.NoError(t, err)
		require.NotNil(t, next)
		require.NoError(t, verifyServing(t, next, next.Data[caBundleKey], at))
		ca, err := singleCertificate(next.Data["ca.crt"])
		require.NoError(t, err)
		require.Equal(t, at.Add(caLifetime), ca.NotAfter)
		unchanged, err := renewTLS(next, "custom-system", at)
		require.NoError(t, err)
		require.Nil(t, unchanged)
	}
}

func TestTLSExpiredCARejectsInvalidUsageAndOwnership(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)

	for _, failure := range []string{"leaf-usage", "ca-key", "leaf-key", "owner", "future"} {
		t.Run(failure, func(t *testing.T) {
			secret, err := newTLS("custom-system", now)
			require.NoError(t, err)
			bindRuntime(secret, "test-installation")
			secret.UID = "test-tls"
			at := now.Add(caLifetime)

			switch failure {
			case "leaf-usage":
				leaf, err := singleCertificate(secret.Data[corev1.TLSCertKey])
				require.NoError(t, err)

				leaf.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
				ca, err := singleCertificate(secret.Data["ca.crt"])
				require.NoError(t, err)
				pair, err := tls.X509KeyPair(secret.Data["ca.crt"], secret.Data["ca.key"])
				require.NoError(t, err)
				der, err := x509.CreateCertificate(rand.Reader, leaf, ca, leaf.PublicKey, pair.PrivateKey)
				require.NoError(t, err)

				secret.Data[corev1.TLSCertKey] = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
			case "ca-key", "leaf-key":
				other, err := newTLS("custom-system", now)
				require.NoError(t, err)

				key := "ca.key"
				if failure == "leaf-key" {
					key = corev1.TLSPrivateKeyKey
				}

				secret.Data[key] = other.Data[key]
			case "owner":
				bindRuntime(secret, "other-installation")
			case "future":
				at = now.Add(-2 * time.Hour)
			}

			env := testEnv(t, secret)
			plan := component.NewPlan()
			value, err := planTLSAt(t.Context(), env, plan, "test-installation", false, at)
			require.Error(t, err)
			require.Nil(t, value)
			require.Zero(t, plan.Len())
		})
	}
}

func TestTLSExpiredCAConcurrentReissue(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)

	old := &corev1.Secret{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, tlsName), old))
	now := stateOf(t, old).CreatedAt.Add(caLifetime)
	first, _, err := planAt(t.Context(), env, now)
	require.NoError(t, err)
	second, _, err := planAt(t.Context(), env, now)
	require.NoError(t, err)

	for _, plan := range []*component.Plan{first, second} {
		require.Len(t, plan.Operations, 1)
		require.Equal(t, tlsName, plan.Operations[0].Object.GetName())
	}

	persist(t, env, first)
	result, err := env.Execute(t.Context(), second)
	require.NoError(t, err)
	require.Len(t, result.Deferred, 1)

	trust := &corev1.ConfigMap{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, trustName), trust))
	require.Equal(t, string(servingRoots(old)), trust.Data["ca.crt"])
	plan, _, err := planAt(t.Context(), env, now)
	require.NoError(t, err)
	persist(t, env, plan)

	stored := &corev1.Secret{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, tlsName), stored))
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, trustName), trust))
	require.Equal(t, string(servingRoots(stored)), trust.Data["ca.crt"])
	require.NoError(t, verifyServing(t, stored, []byte(trust.Data["ca.crt"]), now))
}

func TestTLSEstablishedMaintenanceBeforeUnrelatedFailures(t *testing.T) {
	for _, failure := range []string{"guards", "cache-list", "durable-state"} {
		t.Run(failure, func(t *testing.T) {
			env := testEnv(t, cacheObject("cache"))
			initialize(t, env)

			secret := &corev1.Secret{}
			require.NoError(t, env.Client.Get(t.Context(), objectKey(env, tlsName), secret))
			at := stateOf(t, secret).CreatedAt.Add(caLifetime)
			old := secret.DeepCopy()

			env.APIReader = interceptor.NewClient(env.Client.(client.WithWatch), interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					_, guard := obj.(*admissionv1.ValidatingAdmissionPolicy)
					if (failure == "guards" && guard) || (failure == "durable-state" && key.Name == versionName) {
						return errors.New("unrelated failure")
					}

					return c.Get(ctx, key, obj, opts...)
				},
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, caches := list.(*racerv1.ClusterCacheList); failure == "cache-list" && caches {
						return errors.New("unrelated failure")
					}

					return c.List(ctx, list, opts...)
				},
			})
			for _, name := range []string{tlsName, trustName} {
				plan, _, err := planAt(t.Context(), env, at)
				require.NoError(t, err)
				require.Len(t, plan.Operations, 1)
				require.Equal(t, name, plan.Operations[0].Object.GetName())
				persist(t, env, plan)
			}

			require.NoError(t, env.Client.Get(t.Context(), objectKey(env, tlsName), secret))

			trust := &corev1.ConfigMap{}
			require.NoError(t, env.Client.Get(t.Context(), objectKey(env, trustName), trust))
			require.NoError(t, verifyServing(t, secret, []byte(trust.Data["ca.crt"]), at))
			require.Error(t, verifyServing(t, secret, old.Data["ca.crt"], at))
			plan, _, err := planAt(t.Context(), env, at)
			require.Error(t, err)
			require.Nil(t, plan)
		})
	}
}

func TestTLSWriteFailureDoesNotDelayGuardContainment(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)

	secret := &corev1.Secret{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, tlsName), secret))
	at := stateOf(t, secret).CreatedAt.Add(caLifetime)
	guard := &admissionv1.ValidatingAdmissionPolicy{}
	require.NoError(t, env.Client.Get(t.Context(), client.ObjectKey{Name: guardNames[0]}, guard))
	guard.Spec.Validations = nil
	require.NoError(t, env.Client.Update(t.Context(), guard))
	plan, _, err := planAt(t.Context(), env, at)
	require.NoError(t, err)
	require.Len(t, plan.Operations, 3)

	env.Client = interceptor.NewClient(env.Client.(client.WithWatch), interceptor.Funcs{
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return errors.New("TLS write denied")
		},
	})
	result, err := env.Execute(t.Context(), plan)
	require.NoError(t, err)
	require.ErrorContains(t, result.Err(), "TLS write denied")
	require.True(t, apierrors.IsNotFound(env.Client.Get(t.Context(), objectKey(env, controllerName), &rbacv1.RoleBinding{})))
	require.True(t, apierrors.IsNotFound(env.Client.Get(t.Context(), client.ObjectKey{Name: controllerName}, &rbacv1.ClusterRoleBinding{})))

	stored := &corev1.Secret{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, tlsName), stored))
	require.Equal(t, secret.Data, stored.Data)
}

func TestGuardContainmentFailureDoesNotDelayTLSMaintenance(t *testing.T) {
	for _, deniedKind := range []string{"RoleBinding", "ClusterRoleBinding"} {
		for _, missingTrust := range []bool{false, true} {
			t.Run(deniedKind+map[bool]string{false: "/existing-trust", true: "/missing-trust"}[missingTrust], func(t *testing.T) {
				env := testEnv(t, cacheObject("cache"))
				initialize(t, env)

				old := &corev1.Secret{}
				require.NoError(t, env.Client.Get(t.Context(), objectKey(env, tlsName), old))
				at := stateOf(t, old).CreatedAt.Add(caLifetime)
				guard := &admissionv1.ValidatingAdmissionPolicy{}
				require.NoError(t, env.Client.Get(t.Context(), client.ObjectKey{Name: guardNames[0]}, guard))
				guard.Spec.Validations = nil
				require.NoError(t, env.Client.Update(t.Context(), guard))

				if missingTrust {
					require.NoError(t, env.Client.Delete(t.Context(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: trustName, Namespace: env.Namespace}}))
				}

				env.Client = interceptor.NewClient(env.Client.(client.WithWatch), interceptor.Funcs{
					Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
						if obj.GetObjectKind().GroupVersionKind().Kind == deniedKind {
							return apierrors.NewForbidden(schema.GroupResource{Group: rbacv1.GroupName, Resource: deniedKind}, obj.GetName(), errors.New("containment denied"))
						}

						return c.Delete(ctx, obj, opts...)
					},
				})
				for pass, target := range []string{tlsName, trustName} {
					plan, planned, err := planAt(t.Context(), env, at)
					require.NoError(t, err)
					require.Len(t, plan.Operations, 3-pass)

					for _, op := range plan.Operations {
						require.True(t, op.Kind == component.OpDelete || op.Object.GetName() == target)
					}

					result, err := env.Execute(t.Context(), plan)
					require.NoError(t, err)
					require.ErrorContains(t, result.Err(), "containment denied")
					require.Len(t, result.Failed(), 1)
					require.True(t, apierrors.IsForbidden(result.Failed()[0].Err))
					require.Empty(t, result.Skipped())
					require.ErrorContains(t, component.CombineResult(name, "", planned, result).Err, "containment denied")

					for _, outcome := range result.Results {
						require.Equal(t, name, outcome.Component)
						require.Empty(t, outcome.Site)

						if outcome.Ref.Name == target {
							require.Equal(t, component.OpSucceeded, outcome.Status)
						}
					}
				}

				stored := &corev1.Secret{}
				trust := &corev1.ConfigMap{}

				require.NoError(t, env.Client.Get(t.Context(), objectKey(env, tlsName), stored))
				require.NoError(t, env.Client.Get(t.Context(), objectKey(env, trustName), trust))
				require.NotEqual(t, old.Data["ca.crt"], stored.Data["ca.crt"])
				require.Equal(t, string(servingRoots(stored)), trust.Data["ca.crt"])
				require.NoError(t, verifyServing(t, stored, []byte(trust.Data["ca.crt"]), at))
				require.NoError(t, env.Client.Get(t.Context(), client.ObjectKey{Name: guardNames[0]}, guard))
				require.Empty(t, guard.Spec.Validations)
			})
		}
	}
}
