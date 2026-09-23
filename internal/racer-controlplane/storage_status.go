// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	machina "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/racer"
)

const storageFreshness = 15 * time.Second

func storageText(value string, limit int) string {
	value = strings.ToValidUTF8(value, "?")
	if len(value) > limit {
		value = value[:limit]
		for !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
	}

	return value
}

// Output only. Policy decisions are persisted separately; neither acknowledgments
// nor freshness are restored from this annotation after a controller restart.
type cacheStatus struct {
	Source          string     `json:"source"`
	Requested       string     `json:"requested"`
	RequestedBytes  *int64     `json:"requestedBytes"`
	EffectiveBytes  int64      `json:"effectiveBytes"`
	PolicyIdentity  string     `json:"policyIdentity"`
	PolicyVersion   uint64     `json:"policyVersion"`
	Phase           string     `json:"phase"`
	ValidationError string     `json:"validationError,omitempty"`
	PolicyPhase     string     `json:"policyPhase"`
	Error           string     `json:"error,omitempty"`
	AppliedBytes    uint64     `json:"appliedBytes"`
	AppliedVersion  uint64     `json:"appliedVersion"`
	Shards          uint64     `json:"shards"`
	SelectedPodUID  string     `json:"selectedPodUID"`
	Boot            string     `json:"boot"`
	Fresh           bool       `json:"fresh"`
	LastSeen        *time.Time `json:"lastSeen,omitempty"`
	UpdatedAt       time.Time  `json:"updatedAt"`
}

func storageInput(node *corev1.Node, site *machina.Site) (string, string) {
	if value, present := node.Annotations[racer.CacheSizeAnnotationKey]; present {
		return "node", value
	}

	if site != nil && site.Spec.Components.Racer != nil && site.Spec.Components.Racer.CacheSize != nil {
		return "site", site.Spec.Components.Racer.CacheSize.String()
	}

	return "default", "10Gi"
}

func (s *Server) cacheStatus(node *corev1.Node, site *machina.Site, record storagePolicyRecord, now time.Time) cacheStatus {
	source, requested := storageInput(node, site)

	status := cacheStatus{
		Source: source, Requested: storageText(requested, 256), EffectiveBytes: record.DesiredBytes,
		PolicyIdentity: record.Identity, PolicyVersion: record.Version, PolicyPhase: "pending", UpdatedAt: now,
	}
	if _, err := racer.ResolveCacheSize(node, site); err != nil {
		status.ValidationError = record.ValidationError
	} else if quantity, err := resource.ParseQuantity(requested); err == nil {
		bytes := quantity.Value()
		status.RequestedBytes = &bytes
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	key := recipient{identityBytes("universe", record.Universe), identityBytes("node", string(node.UID))}
	if s.source != nil {
		if topology := s.source.topologies[key.universe]; topology != nil {
			if name, ok := topology.byID[record.Node]; ok {
				status.SelectedPodUID = topology.g.Nodes[name].PodUID
			}
		}
	}

	if report, ok := s.storageReports[key]; ok && status.SelectedPodUID != "" && report.PodUID == status.SelectedPodUID {
		status.Boot = report.Boot
		if !report.Seen.IsZero() {
			seen := report.Seen.UTC()
			status.LastSeen = &seen
			status.Fresh = now.Sub(seen) < storageFreshness
		}

		if !status.Fresh {
			status.PolicyPhase = "stale"
		} else if !report.Supported {
			status.PolicyPhase = "unsupported"
		} else {
			status.AppliedBytes, status.AppliedVersion, status.Shards = report.AppliedBytes, report.AppliedVersion, report.Shards
			if report.OfferedIdentity == record.Identity && report.Version == record.Version && record.Version != 0 {
				status.PolicyPhase, status.Error = report.State, report.Error
			}
		}
	}

	status.Phase = status.PolicyPhase
	if status.ValidationError != "" {
		status.Phase = "invalid"
	}

	return status
}

func (r *storageReconciler) publishStorageStatus(ctx context.Context, node *corev1.Node, site *machina.Site, record storagePolicyRecord, now time.Time) error {
	status := r.server.cacheStatus(node, site, record, now)

	var old cacheStatus
	if json.Unmarshal([]byte(node.Annotations[racer.CacheStatusAnnotationKey]), &old) == nil {
		// Compare the complete semantic state, ignoring only the two timestamps.
		// Fresh heartbeats refresh the annotation at most once per minute. State,
		// selected Pod/boot, source and stale transitions publish on the next poll.
		previousSeen, previousUpdated := old.LastSeen, old.UpdatedAt

		old.LastSeen, old.UpdatedAt = status.LastSeen, status.UpdatedAt
		if reflect.DeepEqual(old, status) && (previousSeen == nil || status.LastSeen == nil || !status.LastSeen.After(*previousSeen) || now.Sub(previousUpdated) < time.Minute) {
			return nil
		}
	}

	raw, err := json.Marshal(status)
	if err != nil {
		return err
	}

	before := node.DeepCopy()
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}

	node.Annotations[racer.CacheStatusAnnotationKey] = string(raw)
	// Optimistic locking prevents a delete/recreate or concurrent input edit from
	// receiving a status computed for the old Node object.
	return r.client.Patch(ctx, node, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}
