// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	"github.com/Azure/unbounded/internal/rebound/ledger"
)

type nodeMirror struct {
	client    client.Client
	reader    client.Reader
	namespace string
	clock     clock.Clock
}

func (r *nodeMirror) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var node corev1.Node
	if err := r.reader.Get(ctx, req.NamespacedName, &node); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var cm corev1.ConfigMap
	if err := r.reader.Get(ctx, types.NamespacedName{Namespace: r.namespace, Name: ledger.ConfigMapName}, &cm); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	l := ledger.Parse(cm.Data)

	var grant *ledger.Grant
	if key, ok := l.NodeGrant(node.Name); ok {
		grant, _ = l.Get(key)
		if grant.Expired(r.clock.Now()) {
			grant = nil
		}
	}

	before := node.DeepCopy()
	mirrorGrant(&node, grant)

	if !reflect.DeepEqual(before.Labels, node.Labels) || !reflect.DeepEqual(before.Spec.Taints, node.Spec.Taints) {
		if err := r.client.Patch(ctx, &node, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
	}

	if grant != nil {
		return ctrl.Result{RequeueAfter: max(time.Nanosecond, grant.ExpiresAt.Sub(r.clock.Now()))}, nil
	}

	return ctrl.Result{}, nil
}

func mirrorGrant(node *corev1.Node, grant *ledger.Grant) {
	if grant == nil {
		delete(node.Labels, LabelNamespace)
		delete(node.Labels, LabelGrant)
	} else {
		if node.Labels == nil {
			node.Labels = make(map[string]string)
		}

		node.Labels[LabelNamespace] = grant.Namespace
		node.Labels[LabelGrant] = grant.Name
	}

	var desired corev1.Taint
	if grant != nil {
		desired = corev1.Taint{Key: TaintKey, Value: grant.Namespace, Effect: corev1.TaintEffectPreferNoSchedule}
	}

	found := false

	taints := node.Spec.Taints[:0]
	for _, taint := range node.Spec.Taints {
		if taint.Key != TaintKey {
			taints = append(taints, taint)
		} else if grant != nil && !found {
			taints = append(taints, desired)
			found = true
		}
	}

	if grant != nil && !found {
		taints = append(taints, desired)
	}

	node.Spec.Taints = taints
}

// ledgerHandler enqueues every node named by the ledger before and after a
// change. Nodes dropped from the ledger appear only in the old object, so
// using both lets the mirror clear their labels and taint without a separate
// node list that could fail and lose the event. Nodes left over from before a
// restart are covered because the Node watch enqueues every node on start.
func (r *nodeMirror) ledgerHandler() handler.EventHandler {
	enqueue := func(q workqueue.TypedRateLimitingInterface[ctrl.Request], objs ...client.Object) {
		for _, req := range r.ledgerNodes(objs...) {
			q.Add(req)
		}
	}

	return handler.Funcs{
		CreateFunc: func(_ context.Context, e event.CreateEvent, q workqueue.TypedRateLimitingInterface[ctrl.Request]) {
			enqueue(q, e.Object)
		},
		UpdateFunc: func(_ context.Context, e event.UpdateEvent, q workqueue.TypedRateLimitingInterface[ctrl.Request]) {
			enqueue(q, e.ObjectOld, e.ObjectNew)
		},
		DeleteFunc: func(_ context.Context, e event.DeleteEvent, q workqueue.TypedRateLimitingInterface[ctrl.Request]) {
			enqueue(q, e.Object)
		},
		GenericFunc: func(_ context.Context, e event.GenericEvent, q workqueue.TypedRateLimitingInterface[ctrl.Request]) {
			enqueue(q, e.Object)
		},
	}
}

// ledgerNodes returns one request per node named by any of the ledger objects.
func (r *nodeMirror) ledgerNodes(objs ...client.Object) []ctrl.Request {
	names := make(map[string]struct{})

	for _, obj := range objs {
		cm, ok := obj.(*corev1.ConfigMap)
		if !ok || cm.Namespace != r.namespace || cm.Name != ledger.ConfigMapName {
			continue
		}

		for _, g := range ledger.Parse(cm.Data).Grants() {
			for _, node := range g.Nodes {
				names[node] = struct{}{}
			}
		}
	}

	requests := make([]ctrl.Request, 0, len(names))
	for name := range names {
		requests = append(requests, ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	}

	return requests
}
