// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"fmt"
	"maps"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/operator/component"
	racercore "github.com/Azure/unbounded/internal/racer"
	"github.com/Azure/unbounded/internal/racer/authority"
)

const (
	credentialsName  = "racer-credentials"
	credentialsClaim = "racer.unbounded-cloud.io/credentials"
	credentialsUID   = "racer.unbounded-cloud.io/credentials-uid"
)

func configAuthority(env *component.Env, cm *corev1.ConfigMap) (authority.Config, error) {
	cfg, err := racercore.ConfigFromLookup(func(key string) (string, bool) {
		if key == "POD_NAMESPACE" {
			return env.Namespace, true
		}

		value, ok := cm.Data[key]

		return value, ok
	})
	if err != nil {
		return authority.Config{}, err
	}

	return authority.Config{
		Cluster: cfg.Cluster, Namespace: cfg.Namespace,
		DataplaneServiceAccount: cfg.DataplaneServiceAccount, ControllerServiceAccount: cfg.ControllerServiceAccount,
		DaemonSetName: cfg.DaemonSetName, CredentialsSecretName: cfg.CredentialsSecretName,
		VersionConfigMapName: cfg.VersionConfigMapName, InstallationConfigMapName: cfg.InstallationConfigMapName,
		Rotation: cfg.Rotation, CertificateLifetime: cfg.CertificateLifetime, SnapshotMaxAge: cfg.SnapshotMaxAge,
		MaxTokenBytes: cfg.Limits.HeaderBytes,
	}, nil
}

func credentialsCommitted(ctx context.Context, env *component.Env) (bool, error) {
	version := &corev1.ConfigMap{}
	if err := env.LiveReader().Get(ctx, objectKey(env, versionName), version); err != nil {
		return false, err
	}

	claim, uid := version.Annotations[credentialsClaim], version.Annotations[credentialsUID]
	if (claim == "") != (uid == "") {
		return false, fmt.Errorf("inconsistent Racer credential commitment")
	}

	return claim != "", nil
}

func validateBootstrapConfig(cfg authority.Config, env *component.Env, marker *corev1.ConfigMap) error {
	if string(cfg.Cluster) != marker.Data["cluster"] || cfg.Namespace != env.Namespace ||
		cfg.InstallationConfigMapName != markerName || cfg.VersionConfigMapName != versionName || cfg.CredentialsSecretName != credentialsName ||
		cfg.ControllerServiceAccount != controllerName || cfg.DataplaneServiceAccount != "racer-dataplane" || cfg.DaemonSetName != "racer-dataplane" {
		return fmt.Errorf("racer bootstrap configuration does not match installation")
	}

	return nil
}

func planBootstrap(ctx context.Context, env *component.Env, runtime *component.Plan, marker *corev1.ConfigMap) (*component.Plan, bool, error) {
	plan := component.NewPlan()
	// Persist canonical configuration in its own pass before any authority write.
	for _, op := range runtime.Operations {
		if op.Object.GetKind() == "ConfigMap" && op.Object.GetName() == configName {
			plan.Add(op)
		}
	}

	if plan.Len() != 0 {
		return plan, true, nil
	}

	cm := &corev1.ConfigMap{}
	if err := env.LiveReader().Get(ctx, objectKey(env, configName), cm); err != nil {
		return nil, false, err
	}

	if cm.UID == "" || cm.ResourceVersion == "" || cm.DeletionTimestamp != nil {
		return nil, false, fmt.Errorf("racer bootstrap configuration is not durable")
	}

	cfg, err := configAuthority(env, cm)
	if err != nil {
		return nil, false, err
	}

	if err := validateBootstrapConfig(cfg, env, marker); err != nil {
		return nil, false, err
	}

	if marker.Data["state"] == "consumed" {
		owner := authority.New(cfg, authority.Dependencies{Reader: env.LiveReader()})
		if err := owner.Recover(ctx, planningWriter{}); err != nil {
			return nil, false, err
		}

		committed, err := credentialsCommitted(ctx, env)
		if err != nil {
			return nil, false, err
		}

		if committed {
			return nil, false, owner.ValidatePersistedCredentials(ctx)
		}
	}

	expectedMarker, expectedConfig := marker.DeepCopy(), cm.DeepCopy()
	anchor := marker.DeepCopy()
	anchor.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}
	plan.Add(component.Operation{Kind: component.OpRun, Component: name, Object: component.ToUnstructured(anchor), Run: func(ctx context.Context) error {
		return bootstrapAuthority(ctx, env, expectedMarker, expectedConfig)
	}})

	return plan, true, nil
}

