// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	publicmachineops "github.com/Azure/unbounded/pkg/machineops"
)

const (
	// DefaultClassRefreshInterval is how long the MachineClass controller
	// waits before resolving a class again when no offering expires sooner,
	// and after an input failure.
	DefaultClassRefreshInterval = 5 * time.Minute

	// DefaultClassMinRequeueInterval is the shortest requeue after a class
	// resolved successfully.
	DefaultClassMinRequeueInterval = 10 * time.Second

	// ClassMinRequeueIntervalLowerBound is the smallest accepted minimum
	// requeue interval.
	ClassMinRequeueIntervalLowerBound = time.Second
)

const (
	classReasonResolved                   = "Resolved"
	classReasonSiteNotFound               = "SiteNotFound"
	classReasonSiteDeleting               = "SiteDeleting"
	classReasonProviderConfigInvalid      = "ProviderConfigInvalid"
	classReasonProviderConfigForbidden    = "ProviderConfigForbidden"
	classReasonProviderConfigNotFound     = "ProviderConfigNotFound"
	classReasonProviderConfigNotImmutable = "ProviderConfigNotImmutable"
	classReasonProviderConfigRejected     = "ProviderConfigRejected"
	classReasonBootstrapFilesForbidden    = "BootstrapFilesForbidden"
	classReasonBootstrapFilesNotFound     = "BootstrapFilesNotFound"
	classReasonBootstrapFilesNotImmutable = "BootstrapFilesNotImmutable"
	classReasonBootstrapFilesInvalid      = "BootstrapFilesInvalid"
	classReasonConfigurationInvalid       = "ConfigurationInvalid"
	classReasonConfigurationNotFound      = "ConfigurationNotFound"
	classReasonConfigurationMismatch      = "ConfigurationMismatch"
	classReasonResolveFailed              = "ResolveFailed"
	classReasonOfferingsInvalid           = "OfferingsInvalid"
	classReasonClassNotReady              = "ClassNotReady"
	classReasonOfferingsAvailable         = "OfferingsAvailable"
	classReasonNoOfferingsAvailable       = "NoOfferingsAvailable"
)

// MachineClassReconciler resolves the inputs of MachineClasses whose provider
// declares capacity and publishes them, with the provider's offerings, in the
// class status.
type MachineClassReconciler struct {
	// Client reads MachineClasses, Sites, MachineConfigurationVersions and
	// MachineOperationCredentials, and writes MachineClass status.
	client.Client
	// ConfigMapReader reads provider-config and bootstrap files ConfigMaps.
	ConfigMapReader client.Reader
	// SecretReader reads credential Secrets.
	SecretReader              client.Reader
	Providers                 []*Provider
	SiteName                  string
	CredentialSecretNamespace string
	ProviderConfigNamespace   string
	ClassRefreshInterval      time.Duration
	MinRequeueInterval        time.Duration
	Now                       func() time.Time
}

// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=machineclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=machineclasses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=sites;machineconfigurationversions;machineoperationcredentials,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get

func (r *MachineClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var class unboundedv1alpha3.MachineClass
	if err := r.Get(ctx, client.ObjectKey{Name: req.Name}, &class); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	capacity, inScope := r.capacityFor(&class)
	if !inScope || !class.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	inputs, failure, err := r.resolveClassInputs(ctx, &class)
	if err != nil {
		return ctrl.Result{}, err
	}

	if failure != nil {
		return r.publishNotReady(ctx, &class, failure.Reason, failure.Message)
	}

	resolution, err := capacity.ResolveClass(ctx, publicmachineops.ClassRequest{
		ClassName:      class.Name,
		ClassUID:       class.UID,
		SiteName:       class.Spec.SiteRef.Name,
		ProviderConfig: publicmachineops.ProviderConfig{Data: inputs.providerConfigData},
		Auth:           inputs.auth,
	})
	if err != nil {
		var permanentErr *publicmachineops.PermanentError
		if errors.As(err, &permanentErr) {
			message := fmt.Sprintf("provider %s rejected MachineClass %s: %v", class.Spec.Provider, class.Name, err)
			return r.publishNotReady(ctx, &class, classReasonProviderConfigRejected, message)
		}

		message := fmt.Sprintf("provider %s failed to resolve MachineClass %s: %v", class.Spec.Provider, class.Name, err)

		return r.publishTransientFailure(ctx, &class, inputs.revision, classReasonResolveFailed, message, err)
	}

	offerings, err := normalizeOfferings(resolution.Offerings)
	if err != nil {
		message := fmt.Sprintf("provider %s returned invalid offerings for MachineClass %s: %v", class.Spec.Provider, class.Name, err)
		return r.publishTransientFailure(ctx, &class, inputs.revision, classReasonOfferingsInvalid, message, err)
	}

	return r.publishResolved(ctx, &class, inputs, offerings)
}

