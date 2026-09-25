// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	"github.com/Azure/unbounded/internal/racer/wire"
)

type WorkloadReconciler struct {
	client.Client
	APIReader client.Reader
	Config    Config
}

func (r *WorkloadReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	desired, err := r.DesiredDaemonSet()
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := ctx.Err(); err != nil {
		return ctrl.Result{}, err
	}
	// Use authoritative reads on every retry, never refresh an old leader's write.
	current := &appsv1.DaemonSet{}

	err = r.APIReader.Get(ctx, client.ObjectKeyFromObject(desired), current)
	if ctx.Err() != nil {
		return ctrl.Result{}, ctx.Err()
	}

	if apierrors.IsNotFound(err) {
		if err := ctx.Err(); err != nil {
			return ctrl.Result{}, err
		}

		err = r.Create(ctx, desired)
	} else if err == nil {
		if current.Labels["app.kubernetes.io/managed-by"] != "racer-controller" || !apiequality.Semantic.DeepEqual(current.Spec.Selector, desired.Spec.Selector) {
			return ctrl.Result{}, fmt.Errorf("unmanaged or incompatible DaemonSet: %w", wire.Conflict)
		}

		if !current.DeletionTimestamp.IsZero() {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}

		// Own the pod spec, including absence of extra mounts, tokens, or privileges.
		// Set API defaults explicitly in desired state so equality is stable after
		// admission. Preserve unrelated object metadata and rollout annotations.
		before := current.DeepCopy()
		desired.Spec.Template.Annotations = current.Spec.Template.Annotations
		current.Spec.Template = desired.Spec.Template

		current.Spec.UpdateStrategy = desired.Spec.UpdateStrategy
		if apiequality.Semantic.DeepEqual(before.Spec, current.Spec) {
			return ctrl.Result{}, nil
		}

		if err := ctx.Err(); err != nil {
			return ctrl.Result{}, err
		}

		err = r.Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	}

	if ctx.Err() != nil {
		return ctrl.Result{}, ctx.Err()
	}

	if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) {
		return ctrl.Result{RequeueAfter: retryConflictDelay}, nil
	}

	return ctrl.Result{}, err
}

