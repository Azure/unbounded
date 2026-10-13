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
	var (
		removed []*ledger.Grant
		next    time.Time
	)

	_, _, err := r.store.Update(ctx, false, func(l *ledger.Ledger) error {
		now := r.clock.Now()
		keep := make(map[string]bool)

		for _, g := range l.Grants() {
			if g.Expired(now) {
				continue
			}

			if _, checked := keep[g.Namespace]; checked {
				continue
			}

			var ns corev1.Namespace

			err := r.reader.Get(ctx, types.NamespacedName{Name: g.Namespace}, &ns)
			if apierrors.IsNotFound(err) {
				err = r.apiReader.Get(ctx, types.NamespacedName{Name: g.Namespace}, &ns)
			}

			if err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("get grant namespace %s: %w", g.Namespace, err)
			}

			keep[g.Namespace] = err == nil && ns.DeletionTimestamp.IsZero()
		}

		removed, next = l.Reap(now, func(g *ledger.Grant) bool { return keep[g.Namespace] })

		return nil
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	for _, g := range removed {
		ctrl.LoggerFrom(ctx).Info("removed node grant", "namespace", g.Namespace, "grant", g.Name, "expiresAt", g.ExpiresAt)
	}

	if next.IsZero() {
		return ctrl.Result{}, nil
	}

	return ctrl.Result{RequeueAfter: max(time.Second, next.Sub(r.clock.Now())+time.Second)}, nil
}
