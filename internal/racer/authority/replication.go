// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package authority

import (
	"context"
	"errors"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/racer/wire"
)

const ReplicationAudience = "racer-controller-replication"

func (a *Authority) Observe(ctx context.Context) error {
	if err := a.gate.Acquire(ctx); err != nil {
		return err
	}
	defer a.gate.Release()

	state, err := loadSigning(ctx, a.reader, a.config, time.Now())
	if err == nil {
		err = a.trust.install(ctx, state.roots, state.bundle)
	}

	if err == nil {
		_, record, readErr := readVersion(ctx, a.reader, a.config)

		err = readErr
		if err == nil {
			err = a.publications.confirm(record)
		}
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}

	if shouldInvalidateTrust(err) {
		a.trust.invalidate()
		a.publications.Suspend()
	}

	return err
}

func (a *Authority) AcceptReplica(ctx, process context.Context, image wire.Publication) error {
	encoded, err := wire.EncodePublication(image)
	if err != nil {
		return err
	}

	content, membership, err := wire.ContentHashes(image)
	if err != nil {
		return err
	}

	want := versionRecord{Cluster: image.Cluster, Sequence: image.Sequence, MembershipVersion: image.MembershipVersion, ContentHash: content, MembershipHash: membership}

	if err := a.gate.Acquire(ctx); err != nil {
		return err
	}
	defer a.gate.Release()

	_, record, err := readVersion(ctx, a.reader, a.config)
	if err != nil {
		if shouldInvalidateTrust(err) {
			a.publications.Suspend()
			a.trust.invalidate()
		}

		return err
	}

	if err := a.publications.confirm(record); err != nil {
		a.publications.Suspend()
		return err
	}

	if record != want {
		return wire.Unavailable
	}

	return a.publications.Install(&committedPublication{owner: a.publications, record: record, encoded: string(encoded), leadership: process})
}

type ReplicaIdentity struct {
	owner   *Authority
	uid     string
	expires time.Time
}

func (i ReplicaIdentity) UID() string        { return i.uid }
func (i ReplicaIdentity) Expires() time.Time { return i.expires }

func (a *Authority) AuthenticateReplica(ctx context.Context, request *http.Request) (ReplicaIdentity, error) {
	status, token, err := reviewBearer(ctx, a.client, request, ReplicationAudience, 0)
	if err != nil {
		return ReplicaIdentity{}, err
	}

	if status.User.Username != "system:serviceaccount:"+a.config.Namespace+":"+a.config.ControllerServiceAccount {
		return ReplicaIdentity{}, wire.Forbidden
	}

	name, uid := singleExtra(status.User, "pod-name"), singleExtra(status.User, "pod-uid")
	if name == "" || uid == "" || status.User.UID == "" {
		return ReplicaIdentity{}, wire.Unauthenticated
	}

	var pod corev1.Pod
	if err := a.reader.Get(ctx, client.ObjectKey{Namespace: a.config.Namespace, Name: name}, &pod); err != nil {
		return ReplicaIdentity{}, authorizationError(err)
	}

	if !controllerPod(a.config, &pod) || string(pod.UID) != uid {
		return ReplicaIdentity{}, wire.Forbidden
	}

	var sa corev1.ServiceAccount
	if err := a.reader.Get(ctx, client.ObjectKey{Namespace: a.config.Namespace, Name: a.config.ControllerServiceAccount}, &sa); err != nil {
		return ReplicaIdentity{}, authorizationError(err)
	}

	if string(sa.UID) != status.User.UID || sa.DeletionTimestamp != nil {
		return ReplicaIdentity{}, wire.Forbidden
	}

	expires, err := tokenExpiration(token)
	if err != nil {
		return ReplicaIdentity{}, err
	}

	return ReplicaIdentity{owner: a, uid: uid, expires: expires}, nil
}

func controllerPod(cfg Config, pod *corev1.Pod) bool {
	return pod.Namespace == cfg.Namespace && pod.UID != "" && pod.DeletionTimestamp == nil && pod.Spec.ServiceAccountName == cfg.ControllerServiceAccount && pod.Status.Phase != corev1.PodFailed && pod.Status.Phase != corev1.PodSucceeded
}
