// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/kube"
	publicmachineops "github.com/Azure/unbounded/pkg/machineops"
)

// reconcileActive takes the next launch step of an allocation that is not
// retired: the snapshot, then the finalizer and labels, then the input
// protection, the token, the Machine and the HostAllocate operation.
func (r *MachineAllocationReconciler) reconcileActive(
	ctx context.Context,
	s *allocationState,
	capacity *publicmachineops.Capacity,
) (ctrl.Result, error) {
	switch {
	case s.status.Snapshot == nil && s.hasFinalizer():
		// The finalizer is added only after the snapshot, so the status was
		// lost. Launching again could start a second host.
		r.setCondition(s, unboundedv1alpha3.MachineAllocationConditionAllocated, metav1.ConditionFalse, allocationReasonSnapshotMissing,
			"the finalizer is present but status.snapshot is missing; the status was lost and the allocation needs manual recovery")

		return r.finishWrite(ctx, s)
	case s.status.Snapshot == nil:
		if s.failed() {
			return ctrl.Result{}, nil
		}

		return r.reconcileSnapshot(ctx, s, capacity)
	case !s.hasFinalizer():
		if s.failed() {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, r.addAllocationFinalizer(ctx, s)
	}

	if err := r.reconcileLaunch(ctx, s); err != nil {
		return ctrl.Result{}, err
	}

	return r.finishWrite(ctx, s)
}

// reconcileSnapshot selects the offering, plans the host and writes the
// snapshot in one status write.
func (r *MachineAllocationReconciler) reconcileSnapshot(
	ctx context.Context,
	s *allocationState,
	capacity *publicmachineops.Capacity,
) (ctrl.Result, error) {
	allocation := s.allocation
	classRef := allocation.Spec.ClassRef

	// The allocation's machine-class and consumer-uid labels carry these
	// values as written, so they are checked before anything else.
	if message := invalidLabelValues(map[string]string{
		"spec.classRef.name":   classRef.Name,
		"spec.consumerRef.uid": string(allocation.Spec.ConsumerRef.UID),
	}); message != "" {
		r.fail(s, unboundedv1alpha3.FailureInvalidRequest, message)
		return r.finishWrite(ctx, s)
	}

	var class unboundedv1alpha3.MachineClass
	if err := r.Get(ctx, client.ObjectKey{Name: classRef.Name}, &class); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("get MachineClass %s: %w", classRef.Name, err)
		}

		r.fail(s, unboundedv1alpha3.FailureInvalidRequest, fmt.Sprintf("MachineClass %s was not found", classRef.Name))

		return r.finishWrite(ctx, s)
	}

	if class.UID != classRef.UID {
		r.fail(s, unboundedv1alpha3.FailureInvalidRequest, fmt.Sprintf("MachineClass %s has UID %s, not %s", classRef.Name, class.UID, classRef.UID))
		return r.finishWrite(ctx, s)
	}

	if !machineClassReady(&class) {
		r.setCondition(s, unboundedv1alpha3.MachineAllocationConditionAllocated, metav1.ConditionFalse, allocationReasonClassNotReady,
			fmt.Sprintf("waiting for MachineClass %s to resolve its generation %d", class.Name, class.Generation))
		s.wait(allocationWaitInterval)

		return r.finishWrite(ctx, s)
	}

	inputs, changed, err := r.verifyClassInputs(ctx, &class)
	if err != nil {
		return ctrl.Result{}, err
	}

	if changed != "" {
		r.setCondition(s, unboundedv1alpha3.MachineAllocationConditionAllocated, metav1.ConditionFalse, allocationReasonClassInputsChanged, changed)
		s.wait(allocationWaitInterval)

		return r.finishWrite(ctx, s)
	}

	if host := inputs.template.Host; host != nil && host.ProvisioningFormat != "" && host.ProvisioningFormat != unboundedv1alpha3.ProvisioningFormatCloudInit {
		r.fail(s, unboundedv1alpha3.FailureInvalidRequest,
			fmt.Sprintf("MachineConfigurationVersion %s uses provisioningFormat %s; allocated hosts support only %s",
				class.Status.Configuration.VersionName, host.ProvisioningFormat, unboundedv1alpha3.ProvisioningFormatCloudInit))

		return r.finishWrite(ctx, s)
	}

	selected, failure := selectOffering(class.Status.Offerings, allocation.Spec.Resources, allocation.Spec.Requirements, r.now())
	if failure != nil {
		r.fail(s, failure.reason, failure.message)
		return r.finishWrite(ctx, s)
	}

	labels := allocationLabels(&inputs.template, allocation.Spec.NodeTemplate, selected.labels)
	if message := invalidMachineLabels(class.Status.Site.Name, kubeletNodeLabels(labels)); message != "" {
		r.fail(s, unboundedv1alpha3.FailureInvalidRequest, message)
		return r.finishWrite(ctx, s)
	}

	plan, err := capacity.PlanAllocation(ctx, publicmachineops.AllocationPlanRequest{
		AllocationName: allocation.Name,
		AllocationUID:  allocation.UID,
		HostName:       s.names.machine,
		ProviderConfig: publicmachineops.ProviderConfig{Data: inputs.providerConfig},
		Offering:       *selected.offering.DeepCopy(),
	})
	if err != nil {
		var permanentErr *publicmachineops.PermanentError
		if !errors.As(err, &permanentErr) {
			return ctrl.Result{}, fmt.Errorf("plan MachineAllocation %s: %w", allocation.Name, err)
		}

		r.fail(s, unboundedv1alpha3.FailureInvalidRequest, fmt.Sprintf("provider %s cannot plan MachineAllocation %s: %v", class.Spec.Provider, allocation.Name, err))

		return r.finishWrite(ctx, s)
	}

	if strings.TrimSpace(plan.ProviderID) == "" {
		r.fail(s, unboundedv1alpha3.FailureInvalidRequest, fmt.Sprintf("provider %s planned no providerID for MachineAllocation %s", class.Spec.Provider, allocation.Name))
		return r.finishWrite(ctx, s)
	}

	snapshot := &unboundedv1alpha3.MachineAllocationSnapshot{
		ClassUID:              class.UID,
		ClassGeneration:       class.Generation,
		ClassRevision:         class.Status.ResolvedRevision,
		Provider:              class.Spec.Provider,
		SiteName:              class.Status.Site.Name,
		SiteUID:               class.Status.Site.UID,
		Credential:            *class.Status.Credential.DeepCopy(),
		Offering:              *selected.offering.DeepCopy(),
		ProviderConfig:        *class.Status.ProviderConfig.DeepCopy(),
		BootstrapFiles:        class.Status.BootstrapFiles.DeepCopy(),
		Configuration:         *class.Status.Configuration.DeepCopy(),
		ConfigurationTemplate: *inputs.template.DeepCopy(),
		HostImage:             plan.HostImage,
		HostProviderID:        plan.ProviderID,
	}

	if class.Spec.Bootstrap != nil {
		snapshot.AgentArtifact = class.Spec.Bootstrap.AgentArtifact.DeepCopy()
	}

	s.status.Snapshot = snapshot
	s.status.ProviderID = unboundedv1alpha3.AllocationProviderIDPrefix + string(allocation.UID)
	s.status.InstanceType = selected.offering.InstanceType
	s.status.Zone = selected.offering.Zone
	s.status.CapacityType = selected.offering.CapacityType
	s.status.Image = plan.HostImage
	s.status.Capacity = selected.offering.Capacity.DeepCopy()
	s.status.Allocatable = capacityAfterOverhead(&selected.offering)
	s.status.Labels = labels
	r.setCondition(s, unboundedv1alpha3.MachineAllocationConditionAllocated, metav1.ConditionFalse, allocationReasonLaunching,
		fmt.Sprintf("offering %s planned as %s", selected.offering.Name, plan.ProviderID))

	return r.finishWrite(ctx, s)
}

