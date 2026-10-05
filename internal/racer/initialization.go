// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/racer/wire"
)

const (
	initializationProtocol       = "racer.unbounded-cloud.io/initialization"
	markerInitializationProtocol = "initialization_protocol"
	stagedInitialization         = "staged-v1"
	versionUID                   = "version_uid"
	credentialUID                = "racer.unbounded-cloud.io/credentials-uid"
)

// Staging is opt-in for new installations. A candidate is not authority: the
// permanent parent CAS binds its Kubernetes UID before any reader can use it.
// Retrying Create cannot restore deleted authority because the UID changes.
// Never upgrade a consumed legacy marker: its missing state is ambiguous.
func ensureStagedInstallation(ctx context.Context, writer client.Writer, reader client.Reader, cfg Config, marker *corev1.ConfigMap) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	for {
		err := stageInstallation(ctx, writer, reader, cfg, marker)
		if !apierrors.IsConflict(err) && !apierrors.IsAlreadyExists(err) {
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}

		marker = &corev1.ConfigMap{}
		if err := reader.Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: cfg.InstallationConfigMapName}, marker); err != nil {
			return err
		}
	}
}

func stageInstallation(ctx context.Context, writer client.Writer, reader client.Reader, cfg Config, marker *corev1.ConfigMap) error {
	if marker.Data[markerInitializationProtocol] != stagedInitialization {
		return wire.Unavailable
	}

	if marker.Data["state"] == "consumed" {
		_, _, err := readVersion(ctx, reader, cfg)
		return err
	}

	if err := validateMarker(marker, cfg, true); err != nil {
		return err
	}

	if marker.Data[versionUID] != "" {
		return wire.Unavailable
	}

	content, membership, err := wire.ContentHashes(wire.Publication{SchemaVersion: wire.SchemaVersion, Cluster: cfg.Cluster})
	if err != nil {
		return err
	}

	data := versionData(VersionRecord{Cluster: cfg.Cluster, Sequence: 1, MembershipVersion: 1, ContentHash: content, MembershipHash: membership})
	key := client.ObjectKey{Namespace: cfg.Namespace, Name: cfg.VersionConfigMapName}

	candidate := &corev1.ConfigMap{}
	if err := reader.Get(ctx, key, candidate); apierrors.IsNotFound(err) {
		candidate = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name, Annotations: map[string]string{installationUIDAnnotation: string(marker.UID), initializationProtocol: stagedInitialization}}, Data: data}

		if err := ctx.Err(); err != nil {
			return err
		}

		if err := writer.Create(ctx, candidate); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	// A concurrent winner may already have committed and advanced the candidate.
	if _, _, err := readVersion(ctx, reader, cfg); err == nil {
		return nil
	}

	if candidate.UID == "" || candidate.ResourceVersion == "" || candidate.DeletionTimestamp != nil || (candidate.Immutable != nil && *candidate.Immutable) || candidate.Annotations[installationUIDAnnotation] != string(marker.UID) || candidate.Annotations[initializationProtocol] != stagedInitialization || candidate.Annotations[credentialClaim] != "" || !reflect.DeepEqual(candidate.Data, data) {
		return wire.Unavailable
	}

	marker.Data[versionUID] = string(candidate.UID)
	marker.Data["state"] = "consumed"
	immutable := true
	marker.Immutable = &immutable

	if err := ctx.Err(); err != nil {
		return err
	}

	if err := writer.Update(ctx, marker); err != nil {
		return err
	}

	_, _, err = readVersion(ctx, reader, cfg)

	return err
}

// Commit only a complete generation-one candidate from this installation. The
// material stays exclusively in the ordinary credentials Secret. No pending
// private-key copy, second Secret, or new RBAC permission is necessary.
func (r *KeyringReconciler) commitStagedCredentials(ctx context.Context, version *corev1.ConfigMap, secret *corev1.Secret) (ctrl.Result, error) {
	cfg := r.runtimeConfig()

	claim := secret.Annotations[credentialClaim]
	if version.Annotations[credentialClaim] != "" || version.Annotations[credentialUID] != "" || secret.UID == "" || secret.ResourceVersion == "" || secret.DeletionTimestamp != nil || (secret.Immutable != nil && *secret.Immutable) || secret.Annotations[initializationProtocol] != stagedInitialization || secret.Annotations[installationUIDAnnotation] != version.Annotations[installationUIDAnnotation] || !validCredentialClaim(cfg, claim) {
		return ctrl.Result{}, wire.Unavailable
	}

	bundle, err := wire.DecodeBundle(bytes.NewReader(secret.Data["bundle.json"]))
	if err != nil {
		return ctrl.Result{}, wire.Unavailable
	}

	candidate := credentialState{bundle: bundle}
	if bundle.Cluster != cfg.Cluster || bundle.Generation != 1 || decodeCredentialMetadata(secret.Data["rotation.json"], &candidate.rotation) != nil || decodeCredentialMetadata(secret.Data["issuer.json"], &candidate.material) != nil {
		return ctrl.Result{}, wire.Unavailable
	}

	if err := candidate.validateRotation(); err != nil {
		return ctrl.Result{}, err
	}

	if claim != cfg.CredentialsSecretName+"/"+candidate.rotation.ActiveIssuer || candidate.rotation.PreparedIssuer != "" {
		return ctrl.Result{}, wire.Unavailable
	}

	version.Annotations[credentialClaim] = claim
	version.Annotations[credentialUID] = string(secret.UID)

	if err := ctx.Err(); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.Update(ctx, version); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: time.Second}, nil
}
