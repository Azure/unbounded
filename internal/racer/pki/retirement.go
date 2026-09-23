// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package pki

import (
	"context"
	"errors"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// checkLivePod is an additional admission check, not a replacement for the
// caller's credential, ownership, and topology authorization. It runs after the
// Secret read on every CAS attempt. A concurrent retirement/collection commit
// therefore forces a retry and a new Pod read before issuance can succeed.
func (m *Manager) checkLivePod(ctx context.Context, s *state, identity Identity) error {
	if !s.RequireLivePod {
		return nil
	}

	if identity.PodName == "" || len(validation.IsDNS1123Subdomain(identity.PodName)) != 0 {
		return errors.New("admission after retirement collection requires a Kubernetes Pod name")
	}

	var pod corev1.Pod
	if err := m.client.Get(ctx, m.objectKey(identity.PodName), &pod); err != nil {
		return err
	}

	if string(pod.UID) != identity.PodUID {
		return errors.New("admission Pod UID no longer exists in Kubernetes")
	}

	return nil
}

// CollectRetirements forgets only tombstones for Pod UIDs absent from an
// authoritative namespace list. Kubernetes never reuses a deleted object's UID.
// A label change, termination timestamp, or certificate expiry is not absence.
// Old boots of live Pods (including the CP pending placeholder) remain blocked.
// Storage is thus prunable across Pod replacement, not bounded for unlimited
// restarts within one Pod UID: boot nonces have no authoritative ordering.
//
// The live-Pod admission requirement and removals share the Secret CAS. Older
// binaries reject the new field rather than silently admitting collected UIDs.
// Reads must use the direct API client supplied to New, not an informer cache.
// CA expiry watermarks and durable members are deliberately unaffected.
func (m *Manager) CollectRetirements(ctx context.Context) error {
	return m.mutate(ctx, func(s *state) error {
		var pods corev1.PodList
		if err := m.client.List(ctx, &pods, client.InNamespace(m.namespace)); err != nil {
			return err
		}

		live := make(map[string]bool, len(pods.Items))
		for _, pod := range pods.Items {
			live[string(pod.UID)] = true
		}

		s.RequireLivePod = true
		for key := range s.Retired {
			uid, boot, ok := strings.Cut(key, "/")
			if !ok || !processID.MatchString(uid) || !processID.MatchString(boot) {
				return errors.New("invalid retirement key")
			}

			if !live[uid] {
				delete(s.Retired, key)
			}
		}

		return nil
	})
}