// invalidLabelValues returns a message naming each field whose value is not
// a valid label value, or "" when all are valid.
func invalidLabelValues(fields map[string]string) string {
	var problems []string

	for _, field := range slices.Sorted(maps.Keys(fields)) {
		if errs := validation.IsValidLabelValue(fields[field]); len(errs) > 0 {
			problems = append(problems, fmt.Sprintf("%s %q is not a valid label value: %s", field, fields[field], strings.Join(errs, "; ")))
		}
	}

	return strings.Join(problems, "; ")
}

// invalidMachineLabels returns a message naming the Site label value or the
// node label of the allocation's Machine that is not valid, or "" when all
// are valid.
func invalidMachineLabels(siteName string, nodeLabels map[string]string) string {
	fields := map[string]string{"MachineClass status.site.name": siteName}

	var problems []string

	for _, key := range slices.Sorted(maps.Keys(nodeLabels)) {
		if errs := validation.IsQualifiedName(key); len(errs) > 0 {
			problems = append(problems, fmt.Sprintf("node label key %q is not a valid label key: %s", key, strings.Join(errs, "; ")))
		}

		fields["node label "+key] = nodeLabels[key]
	}

	if message := invalidLabelValues(fields); message != "" {
		problems = append(problems, message)
	}

	return strings.Join(problems, "; ")
}

// machineClassReady reports whether the class resolved its current
// generation and published every input an allocation freezes.
func machineClassReady(class *unboundedv1alpha3.MachineClass) bool {
	status := &class.Status
	if status.ObservedGeneration != class.Generation || status.ResolvedRevision == "" ||
		status.ProviderConfig == nil || status.Configuration == nil || status.Site == nil || status.Credential == nil {
		return false
	}

	ready := apimeta.FindStatusCondition(status.Conditions, unboundedv1alpha3.MachineClassConditionReady)

	return ready != nil && ready.Status == metav1.ConditionTrue && ready.ObservedGeneration == class.Generation
}

