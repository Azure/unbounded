// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/racerobject"
)

const (
	originPrefix        = "racer-origin-"
	originImageName     = "racer-object"
	originContainerName = "origin"
	originAppName       = "racer-origin"
	originCacheLabel    = "racer.unbounded-cloud.io/origin-cache"
	originConfigHash    = "racer.unbounded-cloud.io/origin-config-hash"
	originIdentity      = "racer.unbounded-cloud.io/origin-identity"
	originMaxRequests   = 65536

	workloadIdentityUse      = "azure.workload.identity/use"
	workloadIdentityClientID = "azure.workload.identity/client-id"
	workloadIdentityTenantID = "azure.workload.identity/tenant-id"
)

// originName is the DaemonSet and ServiceAccount name for a cache origin.
// ClusterCache names are DNS subdomains of at most 82 characters, so the
// result is always a valid object name.
func originName(cache string) string {
	return originPrefix + cache
}

// originCacheValue is a label-safe stable selector value for a cache name,
// which may be longer than a label value or contain dots.
func originCacheValue(cache string) string {
	sum := sha256.Sum256([]byte(cache))
	return hex.EncodeToString(sum[:])[:16]
}

func originLabels() map[string]string {
	return map[string]string{"app.kubernetes.io/name": originAppName, "app.kubernetes.io/component": originAppName}
}

// originSpec is the validated, rendered form of a ClusterCache origin.
type originSpec struct {
	identity      string
	args          []string
	env           []corev1.EnvVar
	envFrom       []corev1.EnvFromSource
	saAnnotations map[string]string
	podLabels     map[string]string
}

// renderOrigin validates spec.origin and renders the origin process inputs. It
// re-checks the CRD rules so a cache that bypassed admission fails closed.
func renderOrigin(cache *racerv1.ClusterCache) (originSpec, error) {
	origin := cache.Spec.Origin
	if origin.Azure == nil {
		return originSpec{}, errors.New("spec.origin.azure is required")
	}

	out := originSpec{}
	credential := "anonymous"

	if c := origin.Azure.Credential; c != nil {
		credential = "default"

		switch {
		case (c.Default == nil) == (c.WorkloadIdentity == nil):
			return originSpec{}, errors.New("spec.origin.azure.credential must set exactly one of default or workloadIdentity")
		case c.Default != nil:
			if c.Default.ClientID != "" && c.Default.SecretRef != nil {
				return originSpec{}, errors.New("spec.origin.azure.credential.default clientID and secretRef are mutually exclusive")
			}

			if c.Default.ClientID != "" {
				out.env = []corev1.EnvVar{{Name: "AZURE_CLIENT_ID", Value: c.Default.ClientID}}
			}

			if c.Default.SecretRef != nil {
				if c.Default.SecretRef.Name == "" {
					return originSpec{}, errors.New("spec.origin.azure.credential.default.secretRef.name is required")
				}

				out.envFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: c.Default.SecretRef.Name}}}}
			}
		default:
			if c.WorkloadIdentity.ClientID == "" {
				return originSpec{}, errors.New("spec.origin.azure.credential.workloadIdentity.clientID is required")
			}

			out.saAnnotations = map[string]string{workloadIdentityClientID: c.WorkloadIdentity.ClientID}
			if c.WorkloadIdentity.TenantID != "" {
				out.saAnnotations[workloadIdentityTenantID] = c.WorkloadIdentity.TenantID
			}

			out.podLabels = map[string]string{workloadIdentityUse: "true"}
		}
	}

	if err := racerobject.ValidateAzureAccountURL(origin.Azure.AccountURL, credential); err != nil {
		return originSpec{}, fmt.Errorf("spec.origin.azure.accountURL: %w", err)
	}

	identity, err := racerobject.AzureOriginIdentity(origin.Azure.AccountURL)
	if err != nil {
		return originSpec{}, fmt.Errorf("spec.origin.azure.accountURL: %w", err)
	}

	if len(origin.Buckets) == 0 {
		return originSpec{}, errors.New("spec.origin.buckets must list at least one bucket")
	}

	buckets := make([]string, 0, len(origin.Buckets))
	for _, bucket := range origin.Buckets {
		buckets = append(buckets, string(bucket))
	}

	if err := racerobject.ValidateOriginScope(identity, buckets); err != nil {
		return originSpec{}, fmt.Errorf("spec.origin.buckets: %w", err)
	}

	out.identity = identity
	out.args = []string{"origin", "--cache=" + cache.Name, "--namespace=" + identity, "--provider=azure", "--account-url=" + origin.Azure.AccountURL, "--azure-credential=" + credential}

	for _, bucket := range buckets {
		out.args = append(out.args, "--bucket="+bucket)
	}

	if origin.MetadataTTL != nil {
		if origin.MetadataTTL.Duration < 0 {
			return originSpec{}, errors.New("spec.origin.metadataTTL must not be negative")
		}

		out.args = append(out.args, "--metadata-ttl="+origin.MetadataTTL.Duration.String())
	}

	if origin.RequestTimeout != nil {
		if origin.RequestTimeout.Duration <= 0 {
			return originSpec{}, errors.New("spec.origin.requestTimeout must be positive")
		}

		out.args = append(out.args, "--request-timeout="+origin.RequestTimeout.Duration.String())
	}

	if origin.MaxConcurrentRequests != nil {
		if n := *origin.MaxConcurrentRequests; n < 1 || n > originMaxRequests {
			return originSpec{}, fmt.Errorf("spec.origin.maxConcurrentRequests must be between 1 and %d", originMaxRequests)
		}

		out.args = append(out.args, "--max-concurrent-requests="+strconv.Itoa(int(*origin.MaxConcurrentRequests)))
	}

	return out, nil
}

