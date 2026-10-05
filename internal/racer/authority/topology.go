// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package authority

import (
	"context"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/membership"
	"github.com/Azure/unbounded/internal/racer/wire"
	"github.com/Azure/unbounded/internal/racer/workload"
)

type publisher struct {
	settings frozenConfig
	client.Writer
	APIReader    client.Reader
	Config       Config
	Publications *publicationStore
	Trust        *trustStore
}

func (r *publisher) runtimeConfig() Config { return r.settings.get(&r.Config) }
func (r *publisher) suspendInvalidAuthority(err error) {
	if shouldInvalidateTrust(err) {
		r.Publications.Suspend()
		r.Trust.invalidate()
	}
}

type (
	AcceptedMembers = membership.History
	TopologyHints   struct {
		Nodes   corev1.NodeList
		Members membership.History
	}
	TopologyObservation struct {
		Nodes   corev1.NodeList
		Input   membership.Input
		Catalog []wire.CacheDefinition
	}
)

// PublishTopology performs discovery under private admission, then durable CAS and
// installation. Only this successful operation advances publisher history.
func (a *Authority) PublishTopology(ctx context.Context, observe func(context.Context) (TopologyObservation, error)) (TopologyHints, error) {
	r := a.publisher
	cfg := r.runtimeConfig()

	if err := a.gate.Acquire(ctx); err != nil {
		return TopologyHints{}, err
	}
	defer a.gate.Release()

	if err := ctx.Err(); err != nil {
		return TopologyHints{}, err
	}

	cm, previous, err := readVersion(ctx, r.APIReader, cfg)
	if err != nil {
		r.suspendInvalidAuthority(err)
		return TopologyHints{}, err
	}

	if observe == nil {
		return TopologyHints{}, wire.InvalidRequest
	}

	observation, err := observe(ctx)
	if err != nil {
		return TopologyHints{}, err
	}

	catalog := observation.Catalog

	if claim := cm.Annotations[credentialClaim]; claim != "" {
		credentials, err := readBoundCredentials(ctx, r.APIReader, cfg, claim, cm)
		if err != nil {
			r.suspendInvalidAuthority(err)
			return TopologyHints{}, err
		}

		keyed := keyedCaches(credentials.bundle)

		accepted := make([]wire.CacheDefinition, 0, len(catalog))
		for _, cache := range catalog {
			if keyed[cache.ID] {
				accepted = append(accepted, cache)
			}
		}

		catalog = accepted
	} else {
		catalog = nil
	}

	result, err := membership.Reconcile(observation.Input, a.accepted)
	if err != nil {
		return TopologyHints{}, err
	}

	for _, d := range result.Diagnostics {
		ctrl.LoggerFrom(ctx).Info("membership input rejected", "object", d.Object, "field", d.Field, "reason", d.Reason)
	}

	prepared, err := r.Publications.Prepare(previous, cm.ResourceVersion, result.Members, catalog)
	if err != nil {
		return TopologyHints{}, err
	}

	committed, err := r.CommitVersion(ctx, prepared)
	if err != nil {
		return TopologyHints{}, err
	}

	if err := ctx.Err(); err != nil {
		return TopologyHints{}, err
	}

	if err := r.Publications.Install(committed); err != nil {
		return TopologyHints{}, err
	}

	a.accepted = result.Members

	return TopologyHints{Nodes: *observation.Nodes.DeepCopy(), Members: cloneAccepted(result.Members)}, nil
}

func cloneAccepted(members AcceptedMembers) AcceptedMembers {
	copy := make(AcceptedMembers, len(members))
	for id, member := range members {
		member.RDMANICs = slices.Clone(member.RDMANICs)
		for i := range member.RDMANICs {
			if numa := member.RDMANICs[i].NUMANode; numa != nil {
				copy := *numa
				member.RDMANICs[i].NUMANode = &copy
			}
		}

		copy[id] = member
	}

	return copy
}

type (
	DataplaneWorkloadIdentities struct {
		namespace string
		workloads [2]workloadIdentity
	}
	workloadIdentity struct {
		name string
		uid  types.UID
	}
)

func (ids DataplaneWorkloadIdentities) observed() membership.WorkloadIdentities {
	observed := membership.WorkloadIdentities{Namespace: ids.namespace}
	for i, workload := range ids.workloads {
		observed.Workloads[i] = membership.WorkloadIdentity{Name: workload.name, UID: workload.uid}
	}

	return observed
}
func (ids DataplaneWorkloadIdentities) Owns(pod *corev1.Pod) bool { return ids.observed().Owns(pod) }
func managedWorkloadNames(cfg Config) []string                    { return workload.ManagedNames(cfg.DaemonSetName) }
func readManagedWorkloadIdentities(ctx context.Context, reader client.Reader, cfg Config) (DataplaneWorkloadIdentities, error) {
	ids := DataplaneWorkloadIdentities{namespace: cfg.Namespace}
	for i, name := range managedWorkloadNames(cfg) {
		ids.workloads[i].name = name

		var ds appsv1.DaemonSet
		if err := reader.Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: name}, &ds); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}

			return DataplaneWorkloadIdentities{}, err
		}

		if ds.DeletionTimestamp == nil {
			ids.workloads[i].uid = ds.UID
		}
	}

	return ids, nil
}

const (
	enrolledSharesAnnotation   = membership.EnrolledSharesAnnotation
	enrolledRDMANICsAnnotation = membership.EnrolledRDMANICsAnnotation
	admittedMemberAnnotation   = membership.AdmittedMemberAnnotation
)

func BuildCatalog(caches []racerv1.ClusterCache) ([]wire.CacheDefinition, error) {
	return membership.BuildCatalog(caches)
}