// verifiedClassInputs is the class content an allocation freezes.
type verifiedClassInputs struct {
	providerConfig map[string]string
	template       unboundedv1alpha3.MachineConfigurationTemplate
}

// verifyClassInputs reads the live class inputs and checks them against the
// class status. It returns a message naming the first input that changed.
func (r *MachineAllocationReconciler) verifyClassInputs(
	ctx context.Context,
	class *unboundedv1alpha3.MachineClass,
) (*verifiedClassInputs, string, error) {
	status := &class.Status

	var site unboundedv1alpha3.Site
	if err := r.Get(ctx, client.ObjectKey{Name: status.Site.Name}, &site); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, "", fmt.Errorf("get Site %s: %w", status.Site.Name, err)
		}

		return nil, fmt.Sprintf("Site %s was not found", status.Site.Name), nil
	}

	if site.UID != status.Site.UID {
		return nil, fmt.Sprintf("Site %s has UID %s, not %s", site.Name, site.UID, status.Site.UID), nil
	}

	if !site.DeletionTimestamp.IsZero() {
		return nil, fmt.Sprintf("Site %s is being deleted", site.Name), nil
	}

	providerConfig, changed, err := r.verifyConfigMap(ctx, status.ProviderConfig)
	if err != nil || changed != "" {
		return nil, changed, err
	}

	if status.BootstrapFiles != nil {
		if _, changed, err := r.verifyConfigMap(ctx, status.BootstrapFiles); err != nil || changed != "" {
			return nil, changed, err
		}
	}

	frozen := status.Configuration

	var version unboundedv1alpha3.MachineConfigurationVersion
	if err := r.Get(ctx, client.ObjectKey{Name: frozen.VersionName}, &version); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, "", fmt.Errorf("get MachineConfigurationVersion %s: %w", frozen.VersionName, err)
		}

		return nil, fmt.Sprintf("MachineConfigurationVersion %s was not found", frozen.VersionName), nil
	}

	hash, err := contentHash(version.Spec.Template)
	if err != nil {
		return nil, "", fmt.Errorf("hash MachineConfigurationVersion %s template: %w", version.Name, err)
	}

	if version.UID != frozen.UID || hash != frozen.Hash {
		return nil, fmt.Sprintf("MachineConfigurationVersion %s changed since MachineClass %s resolved it", version.Name, class.Name), nil
	}

	return &verifiedClassInputs{
		providerConfig: maps.Clone(providerConfig.Data),
		template:       *version.Spec.Template.DeepCopy(),
	}, "", nil
}

// verifyConfigMap reads a class input ConfigMap through ConfigMapReader and
// checks its UID and content hash.
func (r *MachineAllocationReconciler) verifyConfigMap(
	ctx context.Context,
	frozen *unboundedv1alpha3.ObjectContentSnapshot,
) (*corev1.ConfigMap, string, error) {
	var configMap corev1.ConfigMap
	if err := r.ConfigMapReader.Get(ctx, client.ObjectKey{Namespace: frozen.Namespace, Name: frozen.Name}, &configMap); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, "", fmt.Errorf("get ConfigMap %s/%s: %w", frozen.Namespace, frozen.Name, err)
		}

		return nil, fmt.Sprintf("ConfigMap %s/%s was not found", frozen.Namespace, frozen.Name), nil
	}

	hash, err := configMapDataHash(configMap.Data)
	if err != nil {
		return nil, "", fmt.Errorf("hash ConfigMap %s/%s: %w", frozen.Namespace, frozen.Name, err)
	}

	if configMap.UID != frozen.UID || hash != frozen.Hash {
		return nil, fmt.Sprintf("ConfigMap %s/%s changed since the MachineClass resolved it", frozen.Namespace, frozen.Name), nil
	}

	return &configMap, "", nil
}

// addAllocationFinalizer adds the finalizer and the labels after the
// snapshot is written.
func (r *MachineAllocationReconciler) addAllocationFinalizer(ctx context.Context, s *allocationState) error {
	allocation := s.allocation
	labels := map[string]string{
		unboundedv1alpha3.LabelAllocationUID: string(allocation.UID),
		unboundedv1alpha3.LabelConsumerUID:   string(allocation.Spec.ConsumerRef.UID),
		unboundedv1alpha3.LabelMachineClass:  allocation.Spec.ClassRef.Name,
	}

	return r.patchMetadata(ctx, s, allocation, func(updated client.Object) bool {
		current := updated.GetLabels()
		if current == nil {
			current = map[string]string{}
		}

		maps.Copy(current, labels)
		updated.SetLabels(current)
		controllerutil.AddFinalizer(updated, unboundedv1alpha3.MachineAllocationFinalizer)

		return true
	})
}

