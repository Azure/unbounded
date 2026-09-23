// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	eventhandler "sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	machina "github.com/Azure/unbounded/api/machina/v1alpha3"
	racerapi "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer"
)

func (r *reconciler) cacheRequests(ctx context.Context, _ client.Object) []reconcile.Request {
	// Reconcile all Sites on a cache edit, including withdrawals. Cached lists
	// avoid a second selector index and retain old selections on update/delete.
	var sites machina.SiteList
	if err := r.client.List(ctx, &sites); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "list cache Sites")
		return nil
	}

	var names []string
	for _, site := range sites.Items {
		names = append(names, racer.UniverseForSite(site.Name))
	}

	return universeRequests(names...)
}

func (r *reconciler) selectedCaches(ctx context.Context, universe string) ([]racerapi.P2PCache, error) {
	var sites machina.SiteList
	if err := r.client.List(ctx, &sites); err != nil {
		return nil, err
	}

	var caches racerapi.P2PCacheList
	if err := r.client.List(ctx, &caches); err != nil {
		return nil, err
	}

	var selected []racerapi.P2PCache

	for _, site := range sites.Items {
		if racer.UniverseForSite(site.Name) != universe {
			continue
		}

		for _, cache := range caches.Items {
			match, err := racer.CacheSelectsSite(&cache, &site)
			if err != nil {
				return nil, err
			}

			if match {
				selected = append(selected, cache)
			}
		}
	}

	return selected, nil
}

type cacheStatusReconciler struct {
	client     client.Client
	server     *Server
	socketRoot string
}

func setupCacheStatusController(manager ctrl.Manager, server *Server, socketRoot string) error {
	r := &cacheStatusReconciler{client: manager.GetClient(), server: server, socketRoot: socketRoot}
	mapCaches := eventhandler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		var caches racerapi.P2PCacheList
		if err := r.client.List(ctx, &caches); err != nil {
			ctrl.LoggerFrom(ctx).Error(err, "list P2PCaches for status")
			return nil
		}

		requests := make([]reconcile.Request, 0, len(caches.Items))
		for _, cache := range caches.Items {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: cache.Name}})
		}

		return requests
	})

	return ctrl.NewControllerManagedBy(manager).Named("p2pcache-status").
		For(&racerapi.P2PCache{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&machina.Site{}, mapCaches).
		Watches(&corev1.Node{}, mapCaches).
		Watches(&corev1.Pod{}, mapCaches).
		Complete(r)
}