// publishResolved replaces the revision, the snapshots and the offerings in
// one status write.
func (r *MachineClassReconciler) publishResolved(
	ctx context.Context,
	class *unboundedv1alpha3.MachineClass,
	inputs *classInputs,
	offerings []unboundedv1alpha3.MachineClassOffering,
) (ctrl.Result, error) {
	now := r.now()
	status := class.Status.DeepCopy()
	status.ObservedGeneration = class.Generation
	status.ResolvedRevision = inputs.revision
	status.ProviderConfig = inputs.providerConfig
	status.BootstrapFiles = inputs.bootstrapFiles
	status.Configuration = inputs.configuration
	status.Site = inputs.site
	status.Credential = inputs.credential
	status.Offerings = offerings

	r.setClassCondition(status, unboundedv1alpha3.MachineClassConditionReady, metav1.ConditionTrue, classReasonResolved,
		fmt.Sprintf("MachineClass inputs resolved at revision %s", inputs.revision), class.Generation)

	if offeringsCurrent(offerings, now) {
		r.setClassCondition(status, unboundedv1alpha3.MachineClassConditionOfferingsCurrent, metav1.ConditionTrue, classReasonOfferingsAvailable,
			"at least one offering is Available and unexpired", class.Generation)
	} else {
		r.setClassCondition(status, unboundedv1alpha3.MachineClassConditionOfferingsCurrent, metav1.ConditionFalse, classReasonNoOfferingsAvailable,
			"no offering is Available and unexpired", class.Generation)
	}

	if err := r.updateClassStatus(ctx, class, status); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: r.resolvedRequeueAfter(offerings, now)}, nil
}

// publishNotReady reports an input failure or a PermanentError. The published
// revision, snapshots and offerings are kept.
func (r *MachineClassReconciler) publishNotReady(
	ctx context.Context,
	class *unboundedv1alpha3.MachineClass,
	reason string,
	message string,
) (ctrl.Result, error) {
	status := class.Status.DeepCopy()
	r.setClassCondition(status, unboundedv1alpha3.MachineClassConditionReady, metav1.ConditionFalse, reason, message, class.Generation)
	r.setClassCondition(status, unboundedv1alpha3.MachineClassConditionOfferingsCurrent, metav1.ConditionFalse, classReasonClassNotReady,
		"MachineClass is not Ready", class.Generation)

	if err := r.updateClassStatus(ctx, class, status); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: r.classRefreshInterval()}, nil
}

// publishTransientFailure reports a transient provider failure. The published
// revision, snapshots and offerings are kept, and Ready stays True only when
// neither the revision nor the generation moved since it was set. The cause is
// returned so the workqueue backs off.
func (r *MachineClassReconciler) publishTransientFailure(
	ctx context.Context,
	class *unboundedv1alpha3.MachineClass,
	revision string,
	reason string,
	message string,
	cause error,
) (ctrl.Result, error) {
	status := class.Status.DeepCopy()

	keepReady := apimeta.IsStatusConditionTrue(status.Conditions, unboundedv1alpha3.MachineClassConditionReady) &&
		revision == status.ResolvedRevision &&
		class.Generation == status.ObservedGeneration
	if !keepReady {
		r.setClassCondition(status, unboundedv1alpha3.MachineClassConditionReady, metav1.ConditionFalse, reason, message, class.Generation)
	}

	r.setClassCondition(status, unboundedv1alpha3.MachineClassConditionOfferingsCurrent, metav1.ConditionFalse, reason, message, class.Generation)

	if err := r.updateClassStatus(ctx, class, status); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, fmt.Errorf("resolve MachineClass %s: %w", class.Name, cause)
}