// DesiredDaemonSet declares the token audience, common keyring/trust projection,
// node-private identity and slab storage, socket mounts, and exclusion affinity.
// It must never introduce a per-node Secret or trust a node-name as a Node UID.
func (r *WorkloadReconciler) DesiredDaemonSet() (*appsv1.DaemonSet, error) {
	c := r.Config
	if err := c.Validate(); err != nil {
		return nil, err
	}

	u, err := url.Parse(c.ControlURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") || strings.TrimSpace(c.DataplaneImage) == "" {
		return nil, fmt.Errorf("workload endpoint or image: %w", wire.InvalidRequest)
	}

	if u.Port() != "" {
		port, err := strconv.ParseUint(u.Port(), 10, 16)
		if err != nil || port == 0 {
			return nil, fmt.Errorf("workload endpoint port: %w", wire.InvalidRequest)
		}
	}

	labels := map[string]string{"app.kubernetes.io/name": "racer-dataplane", "app.kubernetes.io/managed-by": "racer-controller"}

	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: c.DaemonSetName, Namespace: c.Namespace, Labels: labels}, Spec: appsv1.DaemonSetSpec{
		Selector:       &metav1.LabelSelector{MatchLabels: labels},
		UpdateStrategy: appsv1.DaemonSetUpdateStrategy{Type: appsv1.RollingUpdateDaemonSetStrategyType, RollingUpdate: &appsv1.RollingUpdateDaemonSet{MaxUnavailable: ptr.To(intstr.FromInt32(1)), MaxSurge: ptr.To(intstr.FromInt32(0))}},
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{
			ServiceAccountName: c.DataplaneServiceAccount, AutomountServiceAccountToken: ptr.To(false),
			RestartPolicy: corev1.RestartPolicyAlways, DNSPolicy: corev1.DNSClusterFirst, SchedulerName: corev1.DefaultSchedulerName,
			EnableServiceLinks: ptr.To(false), PreemptionPolicy: ptr.To(corev1.PreemptLowerPriority),
			TerminationGracePeriodSeconds: ptr.To(int64(30)),
			SecurityContext:               &corev1.PodSecurityContext{RunAsUser: ptr.To(int64(0))},
			Affinity:                      &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: wire.ExclusionLabel, Operator: corev1.NodeSelectorOpDoesNotExist}, {Key: "kubernetes.io/os", Operator: corev1.NodeSelectorOpIn, Values: []string{"linux"}}}}}}}},
			Containers: []corev1.Container{{
				Name: "dataplane", Image: c.DataplaneImage, ImagePullPolicy: corev1.PullIfNotPresent,
				TerminationMessagePath: corev1.TerminationMessagePathDefault, TerminationMessagePolicy: corev1.TerminationMessageReadFile,
				Env: []corev1.EnvVar{
					{Name: "RACER_CLUSTER_ID", Value: string(c.Cluster)},
					{Name: "RACER_CONTROL_URL", Value: c.ControlURL},
					{Name: "RACER_PEER_PORT", Value: strconv.Itoa(int(c.PeerPort))},
					{Name: "RACER_BOOTSTRAP_TRUST", Value: "/etc/racer/bootstrap/ca.crt"},
					{Name: "RACER_TOKEN_FILE", Value: "/var/run/racer-token/token"},
					{Name: "RACER_KEYRING_DIRECTORY", Value: "/etc/racer/keyring"},
					{Name: "RACER_IDENTITY_DIRECTORY", Value: "/var/lib/racer/identity"},
					{Name: "RACER_SLAB_DIRECTORY", Value: "/var/lib/racer/slabs"},
				},
				Ports:           []corev1.ContainerPort{{Name: "peer", ContainerPort: int32(c.PeerPort), Protocol: corev1.ProtocolTCP}},
				SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
				VolumeMounts:    []corev1.VolumeMount{{Name: "token", MountPath: "/var/run/racer-token", ReadOnly: true}, {Name: "keyring", MountPath: "/etc/racer/keyring", ReadOnly: true}, {Name: "bootstrap", MountPath: "/etc/racer/bootstrap", ReadOnly: true}, {Name: "identity", MountPath: "/var/lib/racer/identity"}, {Name: "slabs", MountPath: "/var/lib/racer/slabs"}, {Name: "sockets", MountPath: "/run/racer"}},
			}},
			Volumes: []corev1.Volume{
				{Name: "token", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{DefaultMode: ptr.To(int32(0o400)), Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Audience: wire.TokenAudience, ExpirationSeconds: ptr.To(int64(3600)), Path: "token"}}}}}},
				{Name: "keyring", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: c.KeyringSecretName, DefaultMode: ptr.To(int32(0o400)), Items: []corev1.KeyToPath{{Key: "bundle.json", Path: "bundle.json"}}}}},
				{Name: "bootstrap", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: c.BootstrapTrustConfigMap}, DefaultMode: ptr.To(int32(0o444)), Items: []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}}}}},
			},
		}},
	}}
	for _, mount := range []struct{ name, path string }{{"identity", "/var/lib/racer/identity"}, {"slabs", "/var/lib/racer/slabs"}, {"sockets", "/run/racer"}} {
		ds.Spec.Template.Spec.Volumes = append(ds.Spec.Template.Spec.Volumes, corev1.Volume{Name: mount.name, VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: mount.path, Type: ptr.To(corev1.HostPathDirectoryOrCreate)}}})
	}

	return ds, nil
}

func (r *WorkloadReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("racer-workload").
		WatchesRawSource(initialEnqueue()).
		Watches(&appsv1.DaemonSet{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(namedChanges(r.Config.Namespace, r.Config.DaemonSetName))).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(namedChanges(r.Config.Namespace, r.Config.BootstrapTrustConfigMap))).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}
