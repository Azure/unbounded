// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package controller reaps grants, mirrors them onto nodes, and deletes pods
// that grants do not allow.
package controller

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	runtimecontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/Azure/unbounded/internal/rebound/ledger"
)

const (
	// LabelNamespace identifies the namespace holding a node.
	LabelNamespace = "rebound.unbounded-cloud.io/namespace"
	// LabelGrant identifies the grant holding a node.
	LabelGrant = "rebound.unbounded-cloud.io/grant"
	// TaintKey is the PreferNoSchedule taint on granted nodes.
	TaintKey = "rebound.unbounded-cloud.io/granted"
)

// Options configures the grant controllers.
type Options struct {
	// Namespace defaults to unbounded-system and must match Store.Namespace.
	Namespace string
	Store     *ledger.Store
	// Clock defaults to the real clock.
	Clock clock.Clock
	// ExemptNamespaces are never evicted. Namespace is always exempt.
	ExemptNamespaces []string
}

// Setup registers three leader-elected controllers: the reaper, the node
// mirror, and the evictor. The manager must enable leader election and cache
// all Nodes and Namespaces, plus ConfigMaps in opts.Namespace. Scope ConfigMaps
// and Secrets to that namespace using per-object cache options, not a global
// namespace restriction. These controllers do not read Secrets. The evictor
// lists pods live, so Pods need not be cached.
func Setup(mgr ctrl.Manager, opts Options) error {
	if opts.Namespace == "" {
		opts.Namespace = "unbounded-system"
	}

	if opts.Store == nil || opts.Store.Client == nil {
		return errors.New("rebound controller requires a ledger store and client")
	}

	if opts.Store.Namespace != opts.Namespace {
		return errors.New("rebound controller and store namespaces must match")
	}

	if opts.Clock == nil {
		opts.Clock = clock.RealClock{}
	}

	reaper := &reaper{reader: mgr.GetCache(), apiReader: mgr.GetAPIReader(), store: opts.Store, clock: opts.Clock}
	filter := builder.WithPredicates(ledgerPredicate(opts.Namespace))

	leader := runtimecontroller.Options{NeedLeaderElection: ptr.To(true)}
	if err := ctrl.NewControllerManagedBy(mgr).
		Named("rebound-reaper").
		WithOptions(leader).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(singleton), filter).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(singleton)).
		Complete(reaper); err != nil {
		return fmt.Errorf("set up rebound reaper: %w", err)
	}

	mirror := &nodeMirror{client: mgr.GetClient(), reader: mgr.GetCache(), namespace: opts.Namespace, clock: opts.Clock}
	if err := ctrl.NewControllerManagedBy(mgr).
		Named("rebound-node-mirror").
		WithOptions(leader).
		For(&corev1.Node{}).
		Watches(&corev1.ConfigMap{}, mirror.ledgerHandler(), filter).
		Complete(mirror); err != nil {
		return fmt.Errorf("set up rebound node mirror: %w", err)
	}

	exempt := map[string]bool{opts.Namespace: true}
	for _, ns := range opts.ExemptNamespaces {
		exempt[ns] = true
	}

	evictor := &evictor{
		client:    mgr.GetClient(),
		reader:    mgr.GetCache(),
		apiReader: mgr.GetAPIReader(),
		namespace: opts.Namespace,
		exempt:    exempt,
		clock:     opts.Clock,
	}
	if err := ctrl.NewControllerManagedBy(mgr).
		Named("rebound-evictor").
		WithOptions(leader).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(evictor.mapLedger), filter).
		Complete(evictor); err != nil {
		return fmt.Errorf("set up rebound evictor: %w", err)
	}

	return nil
}

func ledgerPredicate(namespace string) predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetNamespace() == namespace && obj.GetName() == ledger.ConfigMapName
	})
}

func singleton(context.Context, client.Object) []ctrl.Request {
	return []ctrl.Request{{NamespacedName: types.NamespacedName{Name: ledger.ConfigMapName}}}
}
