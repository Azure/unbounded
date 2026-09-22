// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	eventhandler "sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	machina "github.com/Azure/unbounded/api/machina/v1alpha3"
	pb "github.com/Azure/unbounded/api/racer"
	"github.com/Azure/unbounded/internal/racer"
)

// One CAS-updated record per Node UID. Never share topology's commit point or GC
// owner label: deleting history could reuse a version while a process is live.
type storagePolicyRecord struct {
	Node            string `json:"node"`
	Universe        string `json:"universe"`
	Identity        string `json:"identity"`
	Version         uint64 `json:"version"`
	DesiredBytes    int64  `json:"desiredBytes"`
	ValidationError string `json:"validationError,omitempty"`
}

// Reports are observations, not durable decisions. Restart requires fresh
// delivery and feedback. Preserve last applied bytes across a pending/failed
// version, but never across a Pod/process identity change.
type storageReport struct {
	PodUID, Boot    string
	Supported       bool
	OfferedIdentity string
	OfferedVersion  uint64
	Version         uint64
	State           string
	AppliedBytes    uint64
	AppliedVersion  uint64
	Shards          uint64
	Error           string
	Seen            time.Time
}

type storageReconciler struct {
	client    client.Client // uncached reads and resourceVersion CAS
	inventory client.Client // indexed Node watch mapping
	store     stateStore
	server    *Server
}

func setupStorageController(manager ctrl.Manager, server *Server) error {
	r := &storageReconciler{client: server.controlStore.client, inventory: manager.GetClient(), store: server.controlStore, server: server}
	filter := predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		return storageNodeChanged(e.ObjectOld, e.ObjectNew)
	}}

	return ctrl.NewControllerManagedBy(manager).Named("racer-storage-policy").
		For(&corev1.Node{}, builder.WithPredicates(filter)).
		Watches(&machina.Site{}, eventhandler.EnqueueRequestsFromMapFunc(r.siteRequests)).Complete(r)
}

func storageNodeChanged(old, next client.Object) bool {
	a, aOK := old.(*corev1.Node)

	b, bOK := next.(*corev1.Node)
	if !aOK || !bOK {
		return false
	}

	av, ap := a.Annotations[racer.CacheSizeAnnotationKey]
	bv, bp := b.Annotations[racer.CacheSizeAnnotationKey]

	return a.UID != b.UID || racer.NodeSite(a) != racer.NodeSite(b) || av != bv || ap != bp
}

func (r *storageReconciler) siteRequests(ctx context.Context, o client.Object) []reconcile.Request {
	var nodes corev1.NodeList
	if err := r.inventory.List(ctx, &nodes, client.MatchingFields{universeIndex: racer.UniverseForSite(o.GetName())}); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "map storage Site change")
		return nil // Periodic Node reconciliation also repairs missed mappings.
	}

	var requests []reconcile.Request

	for _, node := range nodes.Items {
		if racer.NodeSite(&node) == o.GetName() {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: node.Name}})
		}
	}

	return requests
}

