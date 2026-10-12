// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
)

type gatewayInformerSnapshot struct {
	generation   uint64
	available    bool
	pools        cache.SharedIndexInformer
	nodes        cache.SharedIndexInformer
	poolPeerings cache.SharedIndexInformer
}

type gatewayInformerGeneration struct {
	gatewayInformerSnapshot
	cancel   context.CancelFunc
	cleanups []func()
}

type gatewayInformerLifecycle struct {
	client dynamic.Interface
	resync time.Duration

	mu         sync.RWMutex
	required   bool
	starting   bool
	retiring   bool
	generation uint64
	active     *gatewayInformerGeneration
	onEvent    func()
	lastErr    error
}

func newGatewayInformerLifecycle(client dynamic.Interface, resync time.Duration) *gatewayInformerLifecycle {
	return &gatewayInformerLifecycle{
		client: client,
		resync: resync,
	}
}

func (l *gatewayInformerLifecycle) setEventHandler(onEvent func()) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.onEvent = onEvent
}

func (l *gatewayInformerLifecycle) setRequired(required bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.required = required
	l.retiring = !required && l.active != nil
}

func (l *gatewayInformerLifecycle) readiness() (bool, string) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	if l.retiring {
		return false, "gateway informer caches are retained until dependent routes and peers are removed"
	}

	if !l.required {
		return true, ""
	}

	if l.starting {
		return false, "required gateway informer caches are starting"
	}

	if l.active == nil {
		if l.lastErr != nil {
			return false, fmt.Sprintf("required gateway informer caches unavailable: %v", l.lastErr)
		}

		return false, "required gateway informer caches are not running"
	}

	if !gatewayInformerSnapshotSynced(l.active.gatewayInformerSnapshot) {
		return false, "required gateway informer caches are not synced"
	}

	return true, ""
}

func (l *gatewayInformerLifecycle) snapshot() gatewayInformerSnapshot {
	l.mu.RLock()
	defer l.mu.RUnlock()

	if l.active == nil {
		return gatewayInformerSnapshot{}
	}

	return l.active.gatewayInformerSnapshot
}

func (l *gatewayInformerLifecycle) prepare(ctx context.Context, required bool) (gatewayInformerSnapshot, bool, error) {
	l.setRequired(required)

	l.mu.RLock()
	active := l.active
	l.mu.RUnlock()

	if !required {
		if active == nil {
			return gatewayInformerSnapshot{}, false, nil
		}

		return active.gatewayInformerSnapshot, true, nil
	}

	if active != nil {
		if !gatewayInformerSnapshotSynced(active.gatewayInformerSnapshot) {
			return gatewayInformerSnapshot{}, false, fmt.Errorf("required gateway informer generation %d is not synced", active.generation)
		}

		return active.gatewayInformerSnapshot, false, nil
	}

	return l.start(ctx)
}

func (l *gatewayInformerLifecycle) start(ctx context.Context) (gatewayInformerSnapshot, bool, error) {
	l.mu.Lock()
	if l.active != nil {
		snapshot := l.active.gatewayInformerSnapshot
		l.mu.Unlock()

		return snapshot, false, nil
	}

	l.starting = true
	l.lastErr = nil
	l.generation++
	generation := l.generation
	onEvent := l.onEvent
	l.mu.Unlock()

	factory := dynamicinformer.NewDynamicSharedInformerFactory(l.client, l.resync)
	pools := factory.ForResource(gatewayPoolGVR).Informer()
	nodes := factory.ForResource(gatewayNodeGVR).Informer()
	poolPeerings := factory.ForResource(gatewayPoolPeeringGVR).Informer()

	cleanups, err := addGatewayInformerEventHandlers(pools, nodes, poolPeerings, onEvent)
	if err != nil {
		l.recordStartFailure(err)

		return gatewayInformerSnapshot{}, false, err
	}

	runCtx, cancel := context.WithCancel(ctx)
	factory.Start(runCtx.Done())

	snapshot := gatewayInformerSnapshot{
		generation:   generation,
		available:    true,
		pools:        pools,
		nodes:        nodes,
		poolPeerings: poolPeerings,
	}
	if !cache.WaitForCacheSync(runCtx.Done(), pools.HasSynced, nodes.HasSynced, poolPeerings.HasSynced) {
		cancel()
		runGatewayInformerCleanups(cleanups)

		err := fmt.Errorf("failed to sync required gateway informer generation %d", generation)
		if ctx.Err() != nil {
			err = ctx.Err()
		}

		l.recordStartFailure(err)

		return gatewayInformerSnapshot{}, false, err
	}

	activeGeneration := &gatewayInformerGeneration{
		gatewayInformerSnapshot: snapshot,
		cancel:                  cancel,
		cleanups:                cleanups,
	}

	l.mu.Lock()
	l.starting = false
	l.lastErr = nil
	l.active = activeGeneration
	l.retiring = !l.required
	l.mu.Unlock()

	return snapshot, false, nil
}