func bootstrapAuthority(ctx context.Context, env *component.Env, expectedMarker, expectedConfig *corev1.ConfigMap) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	caches := &racerv1.ClusterCacheList{}
	if err := env.LiveReader().List(ctx, caches, client.Limit(1)); err != nil {
		return err
	}

	if len(caches.Items) == 0 {
		return fmt.Errorf("racer bootstrap no longer has active caches")
	}

	claim := &corev1.ConfigMap{}
	if err := env.LiveReader().Get(ctx, objectKey(env, claimName), claim); err != nil {
		return err
	}

	marker, err := claimedMarker(ctx, env, claim)
	if err != nil {
		return err
	}

	if marker.UID != expectedMarker.UID || marker.Annotations[claimAnnotation] != expectedMarker.Annotations[claimAnnotation] || marker.Data["cluster"] != expectedMarker.Data["cluster"] {
		return fmt.Errorf("racer bootstrap installation changed")
	}

	cm := &corev1.ConfigMap{}
	if err := env.LiveReader().Get(ctx, objectKey(env, configName), cm); err != nil {
		return err
	}

	if cm.DeletionTimestamp != nil || cm.UID != expectedConfig.UID || cm.ResourceVersion != expectedConfig.ResourceVersion || !maps.Equal(cm.Data, expectedConfig.Data) {
		return fmt.Errorf("racer bootstrap configuration changed")
	}

	cfg, err := configAuthority(env, cm)
	if err != nil {
		return err
	}

	if err := validateBootstrapConfig(cfg, env, marker); err != nil {
		return err
	}

	tls := &corev1.Secret{}
	if err := env.LiveReader().Get(ctx, objectKey(env, tlsName), tls); err != nil {
		return err
	}

	if tls.DeletionTimestamp != nil {
		return fmt.Errorf("racer bootstrap TLS is terminating")
	}

	if _, err := renewTLS(tls, env.Namespace, time.Now()); err != nil {
		return err
	}

	writer := bootstrapWriter{planningWriter: planningWriter{}, writer: env.Client, namespace: env.Namespace}

	owner := authority.New(cfg, authority.Dependencies{Reader: env.LiveReader(), Writer: writer})
	if err := owner.Recover(ctx, writer); err != nil {
		return err
	}

	committed, err := credentialsCommitted(ctx, env)
	if err != nil {
		return err
	}

	if committed {
		return owner.ValidatePersistedCredentials(ctx)
	}

	_, err = owner.ReconcileCredentials(ctx)

	return err
}

// Initial installation creates candidates and CAS-updates only their parents.
// Reject Secret updates so a competing initializer cannot trigger rotation here.
type bootstrapWriter struct {
	planningWriter
	writer    client.Writer
	namespace string
}

func (w bootstrapWriter) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	allowed := false

	switch obj.(type) {
	case *corev1.ConfigMap:
		allowed = obj.GetName() == versionName
	case *corev1.Secret:
		allowed = obj.GetName() == credentialsName
	}

	if !allowed || obj.GetNamespace() != w.namespace {
		return fmt.Errorf("racer bootstrap rejected create")
	}

	return w.writer.Create(ctx, obj, opts...)
}

func (w bootstrapWriter) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	_, cm := obj.(*corev1.ConfigMap)
	if !cm || obj.GetNamespace() != w.namespace || (obj.GetName() != markerName && obj.GetName() != versionName) || obj.GetUID() == "" || obj.GetResourceVersion() == "" {
		return fmt.Errorf("racer bootstrap rejected update")
	}

	return w.writer.Update(ctx, obj, opts...)
}