// reconcileLaunch takes the launch steps of an allocation with a finalizer
// and a snapshot, then observes its recorded HostAllocate.
func (r *MachineAllocationReconciler) reconcileLaunch(ctx context.Context, s *allocationState) error {
	recorded := s.status.Operations.Allocate
	if recorded == nil && s.failed() {
		return nil
	}

	var operation *unboundedv1alpha3.MachineOperation

	if recorded == nil {
		created, err := r.launch(ctx, s)
		if err != nil || created == nil {
			return err
		}

		s.status.Operations.Allocate = &unboundedv1alpha3.UIDReference{Name: created.Name, UID: created.UID}
		operation = created
	} else {
		found, err := r.recordedOperation(ctx, recorded)
		if err != nil {
			return err
		}

		if found == nil {
			r.allocateOperationMissing(s, recorded)
			return nil
		}

		operation = found
	}

	if err := r.observeAllocateOperation(ctx, s, operation); err != nil {
		return err
	}

	if !apimeta.IsStatusConditionTrue(s.status.Conditions, unboundedv1alpha3.MachineAllocationConditionAllocated) {
		return nil
	}

	return r.registerNode(ctx, s)
}

// launch protects the inputs, mints the token, creates the Machine and
// creates or adopts the HostAllocate operation. It returns the operation to
// record, or nil when the allocation failed.
func (r *MachineAllocationReconciler) launch(ctx context.Context, s *allocationState) (*unboundedv1alpha3.MachineOperation, error) {
	if ok, err := r.protectInputs(ctx, s); err != nil || !ok {
		return nil, err
	}

	if ok, err := r.ensureToken(ctx, s); err != nil || !ok {
		return nil, err
	}

	machine, err := r.ensureMachine(ctx, s)
	if err != nil || machine == nil {
		return nil, err
	}

	s.status.MachineRef = &unboundedv1alpha3.UIDReference{Name: machine.Name, UID: machine.UID}

	return r.ensureOperation(ctx, s, unboundedv1alpha3.OperationHostAllocate, func() bool {
		// The offering is checked again right before the first create.
		offering := &s.status.Snapshot.Offering
		if offeringLaunchable(offering, r.now()) {
			return true
		}

		r.fail(s, unboundedv1alpha3.FailureInsufficientCapacity,
			fmt.Sprintf("offering %s is no longer Available and unexpired", offering.Name))

		return false
	})
}

// inputConfigMaps returns the snapshot's input ConfigMap references.
func inputConfigMaps(snapshot *unboundedv1alpha3.MachineAllocationSnapshot) []*unboundedv1alpha3.ObjectContentSnapshot {
	inputs := []*unboundedv1alpha3.ObjectContentSnapshot{&snapshot.ProviderConfig}
	if snapshot.BootstrapFiles != nil {
		inputs = append(inputs, snapshot.BootstrapFiles)
	}

	return inputs
}

// protectInputs adds the allocation's input finalizer to each input
// ConfigMap. A missing, deleting or replaced ConfigMap fails the allocation.
func (r *MachineAllocationReconciler) protectInputs(ctx context.Context, s *allocationState) (bool, error) {
	for _, frozen := range inputConfigMaps(s.status.Snapshot) {
		var configMap corev1.ConfigMap

		err := r.ConfigMapReader.Get(ctx, client.ObjectKey{Namespace: frozen.Namespace, Name: frozen.Name}, &configMap)
		if err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("get ConfigMap %s/%s: %w", frozen.Namespace, frozen.Name, err)
		}

		if err != nil || !configMap.DeletionTimestamp.IsZero() || configMap.UID != frozen.UID {
			r.fail(s, unboundedv1alpha3.FailureInvalidRequest,
				fmt.Sprintf("input ConfigMap %s/%s is missing, being deleted or replaced", frozen.Namespace, frozen.Name))

			return false, nil
		}

		err = r.patchMetadata(ctx, s, &configMap, func(updated client.Object) bool {
			return controllerutil.AddFinalizer(updated, s.names.inputFinalizer)
		})
		if apierrors.IsNotFound(err) {
			// The cache can still show a ConfigMap the API server deleted.
			r.fail(s, unboundedv1alpha3.FailureInvalidRequest,
				fmt.Sprintf("input ConfigMap %s/%s is missing, being deleted or replaced", frozen.Namespace, frozen.Name))

			return false, nil
		}

		if err != nil {
			return false, err
		}
	}

	return true, nil
}

