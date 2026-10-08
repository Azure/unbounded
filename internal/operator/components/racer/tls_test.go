// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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
					env := testEnv(t, secret)
					plan := component.NewPlan()
					now := stateOf(t, secret).CreatedAt.Add(-tc.skew)
					stored, err := planTLSAt(t.Context(), env, plan, false, now)
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
			delete(secret.Data, key)
			env := testEnv(t, secret)
			plan := component.NewPlan()
			value, err := planTLSAt(t.Context(), env, plan, false, now.Add(caRotationInterval))
			require.Error(t, err)
			require.Nil(t, value)
			require.Zero(t, plan.Len())
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

			_, err := renewTLS(secret, "custom-system", now.Add(caRotationInterval))
			require.Error(t, err)
		})
	}

	_, err = renewTLS(original, "custom-system", now.Add(caLifetime))
	require.ErrorContains(t, err, "CA expired")
	_, err = renewTLS(original, "other-system", now)
	require.Error(t, err)
}

func TestTLSPersistBeforeTrustAndCAS(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	env := testEnv(t)

	first, second := component.NewPlan(), component.NewPlan()
	for _, plan := range []*component.Plan{first, second} {
		secret, err := planTLSAt(t.Context(), env, plan, true, now)
		require.NoError(t, err)
		require.Nil(t, secret)
		require.Len(t, plan.Operations, 1)
	}

	persist(t, env, first)
	persist(t, env, second)
	winner, err := planTLSAt(t.Context(), env, component.NewPlan(), false, now)
	require.NoError(t, err)
	require.NotNil(t, winner)

	first, second = component.NewPlan(), component.NewPlan()

	at := now.Add(caRotationInterval)
	for _, plan := range []*component.Plan{first, second} {
		secret, err := planTLSAt(t.Context(), env, plan, false, at)
		require.NoError(t, err)
		require.Nil(t, secret)
	}

	persist(t, env, first)
	result, err := env.Execute(t.Context(), second)
	require.NoError(t, err)
	require.Len(t, result.Deferred, 1)

	restart := component.NewPlan()
	stored, err := planTLSAt(t.Context(), env, restart, false, at)
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
			_, err := planTLSAt(t.Context(), env, plan, scenario != "established-missing", time.Now())
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
			require.Equal(t, component.ReasonDisabled, result.Reason)
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
	for _, scenario := range []string{"create-response-lost", "patch-response-lost", "patch-forbidden"} {
		t.Run(scenario, func(t *testing.T) {
			now := time.Now().UTC().Truncate(time.Second)
			env := testEnv(t)

			fresh := scenario == "create-response-lost"
			if !fresh {
				secret, err := newTLS(env.Namespace, now.Add(-caRotationInterval))
				require.NoError(t, err)
				require.NoError(t, env.Client.Create(t.Context(), secret))
			}

			plan := component.NewPlan()
			value, err := planTLSAt(t.Context(), env, plan, fresh, now)
			require.NoError(t, err)
			require.Nil(t, value)

			executor := *env
			executor.Client = interceptor.NewClient(env.Client.(client.WithWatch), interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					require.NoError(t, c.Create(ctx, obj, opts...))
					return errors.New("lost create response")
				},
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if scenario == "patch-forbidden" {
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
			value, err = planTLSAt(t.Context(), env, restart, false, now)
			require.NoError(t, err)

			if scenario == "patch-forbidden" {
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