func (r *MachineClassReconciler) setClassCondition(
	status *unboundedv1alpha3.MachineClassStatus,
	conditionType string,
	conditionStatus metav1.ConditionStatus,
	reason string,
	message string,
	generation int64,
) {
	apimeta.SetStatusCondition(&status.Conditions, normalizeCondition(metav1.Condition{
		Type:               conditionType,
		Status:             conditionStatus,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
		LastTransitionTime: metav1.NewTime(r.now()),
	}))
}

// updateClassStatus writes status with the resourceVersion of the class read
// at the start of the reconcile. A conflict is returned, not retried.
func (r *MachineClassReconciler) updateClassStatus(
	ctx context.Context,
	class *unboundedv1alpha3.MachineClass,
	status *unboundedv1alpha3.MachineClassStatus,
) error {
	if equality.Semantic.DeepEqual(class.Status, *status) {
		return nil
	}

	updated := class.DeepCopy()
	updated.Status = *status

	if err := r.Status().Update(ctx, updated); err != nil {
		return fmt.Errorf("update MachineClass %s status: %w", class.Name, err)
	}

	return nil
}

// resolvedRequeueAfter is the refresh interval or the time until the earliest
// unexpired validUntil, whichever is sooner, but never below the minimum
// requeue interval.
func (r *MachineClassReconciler) resolvedRequeueAfter(offerings []unboundedv1alpha3.MachineClassOffering, now time.Time) time.Duration {
	requeueAfter := r.classRefreshInterval()

	for i := range offerings {
		validUntil := offerings[i].ValidUntil
		if validUntil == nil || !validUntil.After(now) {
			continue
		}

		requeueAfter = min(requeueAfter, validUntil.Sub(now))
	}

	return max(requeueAfter, r.minRequeueInterval())
}

func offeringsCurrent(offerings []unboundedv1alpha3.MachineClassOffering, now time.Time) bool {
	for i := range offerings {
		offering := &offerings[i]
		if offering.Availability == unboundedv1alpha3.OfferingAvailable && offering.ValidUntil != nil && offering.ValidUntil.After(now) {
			return true
		}
	}

	return false
}

func (r *MachineClassReconciler) classRefreshInterval() time.Duration {
	return durationOrDefault(r.ClassRefreshInterval, DefaultClassRefreshInterval)
}

func (r *MachineClassReconciler) minRequeueInterval() time.Duration {
	return durationOrDefault(r.MinRequeueInterval, DefaultClassMinRequeueInterval)
}

func (r *MachineClassReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}

	return time.Now()
}

// capacityFor returns the capacity registration of the class's provider when
// the class is in this controller's scope.
func (r *MachineClassReconciler) capacityFor(class *unboundedv1alpha3.MachineClass) (*publicmachineops.Capacity, bool) {
	if r.SiteName != "" && class.Spec.SiteRef.Name != r.SiteName {
		return nil, false
	}

	for _, provider := range r.Providers {
		if provider != nil && provider.Name() == class.Spec.Provider {
			return provider.Capacity()
		}
	}

	return nil, false
}

func (r *MachineClassReconciler) inScope(object client.Object) bool {
	class, ok := object.(*unboundedv1alpha3.MachineClass)
	if !ok {
		return false
	}

	_, inScope := r.capacityFor(class)

	return inScope
}