// unprotectInputs removes the allocation's input finalizer from each input
// ConfigMap. A missing ConfigMap counts as done.
func (r *MachineAllocationReconciler) unprotectInputs(ctx context.Context, s *allocationState, snapshot *unboundedv1alpha3.MachineAllocationSnapshot) error {
	for _, frozen := range inputConfigMaps(snapshot) {
		var configMap corev1.ConfigMap
		if err := r.ConfigMapReader.Get(ctx, client.ObjectKey{Namespace: frozen.Namespace, Name: frozen.Name}, &configMap); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}

			return fmt.Errorf("get ConfigMap %s/%s: %w", frozen.Namespace, frozen.Name, err)
		}

		if err := r.removeFinalizer(ctx, s, &configMap, s.names.inputFinalizer); err != nil {
			return err
		}
	}

	return nil
}

// ensureToken creates the allocation's bootstrap token Secret when it does
// not exist. A Secret with that name that is not this allocation's fails the
// allocation.
func (r *MachineAllocationReconciler) ensureToken(ctx context.Context, s *allocationState) (bool, error) {
	key := client.ObjectKey{Namespace: metav1.NamespaceSystem, Name: s.names.tokenSecret}

	var existing corev1.Secret

	err := r.APIReader.Get(ctx, key, &existing)
	if err == nil {
		if existing.Labels[unboundedv1alpha3.LabelAllocationUID] != string(s.allocation.UID) {
			r.fail(s, unboundedv1alpha3.FailureProviderConflict,
				fmt.Sprintf("bootstrap token Secret %s/%s exists and is not this allocation's", key.Namespace, key.Name))

			return false, nil
		}

		return true, nil
	}

	if !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("get bootstrap token Secret %s/%s: %w", key.Namespace, key.Name, err)
	}

	token, err := kube.NewBootstrapToken()
	if err != nil {
		return false, err
	}

	token.ID = s.names.tokenID
	token.ExpiresAt = r.now().UTC().Add(kube.DefaultBootstrapTokenTTL)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: key.Namespace,
			Name:      key.Name,
			Labels: map[string]string{
				unboundedv1alpha3.MachineSiteLabelKey: s.status.Snapshot.SiteName,
				unboundedv1alpha3.LabelAllocationUID:  string(s.allocation.UID),
			},
		},
		Type: corev1.SecretTypeBootstrapToken,
		Data: kube.BootstrapTokenSecretData(token),
	}

	if err := r.Create(ctx, secret); err != nil {
		return false, fmt.Errorf("create bootstrap token Secret %s/%s: %w", key.Namespace, key.Name, err)
	}

	return true, nil
}

// ensureMachine creates the allocation's Machine when it does not exist. A
// Machine with that name bound to another allocation fails the allocation.
func (r *MachineAllocationReconciler) ensureMachine(ctx context.Context, s *allocationState) (*unboundedv1alpha3.Machine, error) {
	key := client.ObjectKey{Name: s.names.machine}

	machine := &unboundedv1alpha3.Machine{}

	err := r.Get(ctx, key, machine)
	if apierrors.IsNotFound(err) {
		machine = r.newAllocationMachine(s)

		err = r.Create(ctx, machine)
		if apierrors.IsAlreadyExists(err) {
			machine = &unboundedv1alpha3.Machine{}
			err = r.APIReader.Get(ctx, key, machine)
		}
	}

	if err != nil {
		return nil, fmt.Errorf("ensure Machine %s: %w", key.Name, err)
	}

	if !boundTo(machine, s.allocation) {
		r.fail(s, unboundedv1alpha3.FailureProviderConflict, fmt.Sprintf("Machine %s exists and is bound to another allocation", key.Name))
		return nil, nil
	}

	return machine, nil
}

func (r *MachineAllocationReconciler) newAllocationMachine(s *allocationState) *unboundedv1alpha3.Machine {
	allocation := s.allocation
	snapshot := s.status.Snapshot
	template := &snapshot.ConfigurationTemplate
	version := snapshot.Configuration.Version

	machine := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name: s.names.machine,
			Labels: map[string]string{
				unboundedv1alpha3.MachineSiteLabelKey: snapshot.SiteName,
				unboundedv1alpha3.LabelAllocationUID:  string(allocation.UID),
				unboundedv1alpha3.LabelMachineClass:   allocation.Spec.ClassRef.Name,
			},
			Finalizers:      []string{unboundedv1alpha3.MachineAllocationFinalizer},
			OwnerReferences: []metav1.OwnerReference{allocationOwnerReference(allocation)},
		},
		Spec: unboundedv1alpha3.MachineSpec{
			AllocationRef: s.reference(),
			Host: &unboundedv1alpha3.HostSpec{
				Image:              snapshot.HostImage,
				ProvisioningFormat: unboundedv1alpha3.ProvisioningFormatCloudInit,
				External: &unboundedv1alpha3.ExternalHostSpec{
					Provider:   snapshot.Provider,
					ProviderID: snapshot.HostProviderID,
				},
			},
			ConfigurationRef: &unboundedv1alpha3.MachineConfigurationRef{Name: snapshot.Configuration.Name, Version: &version},
			Kubernetes: &unboundedv1alpha3.KubernetesSpec{
				NodeLabels:         kubeletNodeLabels(s.status.Labels),
				RegisterWithTaints: registrationTaints(template, allocation.Spec.NodeTemplate),
			},
		},
	}

	if template.Kubernetes != nil {
		machine.Spec.Kubernetes.Version = template.Kubernetes.Version
	}

	if template.Agent != nil {
		machine.Spec.Agent = &unboundedv1alpha3.AgentSpec{
			Image:    template.Agent.Image,
			LocalDNS: template.Agent.LocalDNS.DeepCopy(),
		}
	}

	return machine
}