// hash covers every rendered input that does not otherwise change the pod
// template, so credential edits roll the origin Pods.
func (o originSpec) hash() (string, error) {
	data, err := json.Marshal(struct {
		Args          []string               `json:"args"`
		Env           []corev1.EnvVar        `json:"env"`
		EnvFrom       []corev1.EnvFromSource `json:"envFrom"`
		SAAnnotations map[string]string      `json:"saAnnotations"`
	}{o.args, o.env, o.envFrom, o.saAnnotations})
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:]), nil
}

// originPlacement follows the node-level constraints (node affinity, node
// selector, tolerations) of the live dataplane DaemonSet, including user
// overrides, so origins run wherever the dataplane serving their cache runs.
// found is false while the dataplane DaemonSet does not exist yet: placement
// overrides are applied after planning, so no safe placement is known and
// callers must defer origins rather than guess one.
// Pod affinity and anti-affinity are not copied: they are relative to
// dataplane Pods, and a rule such as hostname anti-affinity against
// racer-dataplane would otherwise exclude origins from every dataplane node.
func originPlacement(ctx context.Context, env *component.Env) (affinity *corev1.Affinity, nodeSelector map[string]string, tolerations []corev1.Toleration, found bool, err error) {
	ds := &appsv1.DaemonSet{}

	err = env.LiveReader().Get(ctx, objectKey(env, dataplaneName), ds)
	if apierrors.IsNotFound(err) {
		return nil, nil, nil, false, nil
	}

	if err != nil {
		return nil, nil, nil, false, err
	}

	pod := ds.Spec.Template.Spec

	if pod.Affinity != nil && pod.Affinity.NodeAffinity != nil {
		affinity = &corev1.Affinity{NodeAffinity: pod.Affinity.NodeAffinity.DeepCopy()}
	}

	return affinity, maps.Clone(pod.NodeSelector), slices.Clone(pod.Tolerations), true, nil
}

func cacheOwnerReference(cache *racerv1.ClusterCache) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: racerv1.GroupVersion.String(), Kind: "ClusterCache", Name: cache.Name, UID: cache.UID, Controller: ptr.To(true)}
}