// SetupWithManager registers the controller with mgr. ConfigMaps are watched
// through configMaps, the cache that ConfigMapReader reads from.
func (r *MachineClassReconciler) SetupWithManager(mgr ctrl.Manager, configMaps cache.Cache) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("machineclass").
		For(&unboundedv1alpha3.MachineClass{}, builder.WithPredicates(r.classPredicate())).
		Watches(&unboundedv1alpha3.Site{}, handler.EnqueueRequestsFromMapFunc(r.classesForSite)).
		Watches(&unboundedv1alpha3.MachineConfigurationVersion{}, handler.EnqueueRequestsFromMapFunc(r.classesForConfigurationVersion)).
		Watches(&unboundedv1alpha3.MachineOperationCredential{}, handler.EnqueueRequestsFromMapFunc(r.classesForCredential)).
		WatchesRawSource(source.Kind(configMaps, &corev1.ConfigMap{}, handler.TypedEnqueueRequestsFromMapFunc(r.classesForConfigMap))).
		Complete(r)
}

// classPredicate passes events for in-scope classes. Updates pass only on a
// generation change, so the controller's own status writes do not trigger it.
func (r *MachineClassReconciler) classPredicate() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return r.inScope(e.Object) },
		UpdateFunc: func(e event.UpdateEvent) bool {
			return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() && r.inScope(e.ObjectNew)
		},
		DeleteFunc:  func(e event.DeleteEvent) bool { return r.inScope(e.Object) },
		GenericFunc: func(e event.GenericEvent) bool { return r.inScope(e.Object) },
	}
}

func (r *MachineClassReconciler) classesForSite(ctx context.Context, site client.Object) []reconcile.Request {
	return r.classesReferencing(ctx, func(class *unboundedv1alpha3.MachineClass) bool {
		return class.Spec.SiteRef.Name == site.GetName()
	})
}

func (r *MachineClassReconciler) classesForConfigurationVersion(ctx context.Context, version client.Object) []reconcile.Request {
	configuration := version.GetLabels()[unboundedv1alpha3.MCVConfigurationLabelKey]
	if configuration == "" {
		return nil
	}

	return r.classesReferencing(ctx, func(class *unboundedv1alpha3.MachineClass) bool {
		return class.Spec.ConfigurationRef.Name == configuration
	})
}

func (r *MachineClassReconciler) classesForCredential(ctx context.Context, object client.Object) []reconcile.Request {
	credential, ok := object.(*unboundedv1alpha3.MachineOperationCredential)
	if !ok {
		return nil
	}

	return r.classesReferencing(ctx, func(class *unboundedv1alpha3.MachineClass) bool {
		return class.Spec.SiteRef.Name == credential.Spec.SiteName && class.Spec.Provider == credential.Spec.Provider
	})
}

func (r *MachineClassReconciler) classesForConfigMap(ctx context.Context, configMap *corev1.ConfigMap) []reconcile.Request {
	return r.classesReferencing(ctx, func(class *unboundedv1alpha3.MachineClass) bool {
		ref := class.Spec.ProviderConfigRef
		if ref.Namespace == configMap.Namespace && ref.Name == configMap.Name {
			return true
		}

		bootstrap := class.Spec.Bootstrap

		return bootstrap != nil && bootstrap.FilesRef != nil &&
			bootstrap.FilesRef.Namespace == configMap.Namespace && bootstrap.FilesRef.Name == configMap.Name
	})
}

// classesReferencing lists MachineClasses from the manager cache and returns
// the in-scope classes for which references reports true.
func (r *MachineClassReconciler) classesReferencing(
	ctx context.Context,
	references func(*unboundedv1alpha3.MachineClass) bool,
) []reconcile.Request {
	var classes unboundedv1alpha3.MachineClassList
	if err := r.List(ctx, &classes); err != nil {
		log.FromContext(ctx).Error(err, "list MachineClasses for a watch event")
		return nil
	}

	var requests []reconcile.Request

	for i := range classes.Items {
		class := &classes.Items[i]
		if r.inScope(class) && references(class) {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: class.Name}})
		}
	}

	return requests
}
