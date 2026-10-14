// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/rebound/ledger"
)

type reaper struct {
	reader    client.Reader
	apiReader client.Reader
	store     *ledger.Store
	clock     clock.Clock
}

func (r *reaper) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	type namespaceKey struct {
		name string
		uid  types.UID
	}

	var (
		removed []*ledger.Grant
		next    time.Time
	)

	_, _, err := r.store.Update(ctx, false, func(l *ledger.Ledger) error {
		now := r.clock.Now()

		// Releases read here have committed, so confirm any that a lost
		// follow-up write left unconfirmed.
		l.Confirm(now)

		keep := make(map[namespaceKey]bool)

		for _, g := range l.Grants() {
			if g.Expired(now) {
				continue
			}

			key := namespaceKey{g.Namespace, g.NamespaceUID}
			if _, checked := keep[key]; checked {
				continue
			}

			var ns corev1.Namespace

			err := r.reader.Get(ctx, types.NamespacedName{Name: g.Namespace}, &ns)
			if apierrors.IsNotFound(err) || (err == nil && g.NamespaceUID != "" && ns.UID != g.NamespaceUID) {
				err = r.apiReader.Get(ctx, types.NamespacedName{Name: g.Namespace}, &ns)
			}

			if err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("get grant namespace %s: %w", g.Namespace, err)
			}

			keep[key] = err == nil && ns.DeletionTimestamp.IsZero() && (g.NamespaceUID == "" || ns.UID == g.NamespaceUID)
		}

		// Re-read the clock after the namespace lookups: a stale time would
		// backdate ReleasedAt for removed live grants, shortening both the
		// handoff cooldown and release retention.
		removed, next = l.Reap(r.clock.Now(), func(g *ledger.Grant) bool { return keep[namespaceKey{g.Namespace, g.NamespaceUID}] })

		return nil
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	for _, g := range removed {
		ctrl.LoggerFrom(ctx).Info("removed node grant", "namespace", g.Namespace, "grant", g.Name, "expiresAt", g.ExpiresAt)
	}

	// Live removals are written unconfirmed. Now that they have committed,
	// confirm them to start the handoff cooldown. On failure, the retry
	// confirms them at the start of its sweep.
	if len(removed) != 0 {
		if err := r.store.Confirm(ctx, r.clock); err != nil {
			return ctrl.Result{}, fmt.Errorf("confirm node grant removals: %w", err)
		}
	}

	if next.IsZero() {
		return ctrl.Result{}, nil
	}

	return ctrl.Result{RequeueAfter: max(time.Second, next.Sub(r.clock.Now())+time.Second)}, nil
}