func allocationOwnerReference(allocation *unboundedv1alpha3.MachineAllocation) metav1.OwnerReference {
	controller := true

	return metav1.OwnerReference{
		APIVersion: unboundedv1alpha3.GroupVersion.String(),
		Kind:       "MachineAllocation",
		Name:       allocation.Name,
		UID:        allocation.UID,
		Controller: &controller,
	}
}

// registrationTaints are the template taints, then the node template taints,
// then its startup taints, without duplicate key and effect pairs, formatted
// key[=value]:Effect.
func registrationTaints(
	template *unboundedv1alpha3.MachineConfigurationTemplate,
	nodeTemplate *unboundedv1alpha3.MachineAllocationNodeTemplate,
) []string {
	var taints []corev1.Taint

	if template.Kubernetes != nil {
		taints = append(taints, template.Kubernetes.RegisterWithTaints...)
	}

	if nodeTemplate != nil {
		taints = append(taints, nodeTemplate.Taints...)
		taints = append(taints, nodeTemplate.StartupTaints...)
	}

	var formatted []string

	seen := map[string]struct{}{}

	for _, taint := range taints {
		key := taint.Key + ":" + string(taint.Effect)
		if _, ok := seen[key]; ok {
			continue
		}

		seen[key] = struct{}{}

		if taint.Value == "" {
			formatted = append(formatted, key)
		} else {
			formatted = append(formatted, taint.Key+"="+taint.Value+":"+string(taint.Effect))
		}
	}

	return formatted
}

// kubeletSelfLabels are the labels in the kubernetes.io and k8s.io
// namespaces that the NodeRestriction admission plugin lets a kubelet set on
// its own Node.
var kubeletSelfLabels = []string{
	corev1.LabelHostname,
	corev1.LabelTopologyZone,
	corev1.LabelTopologyRegion,
	corev1.LabelFailureDomainBetaZone,
	corev1.LabelFailureDomainBetaRegion,
	corev1.LabelInstanceType,
	corev1.LabelInstanceTypeStable,
	corev1.LabelOSStable,
	corev1.LabelArchStable,
	"beta.kubernetes.io/os",
	"beta.kubernetes.io/arch",
}

// kubeletNodeLabels returns the labels a kubelet may set on its own Node.
func kubeletNodeLabels(labels map[string]string) map[string]string {
	allowed := map[string]string{}

	for key, value := range labels {
		if kubeletMaySetLabel(key) {
			allowed[key] = value
		}
	}

	return allowed
}

func kubeletMaySetLabel(key string) bool {
	namespace, _, found := strings.Cut(key, "/")
	if !found || !inLabelDomain(namespace, "kubernetes.io", "k8s.io") {
		return true
	}

	return slices.Contains(kubeletSelfLabels, key) || inLabelDomain(namespace, "kubelet.kubernetes.io", "node.kubernetes.io")
}

func inLabelDomain(namespace string, domains ...string) bool {
	for _, domain := range domains {
		if namespace == domain || strings.HasSuffix(namespace, "."+domain) {
			return true
		}
	}

	return false
}

// ensureOperation creates or adopts the allocation's operation of kind under
// its deterministic name. beforeCreate runs right before a create and can
// stop it. It returns nil when the allocation failed or the create was
// stopped.
func (r *MachineAllocationReconciler) ensureOperation(
	ctx context.Context,
	s *allocationState,
	kind unboundedv1alpha3.OperationKind,
	beforeCreate func() bool,
) (*unboundedv1alpha3.MachineOperation, error) {
	key := client.ObjectKey{Name: s.names.operationName(kind)}

	operation := &unboundedv1alpha3.MachineOperation{}

	err := r.Get(ctx, key, operation)
	if apierrors.IsNotFound(err) {
		if beforeCreate != nil && !beforeCreate() {
			return nil, nil
		}

		operation = &unboundedv1alpha3.MachineOperation{
			ObjectMeta: metav1.ObjectMeta{
				Name:            key.Name,
				Labels:          map[string]string{unboundedv1alpha3.LabelAllocationUID: string(s.allocation.UID)},
				Finalizers:      []string{unboundedv1alpha3.MachineAllocationFinalizer},
				OwnerReferences: []metav1.OwnerReference{allocationOwnerReference(s.allocation)},
			},
			Spec: unboundedv1alpha3.MachineOperationSpec{
				MachineRef:    s.names.machine,
				OperationKind: kind,
				AllocationRef: s.reference(),
			},
		}

		err = r.Create(ctx, operation)
		if apierrors.IsAlreadyExists(err) {
			operation = &unboundedv1alpha3.MachineOperation{}
			err = r.APIReader.Get(ctx, key, operation)
		}
	}

	if err != nil {
		return nil, fmt.Errorf("ensure %s MachineOperation %s: %w", kind, key.Name, err)
	}

	if !adoptable(operation, s, kind) {
		r.fail(s, unboundedv1alpha3.FailureProviderConflict, fmt.Sprintf("MachineOperation %s exists and is not this allocation's %s", key.Name, kind))
		return nil, nil
	}

	return operation, nil
}