func (r *storageReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	var node corev1.Node
	if err := r.client.Get(ctx, request.NamespacedName, &node); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if node.UID == "" {
		return ctrl.Result{}, fmt.Errorf("storage Node has no UID")
	}

	nodeID := identity("node", string(node.UID))

	var site *machina.Site
	if name := racer.NodeSite(&node); name != "" {
		site = &machina.Site{}
		if err := r.client.Get(ctx, types.NamespacedName{Name: name}, site); err != nil {
			if !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}

			site = nil
		}
	}

	desired, validation := racer.ResolveCacheSize(&node, site)
	key := types.NamespacedName{Namespace: r.store.namespace, Name: "racer-storage-" + nodeID}
	cm := &corev1.ConfigMap{}
	err := r.store.client.Get(ctx, key, cm)

	create := apierrors.IsNotFound(err)
	if err != nil && !create {
		return ctrl.Result{}, err
	}

	record := storagePolicyRecord{Node: nodeID}

	if !create {
		if cm.Labels[stateLabel] != "storage" {
			return ctrl.Result{}, fmt.Errorf("storage state name collision")
		}

		if err := json.Unmarshal([]byte(cm.Data["policy"]), &record); err != nil {
			return ctrl.Result{}, err
		}

		id, err := hex.DecodeString(record.Identity)
		if err != nil || len(id) != 32 || record.Node != nodeID || (record.Version == 0) != (record.DesiredBytes == 0) || (record.Version > 0 && !validStorageBytes(record.DesiredBytes)) {
			return ctrl.Result{}, fmt.Errorf("invalid durable storage policy")
		}
	} else {
		var id [32]byte
		if _, err := rand.Read(id[:]); err != nil {
			return ctrl.Result{}, err
		}

		record.Identity = hex.EncodeToString(id[:])
		cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name, Labels: map[string]string{stateLabel: "storage"}}}
	}

	previous := record
	record.Universe = racer.NodeUniverse(&node)

	record.ValidationError = ""
	if validation != nil {
		record.ValidationError = storageText(validation.Error(), 1024)
	} else if record.DesiredBytes != desired {
		if record.Version == ^uint64(0) {
			return ctrl.Result{}, fmt.Errorf("storage policy version exhausted")
		}

		record.Version++
		record.DesiredBytes = desired
	}

	if create || record != previous {
		raw, err := json.Marshal(record)
		if err != nil {
			return ctrl.Result{}, err
		}

		cm.Data = map[string]string{"policy": string(raw)}
		if create {
			err = r.store.client.Create(ctx, cm)
		} else {
			err = r.store.client.Update(ctx, cm)
		}
		// On ambiguous writes/conflicts, reload next reconcile; never publish intent
		// before its durable decision. The prior published policy stays usable.
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	r.server.mu.Lock()
	if r.server.storagePolicies == nil {
		r.server.storagePolicies = map[string]storagePolicyRecord{}
	}

	r.server.storagePolicies[nodeID] = record
	r.server.mu.Unlock()

	if err := r.publishStorageStatus(ctx, &node, site, record, time.Now()); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func validStorageBytes(bytes int64) bool {
	return bytes >= racer.MinCacheSizeBytes && bytes <= racer.MaxCacheSizeBytes && bytes%racer.CacheSizeAlignment == 0
}

// Called only after current Node/Pod authorization and boot-collision checks,
// under Server.mu. Storage feedback cannot fail or advance a topology phase.
func (s *Server) storageCommand(req *http.Request, key recipient, podUID string) *pb.StoragePolicy {
	record, ok := s.storagePolicies[hex.EncodeToString(key.node[:])]
	if !ok || record.Universe == "" || identity("universe", record.Universe) != hex.EncodeToString(key.universe[:]) {
		return nil
	}

	if s.storageReports == nil {
		s.storageReports = map[recipient]storageReport{}
	}

	report := s.storageReports[key]

	boot := req.Header.Get("X-Racer-Boot")
	if report.PodUID != podUID || report.Boot != boot {
		report = storageReport{PodUID: podUID, Boot: boot}
	}

	report.Supported = req.Header.Get("X-Racer-Storage-Policy") == "1"
	if !report.Supported {
		report = storageReport{PodUID: podUID, Boot: boot, State: "unsupported", Seen: time.Now()}
		s.storageReports[key] = report

		return nil
	}

	version, ve := strconv.ParseUint(req.Header.Get("X-Racer-Storage-Version"), 10, 64)
	applied, ae := strconv.ParseUint(req.Header.Get("X-Racer-Storage-Applied-Bytes"), 10, 64)

	state := req.Header.Get("X-Racer-Storage-State")
	if ve == nil && ae == nil && version != 0 && version == record.Version && version == report.OfferedVersion &&
		req.Header.Get("X-Racer-Storage-Identity") == record.Identity && report.OfferedIdentity == record.Identity &&
		(state == "pending" || state == "failed" || state == "applied") && (applied == 0 || (applied <= uint64(racer.MaxCacheSizeBytes) && validStorageBytes(int64(applied)))) &&
		(state != "applied" || applied == uint64(record.DesiredBytes)) {
		if report.AppliedBytes != applied {
			report.AppliedVersion, report.Shards = 0, 0
		}

		report.Version, report.State, report.AppliedBytes = version, state, applied
		report.Seen = time.Now()
		report.Error = ""

		if state == "failed" {
			// Hex keeps diagnostics bounded and safe in HTTP headers, including
			// filesystem paths and errors containing newlines or non-ASCII text.
			raw := req.Header.Get("X-Racer-Storage-Error")
			if len(raw) <= 2048 {
				if decoded, err := hex.DecodeString(raw); err == nil {
					report.Error = storageText(string(decoded), 1024)
				}
			}
		}

		if shards, err := strconv.ParseUint(req.Header.Get("X-Racer-Storage-Shards"), 10, 32); err == nil && shards <= applied/uint64(racer.MinCacheSizeBytes) {
			report.Shards = shards
		}

		if state == "applied" {
			report.AppliedVersion = version
		}
	}

	if record.Version == 0 {
		report.State, report.Seen = "pending", time.Now()
		s.storageReports[key] = report

		return nil
	}

	if report.OfferedVersion != record.Version || report.OfferedIdentity != record.Identity {
		report.State = "pending"

		report.Error = ""
		if report.Seen.IsZero() {
			report.Seen = time.Now()
		}
	}

	report.OfferedIdentity, report.OfferedVersion = record.Identity, record.Version
	s.storageReports[key] = report

	id, err := hex.DecodeString(record.Identity)
	if err != nil || len(id) != 32 {
		return nil
	}

	return &pb.StoragePolicy{Identity: id, Version: record.Version, DesiredBytes: uint64(record.DesiredBytes)}
}