func originDaemonSet(env *component.Env, cache *racerv1.ClusterCache, spec originSpec, hash string, affinity *corev1.Affinity, nodeSelector map[string]string, tolerations []corev1.Toleration) *appsv1.DaemonSet {
	name := originName(cache.Name)
	selector := map[string]string{"app.kubernetes.io/name": originAppName, originCacheLabel: originCacheValue(cache.Name)}

	podLabels := originLabels()
	podLabels[originCacheLabel] = selector[originCacheLabel]

	for key, value := range spec.podLabels {
		podLabels[key] = value
	}

	socketDir := "/run/racer/" + cache.Name + "/origin"

	return &appsv1.DaemonSet{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"},
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: env.Namespace, Labels: originLabels(),
			OwnerReferences: []metav1.OwnerReference{cacheOwnerReference(cache)},
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			UpdateStrategy: appsv1.DaemonSetUpdateStrategy{
				Type: appsv1.RollingUpdateDaemonSetStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDaemonSet{
					MaxUnavailable: ptr.To(intstr.FromString("100%")),
					MaxSurge:       ptr.To(intstr.FromInt32(0)),
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      podLabels,
					Annotations: map[string]string{originConfigHash: hash, originIdentity: spec.identity},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName:            name,
					AutomountServiceAccountToken:  ptr.To(false),
					RestartPolicy:                 corev1.RestartPolicyAlways,
					DNSPolicy:                     corev1.DNSClusterFirst,
					SchedulerName:                 corev1.DefaultSchedulerName,
					EnableServiceLinks:            ptr.To(false),
					PreemptionPolicy:              ptr.To(corev1.PreemptLowerPriority),
					TerminationGracePeriodSeconds: ptr.To(int64(5)),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsUser: ptr.To(int64(0)), RunAsGroup: ptr.To(int64(0)), RunAsNonRoot: ptr.To(false),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Affinity:     affinity,
					NodeSelector: nodeSelector,
					Tolerations:  tolerations,
					Containers: []corev1.Container{{
						Name:                     originContainerName,
						Image:                    env.Config.Image(originImageName),
						ImagePullPolicy:          corev1.PullIfNotPresent,
						TerminationMessagePath:   corev1.TerminationMessagePathDefault,
						TerminationMessagePolicy: corev1.TerminationMessageReadFile,
						Args:                     spec.args,
						Env:                      spec.env,
						EnvFrom:                  spec.envFrom,
						Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("50m"),
							corev1.ResourceMemory: resource.MustParse("64Mi"),
						}},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true),
							Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						VolumeMounts: []corev1.VolumeMount{{Name: originContainerName, MountPath: socketDir}},
					}},
					Volumes: []corev1.Volume{{
						Name:         originContainerName,
						VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: socketDir, Type: ptr.To(corev1.HostPathDirectoryOrCreate)}},
					}},
				},
			},
		},
	}
}

// planOrigin adds one cache's ServiceAccount and DaemonSet, or neither.
func planOrigin(ctx context.Context, env *component.Env, plan *component.Plan, marker *corev1.ConfigMap, cache *racerv1.ClusterCache, spec originSpec, affinity *corev1.Affinity, nodeSelector map[string]string, tolerations []corev1.Toleration) error {
	hash, err := spec.hash()
	if err != nil {
		return err
	}

	domain := "racer-origin/" + cache.Name
	account := &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{
			Name: originName(cache.Name), Namespace: env.Namespace, Labels: originLabels(), Annotations: maps.Clone(spec.saAnnotations),
			OwnerReferences: []metav1.OwnerReference{cacheOwnerReference(cache)},
		},
		AutomountServiceAccountToken: ptr.To(false),
	}

	accountOp, err := ownedRuntimeOperation(ctx, env, component.Operation{Kind: component.OpApply, Object: component.ToUnstructured(account), Component: name, FailureDomain: domain}, marker.UID)
	if err != nil {
		return err
	}

	ds := originDaemonSet(env, cache, spec, hash, affinity, nodeSelector, tolerations)

	dsOp, err := ownedRuntimeOperation(ctx, env, component.Operation{
		Kind: component.OpApply, Object: component.ToUnstructured(ds), Component: name, FailureDomain: domain,
		DependsOn: []component.ObjectRef{accountOp.Ref()},
	}, marker.UID)
	if err != nil {
		return err
	}

	plan.Add(accountOp)
	plan.Add(dsOp)

	return nil
}