func (r *cacheStatusReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	var cache racerapi.P2PCache
	if err := r.client.Get(ctx, request.NamespacedName, &cache); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var sites machina.SiteList
	if err := r.client.List(ctx, &sites); err != nil {
		return ctrl.Result{}, err
	}

	var nodes corev1.NodeList
	if err := r.client.List(ctx, &nodes); err != nil {
		return ctrl.Result{}, err
	}

	var pods corev1.PodList
	if err := r.client.List(ctx, &pods, client.MatchingLabels{dataplaneLabel: "true"}); err != nil {
		return ctrl.Result{}, err
	}

	before := cache.DeepCopy()

	// Use the durable client rather than the informer cache: every committed
	// universe must be accounted for, including deleted Sites and universes
	// not yet restored after restart or an ambiguous commit.
	store := r.server.controlStore
	if store.client == nil {
		store.client = r.client
	}

	var pointers corev1.ConfigMapList
	if err := store.client.List(ctx, &pointers, client.InNamespace(store.namespace), client.MatchingLabels{stateLabel: "commit"}); err != nil {
		return ctrl.Result{}, err
	}

	cache.Status = r.status(&cache, sites.Items, nodes.Items, pods.Items, time.Now(), pointers.Items...)
	if !reflect.DeepEqual(before.Status, cache.Status) {
		if err := r.client.Status().Patch(ctx, &cache, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
}

func (r *cacheStatusReconciler) status(cache *racerapi.P2PCache, sites []machina.Site, nodes []corev1.Node, pods []corev1.Pod, now time.Time, pointers ...corev1.ConfigMap) racerapi.P2PCacheStatus {
	status := *cache.Status.DeepCopy()
	status.ObservedGeneration = cache.Generation
	status.Participants = racerapi.P2PCacheParticipants{}
	condition := func(kind string, value bool, reason, message string) {
		state := metav1.ConditionFalse
		if value {
			state = metav1.ConditionTrue
		}

		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: kind, Status: state, Reason: reason, Message: message, ObservedGeneration: cache.Generation})
	}
	selected := map[string]bool{}

	if _, err := metav1.LabelSelectorAsSelector(&cache.Spec.SiteSelector); err != nil {
		condition(racerapi.ConditionAccepted, false, "InvalidSelector", err.Error())
		condition(racerapi.ConditionReady, false, "InvalidSelector", err.Error())

		return status
	}

	root := r.socketRoot
	if root == "" {
		root = racer.SocketRoot
	}

	if _, _, err := racer.CacheSockets(root, cache.Name); err != nil {
		condition(racerapi.ConditionAccepted, false, "InvalidSocketPath", err.Error())
		condition(racerapi.ConditionReady, false, "InvalidSocketPath", err.Error())

		return status
	}

	if cache.Spec.CacheGeneration < 0 || cache.Spec.MaxCandidateAttempts < 1 || cache.Spec.MaxCandidateAttempts > 8 {
		condition(racerapi.ConditionAccepted, false, "InvalidConfiguration", "Cache generation and candidate attempts are outside their allowed ranges")
		condition(racerapi.ConditionReady, false, "InvalidConfiguration", "Cache configuration is invalid")

		return status
	}

	for _, site := range sites {
		match, err := racer.CacheSelectsSite(cache, &site)
		if err != nil {
			condition(racerapi.ConditionAccepted, false, "InvalidSelector", err.Error())
			condition(racerapi.ConditionReady, false, "InvalidSelector", err.Error())

			return status
		}

		if match {
			selected[racer.UniverseForSite(site.Name)] = true
		}
	}

	condition(racerapi.ConditionAccepted, true, "Accepted", "P2PCache configuration is valid")

	livePods := map[string]bool{}
	for _, pod := range pods {
		livePods[string(pod.UID)] = podAvailable(&pod) && podReady(&pod)
	}

	r.server.mu.Lock()
	defer r.server.mu.Unlock()

	converged := true

	for _, pointer := range pointers {
		var m manifest
		if json.Unmarshal([]byte(pointer.Data["manifest"]), &m) != nil || m.Universe == "" || pointer.Name != stateName(m.Universe) || r.server.source == nil {
			converged = false
			continue
		}

		t := r.server.source.topologies[identityBytes("universe", m.Universe)]

		roll := r.server.rollouts[m.Universe]
		if t == nil || roll == nil || roll.invalid || roll.revision != t.g.Revision {
			converged = false
			continue
		}

		digest, err := t.publishedDigest()
		if err != nil || digest != m.Digest {
			converged = false
		}
	}

	for _, node := range nodes {
		universe := racer.NodeUniverse(&node)
		if !selected[universe] || !racer.NodeEligible(&node) || node.DeletionTimestamp != nil || node.Labels[corev1.LabelOSStable] != "linux" {
			continue
		}

		status.Participants.Desired++

		if !nodeReady(&node) || r.server.source == nil {
			continue
		}

		t := r.server.source.topologies[identityBytes("universe", universe)]
		if t == nil || !generationHasCache(t.g, cache) {
			continue
		}

		participant := t.g.Nodes[node.Name]

		roll := r.server.rollouts[universe]
		if participant.ID != identity("node", string(node.UID)) || participant.IP == "" || !livePods[participant.PodUID] || roll == nil || roll.invalid || roll.revision != t.g.Revision || roll.phase != 4 {
			continue
		}

		ack := roll.acks[participant.ID]
		if ack.boot != "" && ack.phase == 4 && ack.healthy && !ack.seen.After(now) && now.Sub(ack.seen) < storageFreshness {
			status.Participants.Ready++
		}
	}
	// A selector withdrawal is not converged while its old cache is still
	// published or while the replacement retains unresolved retirement duties.
	if r.server.source != nil {
		for _, t := range r.server.source.topologies {
			if selected[t.g.Universe] {
				continue
			}

			relevant := t.g.Withdrawn[string(cache.UID)]
			for _, volume := range t.g.volumes() {
				if volume.Volume.Name == cache.Name {
					converged = false
					relevant = true
				}
			}

			if !relevant {
				continue
			}

			roll := r.server.rollouts[t.g.Universe]
			if roll == nil || roll.invalid || roll.revision != t.g.Revision || roll.phase != 4 {
				converged = false
				continue
			}

			for _, member := range t.g.Nodes {
				if member.IP != "" && roll.acks[member.ID].phase != 4 {
					converged = false
				}
			}

			if roll.pointer != nil {
				removals, err := removalHistory(roll.pointer.Data["removals"], t.g.Universe, roll.revision)
				if err != nil {
					converged = false
				}

				for _, removal := range removals {
					if !removal.Done {
						converged = false
					}
				}

				forwards, err := roll.forwardHistory(t.g.Universe)
				if err != nil {
					converged = false
				}

				for _, forward := range forwards {
					// Wildcards remain durable recovery evidence after a boot has
					// retired. Only a fresh acknowledgment of the current revision
					// proves that retained participant has completed withdrawal.
					ack := roll.acks[forward.snapshotRef().Node]
					if forward.Boot != "" || ack.boot == "" || ack.phase != 4 || ack.seen.After(now) || now.Sub(ack.seen) >= storageFreshness {
						converged = false
					}
				}
			}
		}
	}

	reason, message := "Progressing", fmt.Sprintf("%d of %d desired participants have activated", status.Participants.Ready, status.Participants.Desired)

	ready := converged && status.Participants.Desired > 0 && status.Participants.Ready == status.Participants.Desired
	if ready {
		reason = "Ready"
	} else if len(selected) == 0 {
		reason, message = "NoMatchingSites", "No Racer-enabled Sites match the selector"
	} else if status.Participants.Desired == 0 {
		reason, message = "NoParticipants", "Selected Sites have no eligible participants"
	} else if !converged {
		reason, message = "Retiring", "Waiting for withdrawn Sites to retire prior configurations"
	}

	condition(racerapi.ConditionReady, ready, reason, message)

	return status
}

func generationHasCache(g *generation, cache *racerapi.P2PCache) bool {
	for _, volume := range g.volumes() {
		v := volume.Volume
		if v.ID == string(cache.UID) && v.ResourceGeneration == cache.Generation && v.Cache == uint64(cache.Spec.CacheGeneration) && v.Attempts == uint32(cache.Spec.MaxCandidateAttempts) {
			return true
		}
	}

	return false
}
