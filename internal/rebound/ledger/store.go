// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package ledger

import (
	"context"
	"errors"
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

// cleanupMaxSize leaves room for metadata below the ConfigMap's 1 MiB limit.
const cleanupMaxSize = 1000 * 1024

// ErrTooLarge is returned when a write would exceed its ledger size limit.
var ErrTooLarge = errors.New("ledger is full")

// Store reads and writes the ledger ConfigMap.
type Store struct {
	Client    kubernetes.Interface
	Namespace string
}

// Update reads the ledger, applies mutate, and writes the result with an
// optimistic concurrency check on the resourceVersion it read. Write
// conflicts are retried with a fresh read. If mutate returns an error,
// nothing is written and the error is returned unchanged. If the ledger is
// unchanged, or dryRun is set, nothing is written.
//
// Update returns the resulting ledger and the ConfigMap resourceVersion it
// corresponds to (for dryRun, the version that was read).
func (s *Store) Update(ctx context.Context, dryRun bool, mutate func(*Ledger) error) (*Ledger, string, error) {
	var (
		out *Ledger
		rv  string
	)

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cm, err := s.Get(ctx)
		if err != nil {
			return err
		}

		l := Parse(cm.Data)

		grantCount := len(l.grants)
		if err := mutate(l); err != nil {
			return err
		}

		data := l.Encode()
		// Cleanup can grow release records, so reserve space beyond the grant limit.
		limit := cleanupMaxSize
		if len(l.grants) > grantCount {
			limit = MaxSize
		}

		if Size(data) > limit {
			return ErrTooLarge
		}

		out, rv = l, cm.ResourceVersion

		if dryRun || maps.Equal(data, cm.Data) {
			return nil
		}

		cm = cm.DeepCopy()
		cm.Data = data

		if cm.ResourceVersion == "" {
			cm, err = s.Client.CoreV1().ConfigMaps(s.Namespace).Create(ctx, cm, metav1.CreateOptions{})
		} else {
			cm, err = s.Client.CoreV1().ConfigMaps(s.Namespace).Update(ctx, cm, metav1.UpdateOptions{})
		}

		if err != nil {
			if apierrors.IsAlreadyExists(err) {
				// Lost a race creating the ledger; retry against it.
				return apierrors.NewConflict(corev1.Resource("configmaps"), ConfigMapName, err)
			}

			return err
		}

		rv = cm.ResourceVersion

		return nil
	})
	if err != nil {
		return nil, "", err
	}

	return out, rv, nil
}

// Get reads the ledger ConfigMap live, or returns an unsaved empty one if it does not
// exist yet.
func (s *Store) Get(ctx context.Context) (*corev1.ConfigMap, error) {
	cm, err := s.Client.CoreV1().ConfigMaps(s.Namespace).Get(ctx, ConfigMapName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: s.Namespace},
		}, nil
	}

	if err != nil {
		return nil, fmt.Errorf("get ledger: %w", err)
	}

	return cm, nil
}