// adoptable reports whether operation is exactly the operation the
// controller would create for the allocation.
func adoptable(operation *unboundedv1alpha3.MachineOperation, s *allocationState, kind unboundedv1alpha3.OperationKind) bool {
	spec := &operation.Spec

	return spec.OperationKind == kind &&
		spec.MachineRef == s.names.machine &&
		sameUIDReference(spec.AllocationRef, s.reference()) &&
		len(spec.Parameters) == 0 &&
		spec.MachineSelector == nil &&
		spec.TTLSecondsAfterFinished == nil
}

func sameUIDReference(a, b *unboundedv1alpha3.UIDReference) bool {
	return a != nil && b != nil && a.Name == b.Name && a.UID == b.UID
}

// recordedOperation returns the recorded operation, or nil when it is
// missing or replaced. A cache miss or another UID is confirmed live.
func (r *MachineAllocationReconciler) recordedOperation(
	ctx context.Context,
	recorded *unboundedv1alpha3.UIDReference,
) (*unboundedv1alpha3.MachineOperation, error) {
	key := client.ObjectKey{Name: recorded.Name}

	var operation unboundedv1alpha3.MachineOperation

	err := r.Get(ctx, key, &operation)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get MachineOperation %s: %w", recorded.Name, err)
	}

	if err == nil && operation.UID == recorded.UID {
		return &operation, nil
	}

	operation = unboundedv1alpha3.MachineOperation{}
	if err := r.APIReader.Get(ctx, key, &operation); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("get MachineOperation %s: %w", recorded.Name, err)
	}

	if operation.UID != recorded.UID {
		return nil, nil
	}

	return &operation, nil
}

// allocateOperationMissing reports a recorded HostAllocate that is gone or
// replaced. The reference is kept and no other HostAllocate is created.
func (r *MachineAllocationReconciler) allocateOperationMissing(s *allocationState, recorded *unboundedv1alpha3.UIDReference) {
	message := fmt.Sprintf("recorded HostAllocate MachineOperation %s (%s) is missing or replaced; its provider effects need manual recovery",
		recorded.Name, recorded.UID)
	r.fail(s, unboundedv1alpha3.FailureProviderConflict, message)
	r.setCondition(s, unboundedv1alpha3.MachineAllocationConditionReleased, metav1.ConditionFalse, allocationReasonAllocateOperationMissing, message)
}

// observeAllocateOperation copies the recorded HostAllocate's state into the
// status and releases the input ConfigMaps once Begin was submitted or the
// operation finished.
func (r *MachineAllocationReconciler) observeAllocateOperation(
	ctx context.Context,
	s *allocationState,
	operation *unboundedv1alpha3.MachineOperation,
) error {
	result := operation.Status.Allocation

	switch operation.Status.Phase {
	case unboundedv1alpha3.OperationPhaseComplete:
		if result != nil {
			s.status.ProviderResourceID = result.ProviderResourceID
			s.status.InstanceType = valueOrDefault(result.InstanceType, s.status.InstanceType)
			s.status.Zone = valueOrDefault(result.Zone, s.status.Zone)
			s.status.Image = valueOrDefault(result.Image, s.status.Image)
			s.status.OwnedResources = slices.Clone(result.OwnedResources)
		}

		if !s.failed() {
			r.setCondition(s, unboundedv1alpha3.MachineAllocationConditionAllocated, metav1.ConditionTrue, allocationReasonAllocated,
				fmt.Sprintf("HostAllocate MachineOperation %s completed", operation.Name))
		}
	case unboundedv1alpha3.OperationPhaseFailed:
		if result != nil {
			s.status.OwnedResources = slices.Clone(result.OwnedResources)
		}

		reason, message := operationCompletion(operation)
		r.fail(s, allocationFailureReason(reason), fmt.Sprintf("HostAllocate MachineOperation %s failed: %s: %s", operation.Name, reason, message))
	default:
		if !s.failed() {
			r.setCondition(s, unboundedv1alpha3.MachineAllocationConditionAllocated, metav1.ConditionFalse, allocationReasonLaunching,
				fmt.Sprintf("waiting for HostAllocate MachineOperation %s", operation.Name))
		}
	}

	if !operation.Status.IsTerminal() && !hasProviderHandle(operation) {
		return nil
	}

	return r.unprotectInputs(ctx, s, s.status.Snapshot)
}