func (l *gatewayInformerLifecycle) recordStartFailure(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.starting = false
	l.lastErr = err
}

func (l *gatewayInformerLifecycle) retire(generation uint64) {
	l.mu.Lock()

	active := l.active
	if active == nil || active.generation != generation || l.required {
		l.mu.Unlock()

		return
	}

	l.active = nil
	l.retiring = false
	l.mu.Unlock()

	runGatewayInformerCleanups(active.cleanups)
	active.cancel()
}

func (l *gatewayInformerLifecycle) close() {
	l.mu.Lock()
	active := l.active
	l.active = nil
	l.required = false
	l.retiring = false
	l.mu.Unlock()

	if active != nil {
		runGatewayInformerCleanups(active.cleanups)
		active.cancel()
	}
}

func gatewayInformerSnapshotSynced(snapshot gatewayInformerSnapshot) bool {
	return snapshot.available &&
		snapshot.pools != nil && snapshot.pools.HasSynced() &&
		snapshot.nodes != nil && snapshot.nodes.HasSynced() &&
		snapshot.poolPeerings != nil && snapshot.poolPeerings.HasSynced()
}

func addGatewayInformerEventHandlers(
	pools, nodes, poolPeerings cache.SharedIndexInformer,
	onEvent func(),
) ([]func(), error) {
	if onEvent == nil {
		onEvent = func() {}
	}

	handler := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(interface{}) { onEvent() },
		UpdateFunc: func(interface{}, interface{}) { onEvent() },
		DeleteFunc: func(interface{}) { onEvent() },
	}

	var cleanups []func()

	add := func(name string, informer cache.SharedIndexInformer, eventHandler cache.ResourceEventHandler) error {
		registration, err := informer.AddEventHandler(eventHandler)
		if err != nil {
			return fmt.Errorf("add %s event handler: %w", name, err)
		}

		cleanups = append(cleanups, func() {
			if err := informer.RemoveEventHandler(registration); err != nil {
				klog.V(4).Infof("Failed to remove %s event handler: %v", name, err)
			}
		})

		return nil
	}

	if err := add("GatewayPool", pools, handler); err != nil {
		runGatewayInformerCleanups(cleanups)

		return nil, err
	}

	gatewayNodeHandler := cache.ResourceEventHandlerFuncs{
		AddFunc: func(interface{}) { onEvent() },
		UpdateFunc: func(oldObj, newObj interface{}) {
			if shouldReconcileGatewayNodeUpdate(oldObj, newObj) {
				onEvent()
			}
		},
		DeleteFunc: func(interface{}) { onEvent() },
	}
	if err := add("GatewayPoolNode", nodes, gatewayNodeHandler); err != nil {
		runGatewayInformerCleanups(cleanups)

		return nil, err
	}

	if err := add("GatewayPoolPeering", poolPeerings, handler); err != nil {
		runGatewayInformerCleanups(cleanups)

		return nil, err
	}

	return cleanups, nil
}

func runGatewayInformerCleanups(cleanups []func()) {
	for i := len(cleanups) - 1; i >= 0; i-- {
		cleanups[i]()
	}
}

func gatewayCachesRequired(adopted nodeAssignment, siteName string, assignmentInformer cache.SharedIndexInformer) bool {
	if adopted.gatewayPool != "" {
		return true
	}

	siteName = strings.TrimSpace(siteName)
	if siteName == "" || assignmentInformer == nil {
		return false
	}

	for _, item := range assignmentInformer.GetStore().List() {
		unstr, ok := item.(*unstructured.Unstructured)
		if !ok {
			continue
		}

		assignment, err := parseSiteGatewayPoolAssignment(unstr)
		if err != nil || !unboundednetv1alpha1.SpecEnabled(assignment.Spec.Enabled) {
			continue
		}

		targetsSite := false

		for _, assignmentSite := range assignment.Spec.Sites {
			if strings.TrimSpace(assignmentSite) == siteName {
				targetsSite = true

				break
			}
		}

		if !targetsSite {
			continue
		}

		for _, poolName := range assignment.Spec.GatewayPools {
			if strings.TrimSpace(poolName) != "" {
				return true
			}
		}
	}

	return false
}