// planOrigins deploys one racer-object origin DaemonSet per ClusterCache with
// spec.origin and prunes origins of caches that no longer request one. Invalid
// or conflicting caches are skipped, keep their existing objects, and are
// returned for status; they never block other caches or the dataplane.
// waiting reports that some cache wants an origin but the dataplane DaemonSet
// does not exist yet; no origin is applied until its placement is known, while
// pruning still proceeds and existing origins of wanted caches are kept.
func planOrigins(ctx context.Context, env *component.Env, plan *component.Plan, marker *corev1.ConfigMap) (invalid []string, waiting bool, err error) {
	caches := &racerv1.ClusterCacheList{}
	if err := env.LiveReader().List(ctx, caches); err != nil {
		return nil, false, err
	}

	slices.SortFunc(caches.Items, func(a, b racerv1.ClusterCache) int { return strings.Compare(a.Name, b.Name) })

	wanted := map[string]bool{}

	var (
		placement bool
		found     bool
		affinity  *corev1.Affinity
		selector  map[string]string
		tolerate  []corev1.Toleration
	)

	for i := range caches.Items {
		cache := &caches.Items[i]
		// A terminating cache keeps no origin, even while finalizers hold it.
		if cache.Spec.Origin == nil || cache.DeletionTimestamp != nil {
			continue
		}

		wanted[originName(cache.Name)] = true

		spec, err := renderOrigin(cache)
		if err != nil {
			invalid = append(invalid, fmt.Sprintf("%s (%v)", cache.Name, err))
			continue
		}

		if !placement {
			affinity, selector, tolerate, found, err = originPlacement(ctx, env)
			if err != nil {
				return nil, false, err
			}

			placement = true
		}

		if !found {
			waiting = true
			continue
		}

		err = planOrigin(ctx, env, plan, marker, cache, spec, affinity.DeepCopy(), maps.Clone(selector), slices.Clone(tolerate))
		if errors.Is(err, errNotOwned) {
			invalid = append(invalid, fmt.Sprintf("%s (%v)", cache.Name, err))
			continue
		}

		if err != nil {
			return nil, false, err
		}
	}

	if err := pruneOrigins(ctx, env, plan, marker, wanted); err != nil {
		return nil, false, err
	}

	return invalid, waiting, nil
}

func pruneOrigins(ctx context.Context, env *component.Env, plan *component.Plan, marker *corev1.ConfigMap, wanted map[string]bool) error {
	opts := []client.ListOption{client.InNamespace(env.Namespace), client.MatchingLabels{"app.kubernetes.io/component": originAppName}}

	daemonSets := &appsv1.DaemonSetList{}
	if err := env.LiveReader().List(ctx, daemonSets, opts...); err != nil {
		return err
	}

	accounts := &corev1.ServiceAccountList{}
	if err := env.LiveReader().List(ctx, accounts, opts...); err != nil {
		return err
	}

	var stale []client.Object

	for i := range daemonSets.Items {
		ds := &daemonSets.Items[i]
		ds.TypeMeta = metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"}
		stale = append(stale, ds)
	}

	for i := range accounts.Items {
		sa := &accounts.Items[i]
		sa.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"}
		stale = append(stale, sa)
	}

	for _, obj := range stale {
		if !strings.HasPrefix(obj.GetName(), originPrefix) || wanted[obj.GetName()] || validateRuntimeOwner(obj, marker.UID) != nil {
			continue
		}

		op := component.DeleteOperation(obj, name, "")
		op.FailureDomain = "racer-origin/" + strings.TrimPrefix(obj.GetName(), originPrefix)
		plan.Add(op)
	}

	return nil
}