func hasProviderHandle(operation *unboundedv1alpha3.MachineOperation) bool {
	for i := range operation.Status.Targets {
		if operation.Status.Targets[i].ProviderOperation != nil {
			return true
		}
	}

	return false
}

func operationCompletion(operation *unboundedv1alpha3.MachineOperation) (string, string) {
	condition := apimeta.FindStatusCondition(operation.Status.Conditions, OperationConditionCompleted)
	if condition == nil {
		return "", operation.Status.Message
	}

	return condition.Reason, condition.Message
}

// allocationFailureReason maps the Completed reason of a failed HostAllocate
// to a failure category.
func allocationFailureReason(reason string) unboundedv1alpha3.MachineAllocationFailureReason {
	switch failure := unboundedv1alpha3.MachineAllocationFailureReason(reason); failure {
	case unboundedv1alpha3.FailureInsufficientCapacity,
		unboundedv1alpha3.FailureQuotaExceeded,
		unboundedv1alpha3.FailureInvalidRequest,
		unboundedv1alpha3.FailureAuthFailed,
		unboundedv1alpha3.FailureProviderConflict,
		unboundedv1alpha3.FailureProviderFailed:
		return failure
	}

	if strings.HasPrefix(reason, "Auth") || reason == allocationReasonAuthorityReplaced {
		return unboundedv1alpha3.FailureAuthFailed
	}

	switch reason {
	case allocationReasonFenced, allocationReasonNotRecorded, allocationReasonUnavailable, reasonRequestBuildFailed,
		"InvalidSpec", "InvalidHost", "InvalidProviderRef", reasonUnsupportedOperation:
		return unboundedv1alpha3.FailureInvalidRequest
	default:
		return unboundedv1alpha3.FailureProviderFailed
	}
}

// registerNode binds the Node named after the Machine when it registered
// with the allocation's providerID.
func (r *MachineAllocationReconciler) registerNode(ctx context.Context, s *allocationState) error {
	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: s.names.machine}, &node); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get Node %s: %w", s.names.machine, err)
		}

		r.setCondition(s, unboundedv1alpha3.MachineAllocationConditionRegistered, metav1.ConditionFalse, allocationReasonNodeNotRegistered,
			fmt.Sprintf("waiting for Node %s to register", s.names.machine))

		return nil
	}

	if node.Spec.ProviderID != s.status.ProviderID {
		r.setCondition(s, unboundedv1alpha3.MachineAllocationConditionRegistered, metav1.ConditionFalse, allocationReasonNodeConflict,
			fmt.Sprintf("Node %s has providerID %q, not %q", node.Name, node.Spec.ProviderID, s.status.ProviderID))

		return nil
	}

	if s.status.NodeRef == nil {
		s.status.NodeRef = &unboundedv1alpha3.UIDReference{Name: node.Name, UID: node.UID}
	}

	if node.Status.Allocatable != nil {
		s.status.Allocatable = node.Status.Allocatable.DeepCopy()
	}

	r.setCondition(s, unboundedv1alpha3.MachineAllocationConditionRegistered, metav1.ConditionTrue, allocationReasonNodeRegistered,
		fmt.Sprintf("Node %s registered", node.Name))

	return r.bindMachineNode(ctx, s, node.Name)
}

// bindMachineNode sets the Machine's nodeRef, which the daemon CSR approver
// requires for renewal.
func (r *MachineAllocationReconciler) bindMachineNode(ctx context.Context, s *allocationState, nodeName string) error {
	var machine unboundedv1alpha3.Machine
	if err := r.Get(ctx, client.ObjectKey{Name: s.names.machine}, &machine); err != nil {
		return client.IgnoreNotFound(err)
	}

	if !boundTo(&machine, s.allocation) {
		return nil
	}

	if machine.Spec.Kubernetes != nil && machine.Spec.Kubernetes.NodeRef != nil && machine.Spec.Kubernetes.NodeRef.Name == nodeName {
		return nil
	}

	updated := machine.DeepCopy()
	if updated.Spec.Kubernetes == nil {
		updated.Spec.Kubernetes = &unboundedv1alpha3.KubernetesSpec{}
	}

	updated.Spec.Kubernetes.NodeRef = &unboundedv1alpha3.LocalObjectReference{Name: nodeName}

	if err := r.Patch(ctx, updated, client.MergeFrom(&machine)); err != nil {
		return fmt.Errorf("set Machine %s nodeRef: %w", machine.Name, err)
	}

	return nil
}
