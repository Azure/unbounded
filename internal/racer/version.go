// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"encoding/hex"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/racer/wire"
)

const installationUIDAnnotation = "racer.unbounded-cloud.io/installation-uid"

// Initialize consumes a new installation's permanent marker before the sole
// counter Create attempt. It does not run controllers or acquire serving authority.
func Initialize(ctx context.Context, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return err
	}

	restConfig, err := ctrl.GetConfig()
	if err != nil {
		return err
	}

	c, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}

	r := &TopologyReconciler{Client: c, APIReader: c, Config: cfg}

	return r.InitializeVersion(ctx)
}

func (r *TopologyReconciler) installation(ctx context.Context, fresh bool) (*corev1.ConfigMap, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	cm := &corev1.ConfigMap{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: r.Config.Namespace, Name: r.Config.InstallationConfigMapName}, cm); err != nil {
		return nil, err
	}

	state := "consumed"
	if fresh {
		state = "fresh"
	}

	immutable := cm.Immutable != nil && *cm.Immutable
	if cm.UID == "" || cm.ResourceVersion == "" || cm.DeletionTimestamp != nil || cm.Data["cluster"] != string(r.Config.Cluster) || cm.Data["version_configmap"] != r.Config.VersionConfigMapName || cm.Data["state"] != state || immutable == fresh {
		return nil, fmt.Errorf("installation marker invalid or already consumed: %w", wire.Unavailable)
	}

	return cm, nil
}

// InitializeVersion never retries marker CAS or counter creation, including
// ambiguous transport failures. See the initialization crash table in the design.
func (r *TopologyReconciler) InitializeVersion(ctx context.Context) error {
	if !wire.ValidUUID(string(r.Config.Cluster)) {
		return wire.InvalidRequest
	}

	marker, err := r.installation(ctx, true)
	if err != nil {
		return err
	}

	key := client.ObjectKey{Namespace: r.Config.Namespace, Name: r.Config.VersionConfigMapName}
	if err := r.APIReader.Get(ctx, key, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		if err != nil {
			return err
		}

		return fmt.Errorf("version state already exists: %w", wire.Conflict)
	}

	content, membership, err := wire.ContentHashes(wire.Publication{SchemaVersion: wire.SchemaVersion, Cluster: r.Config.Cluster})
	if err != nil {
		return err
	}

	marker.Data["state"] = "consumed"
	immutable := true
	marker.Immutable = &immutable

	if err := ctx.Err(); err != nil {
		return err
	}

	if err := r.Update(ctx, marker); err != nil {
		return err
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	return r.Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name, Annotations: map[string]string{installationUIDAnnotation: string(marker.UID)}},
		Data:       versionData(VersionRecord{Cluster: r.Config.Cluster, Sequence: 1, MembershipVersion: 1, ContentHash: content, MembershipHash: membership}),
	})
}

func versionData(v VersionRecord) map[string]string {
	return map[string]string{"cluster": string(v.Cluster), "sequence": strconv.FormatUint(uint64(v.Sequence), 10), "membership_version": strconv.FormatUint(uint64(v.MembershipVersion), 10), "content_hash": v.ContentHash, "membership_hash": v.MembershipHash}
}

func validHash(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32 && hex.EncodeToString(b) == s
}

func (v VersionRecord) valid() bool {
	return wire.ValidUUID(string(v.Cluster)) && v.Sequence > 0 && v.MembershipVersion > 0 && uint64(v.MembershipVersion) <= uint64(v.Sequence) && validHash(v.ContentHash) && validHash(v.MembershipHash)
}

func parseVersion(cm *corev1.ConfigMap, cluster wire.ClusterID, markerUID types.UID) (VersionRecord, error) {
	sequence, e1 := strconv.ParseUint(cm.Data["sequence"], 10, 64)
	membership, e2 := strconv.ParseUint(cm.Data["membership_version"], 10, 64)

	v := VersionRecord{Cluster: wire.ClusterID(cm.Data["cluster"]), Sequence: wire.Sequence(sequence), MembershipVersion: wire.MembershipVersion(membership), ContentHash: cm.Data["content_hash"], MembershipHash: cm.Data["membership_hash"]}
	if e1 != nil || e2 != nil || !v.valid() || v.Cluster != cluster || cm.ResourceVersion == "" || cm.DeletionTimestamp != nil || cm.Annotations[installationUIDAnnotation] != string(markerUID) || strconv.FormatUint(sequence, 10) != cm.Data["sequence"] || strconv.FormatUint(membership, 10) != cm.Data["membership_version"] {
		return VersionRecord{}, fmt.Errorf("durable version state invalid; explicit new-cluster rebootstrap required: %w", wire.Unavailable)
	}

	return v, nil
}

func (r *TopologyReconciler) readVersion(ctx context.Context) (*corev1.ConfigMap, VersionRecord, error) {
	marker, err := r.installation(ctx, false)
	if err != nil {
		return nil, VersionRecord{}, err
	}

	cm := &corev1.ConfigMap{}

	if err := ctx.Err(); err != nil {
		return nil, VersionRecord{}, err
	}

	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: r.Config.Namespace, Name: r.Config.VersionConfigMapName}, cm); err != nil {
		return nil, VersionRecord{}, err
	}

	v, err := parseVersion(cm, r.Config.Cluster, marker.UID)

	return cm, v, err
}

// ValidateInstallation checks the same durable marker and counter binding that
// normal controller startup requires, without modifying either object.
func ValidateInstallation(ctx context.Context, reader client.Reader, namespace, cluster string) error {
	r := &TopologyReconciler{APIReader: reader, Config: Config{
		Namespace: namespace, Cluster: wire.ClusterID(cluster),
		InstallationConfigMapName: "racer-installation", VersionConfigMapName: "racer-version",
	}}
	_, _, err := r.readVersion(ctx)

	return err
}

// CommitVersion mints the only installable type after a resource-version CAS.
// Even unchanged content is CAS-confirmed; its counters and bytes remain identical.
func (r *TopologyReconciler) CommitVersion(ctx context.Context, p *PreparedPublication) (*CommittedPublication, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if p == nil || p.owner != r.Publications || p.previous.Cluster != r.Config.Cluster {
		return nil, wire.InvalidRequest
	}

	cm, previous, err := r.readVersion(ctx)
	if err != nil {
		r.Publications.Suspend()
		return nil, err
	}

	if cm.ResourceVersion != p.resourceVersion || previous != p.previous {
		return nil, apierrors.NewConflict(corev1.Resource("configmaps"), cm.Name, wire.Conflict)
	}

	cm.Data = versionData(p.record)

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if err := r.Update(ctx, cm); err != nil {
		return nil, err
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return &CommittedPublication{owner: p.owner, record: p.record, encoded: p.encoded, leadership: ctx}, nil
}
