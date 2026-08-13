// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package net

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"slices"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	apiregistrationv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/operator/component"
)

const (
	// servingCAName is the ConfigMap the controller publishes its self-signed
	// serving CA into, and servingCAKey is the PEM inside it.
	servingCAName = "unbounded-net-serving-ca"
	servingCAKey  = "ca.crt"

	// backendPollInterval is how often a pass re-checks a backend that is not
	// yet serving.
	//
	// Polling is necessary rather than lazy: readiness lives in Deployment
	// status and in Endpoints, and neither produces an event this operator
	// sees. The workload predicate deliberately filters status-only updates,
	// because reacting to them would re-apply every manifest on every pod
	// restart, and Endpoints are not watched at all.
	backendPollInterval = 15 * time.Second
)

// registrations identifies the objects that tell the apiserver to send traffic
// to the net controller Service.
//
// The names are duplicated from the manifests rather than decoded out of them
// because the gate has to look these up in the cluster before it applies
// anything. TestRegistrationIdentitiesMatchTheManifests fails if the two ever
// disagree.
var registrations = []struct {
	gvk  schema.GroupVersionKind
	name string
}{
	{
		gvk:  admissionregistrationv1.SchemeGroupVersion.WithKind("ValidatingWebhookConfiguration"),
		name: "unbounded-net-validating-webhook",
	},
	{
		gvk:  admissionregistrationv1.SchemeGroupVersion.WithKind("MutatingWebhookConfiguration"),
		name: "unbounded-net-mutating-webhook",
	},
	{
		gvk:  apiregistrationv1.SchemeGroupVersion.WithKind("APIService"),
		name: "v1alpha1.status.net.unbounded-cloud.io",
	},
}

// isBackendRegistration reports whether obj tells the apiserver to send traffic
// to the net controller Service.
//
// The ValidatingAdmissionPolicies are deliberately not included. They are
// evaluated inside the apiserver with no backend to reach, so gating them on a
// running controller would withhold enforcement for no reason.
func isBackendRegistration(obj *unstructured.Unstructured) bool {
	switch obj.GetKind() {
	case "ValidatingWebhookConfiguration", "MutatingWebhookConfiguration", "APIService":
		return true
	default:
		return false
	}
}

// backendState is what a pass knows about the controller the registrations
// point at.
type backendState struct {
	// caBundle is the serving CA the apiserver needs to verify the controller's
	// certificate, nil when the controller has not published one yet.
	caBundle []byte

	// ready reports that a backend is actually serving: the current Deployment
	// spec has rolled out and the Service has an endpoint behind it.
	ready bool

	// reason says which of those is missing, for the Site condition.
	reason string
}

// readBackendState reports whether the net controller can serve the traffic its
// registrations would send it.
//
// This exists because applying in order is not readiness. The manifests are
// applied in filename order, so the Deployment goes out before the
// registrations, but "before" only means the apply call returned: a Deployment
// object accepted by the apiserver has no pods yet. Registering a
// failurePolicy: Ignore webhook against a backend that is not listening does
// not fail loudly, it silently stops enforcing, and an APIService in the same
// state makes the aggregated API return errors for a type the cluster believes
// is served.
//
// Every read goes through LiveReader. The cache cannot answer this: it is
// populated by a watch, so immediately after the same pass applied the
// Deployment it still holds the previous generation, and the gate would read
// the outgoing pod's availability as the incoming one's.
func readBackendState(ctx context.Context, env *component.Env) (backendState, error) {
	reader := env.LiveReader()

	var configMap corev1.ConfigMap

	caKey := client.ObjectKey{Namespace: env.Namespace, Name: servingCAName}

	switch err := reader.Get(ctx, caKey, &configMap); {
	case apierrors.IsNotFound(err):
		return backendState{reason: "the controller has not published its serving CA yet"}, nil
	case err != nil:
		return backendState{}, fmt.Errorf("get net serving CA %s/%s: %w", caKey.Namespace, caKey.Name, err)
	}

	ca := []byte(configMap.Data[servingCAKey])
	if len(ca) == 0 {
		return backendState{reason: "the serving CA ConfigMap carries no " + servingCAKey}, nil
	}

	var deployment appsv1.Deployment

	deployKey := client.ObjectKey{Namespace: env.Namespace, Name: controllerName}

	switch err := reader.Get(ctx, deployKey, &deployment); {
	case apierrors.IsNotFound(err):
		return backendState{caBundle: ca, reason: "the controller Deployment does not exist yet"}, nil
	case err != nil:
		return backendState{}, fmt.Errorf("get net controller %s/%s: %w", deployKey.Namespace, deployKey.Name, err)
	}

	if reason, rolled := rolloutComplete(&deployment); !rolled {
		return backendState{caBundle: ca, reason: reason}, nil
	}

	serving, err := serviceHasEndpoint(ctx, reader, env.Namespace, &deployment)
	if err != nil {
		return backendState{}, err
	}

	if !serving {
		// The Service has no selector: its leader pod writes the Endpoints and
		// EndpointSlice itself when it wins the lease. So this is not a
		// restatement of the Deployment check, it is the difference between a
		// pod that is running and a pod that has taken leadership and is
		// answering.
		return backendState{caBundle: ca, reason: "no endpoint is registered for the controller Service"}, nil
	}

	return backendState{caBundle: ca, ready: true}, nil
}

// rolloutComplete reports whether a Deployment's current spec is the one that
// is running, naming what is outstanding when it is not.
func rolloutComplete(deployment *appsv1.Deployment) (string, bool) {
	desired := int32(1)
	if deployment.Spec.Replicas != nil {
		desired = *deployment.Spec.Replicas
	}

	if desired == 0 {
		return "the controller Deployment is scaled to zero", false
	}

	if deployment.Status.ObservedGeneration < deployment.Generation {
		return "the controller Deployment has not been observed at its current generation", false
	}

	// UpdatedReplicas counts pods running the current pod template, so this is
	// what distinguishes "an old replica is still serving" from "the spec this
	// pass wrote is serving". Registration carries the CA and the Service
	// reference that go with the new spec, so the old pod's availability is
	// not the question.
	if deployment.Status.Replicas != desired ||
		deployment.Status.UpdatedReplicas != desired ||
		deployment.Status.ReadyReplicas != desired ||
		deployment.Status.AvailableReplicas != desired {
		return fmt.Sprintf("the controller Deployment is rolling out (%d/%d replicas, %d updated, %d ready, %d available)",
			deployment.Status.Replicas, desired, deployment.Status.UpdatedReplicas,
			deployment.Status.ReadyReplicas, deployment.Status.AvailableReplicas), false
	}

	return "", true
}

// serviceHasEndpoint reports whether anything is registered behind the
// controller Service.
//
// EndpointSlice is authoritative on every supported version, but the legacy
// Endpoints object is checked too: the controller writes both, and an
// apiserver old enough to resolve an APIService through Endpoints alone is
// exactly the case the controller keeps writing it for.

func serviceHasEndpoint(ctx context.Context, reader client.Reader, namespace string, deployment *appsv1.Deployment) (bool, error) {
	key := client.ObjectKey{Namespace: namespace, Name: controllerName}

	var endpointSlice discoveryv1.EndpointSlice

	switch err := reader.Get(ctx, key, &endpointSlice); {
	case err == nil:
		if endpointSlice.Labels[discoveryv1.LabelServiceName] != controllerName || !hasHTTPSPort(endpointSlice.Ports) {
			return false, nil
		}

		for _, endpoint := range endpointSlice.Endpoints {
			// A nil Ready condition means ready, per the EndpointSlice API.
			if len(endpoint.Addresses) == 0 || (endpoint.Conditions.Ready != nil && !*endpoint.Conditions.Ready) {
				continue
			}

			ready, err := endpointTargetsReadyPod(ctx, reader, namespace, deployment, endpoint.TargetRef, endpoint.Addresses)
			if err != nil {
				return false, err
			}

			if ready {
				return true, nil
			}
		}

		return false, nil
	case !apierrors.IsNotFound(err):
		return false, fmt.Errorf("get endpoint slice %s/%s: %w", namespace, controllerName, err)
	}

	var endpoints corev1.Endpoints //nolint:staticcheck // the controller still writes it for APIService availability on Kubernetes 1.33 and earlier

	switch err := reader.Get(ctx, key, &endpoints); {
	case apierrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("get endpoints %s/%s: %w", namespace, controllerName, err)
	}

	for _, subset := range endpoints.Subsets {
		if !hasLegacyHTTPSPort(subset.Ports) {
			continue
		}

		for _, address := range subset.Addresses {
			ready, err := endpointTargetsReadyPod(ctx, reader, namespace, deployment, address.TargetRef, []string{address.IP})
			if err != nil {
				return false, err
			}

			if ready {
				return true, nil
			}
		}
	}

	return false, nil
}

func hasHTTPSPort(ports []discoveryv1.EndpointPort) bool {
	for _, port := range ports {
		if port.Name != nil && *port.Name == "https" && port.Port != nil && *port.Port == 9999 &&
			(port.Protocol == nil || *port.Protocol == corev1.ProtocolTCP) {
			return true
		}
	}

	return false
}

func hasLegacyHTTPSPort(ports []corev1.EndpointPort) bool {
	for _, port := range ports {
		if port.Name == "https" && port.Port == 9999 && port.Protocol == corev1.ProtocolTCP {
			return true
		}
	}

	return false
}

func endpointTargetsReadyPod(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	deployment *appsv1.Deployment,
	target *corev1.ObjectReference,
	addresses []string,
) (bool, error) {
	if target == nil || target.Kind != "Pod" || target.Name == "" || target.UID == "" ||
		(target.Namespace != "" && target.Namespace != namespace) {
		return false, nil
	}

	var pod corev1.Pod

	key := client.ObjectKey{Namespace: namespace, Name: target.Name}

	switch err := reader.Get(ctx, key, &pod); {
	case apierrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("get endpoint target pod %s/%s: %w", namespace, target.Name, err)
	}

	if pod.UID != target.UID || pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning ||
		!slices.Contains(addresses, pod.Status.PodIP) {
		return false, nil
	}

	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err != nil {
		return false, fmt.Errorf("parse controller Deployment selector: %w", err)
	}

	if !selector.Matches(labels.Set(pod.Labels)) {
		return false, nil
	}

	podOwner := metav1.GetControllerOf(&pod)
	if podOwner == nil || podOwner.Kind != "ReplicaSet" || podOwner.Name == "" || podOwner.UID == "" {
		return false, nil
	}

	var replicaSet appsv1.ReplicaSet

	rsKey := client.ObjectKey{Namespace: namespace, Name: podOwner.Name}

	switch err := reader.Get(ctx, rsKey, &replicaSet); {
	case apierrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("get endpoint target ReplicaSet %s/%s: %w", namespace, podOwner.Name, err)
	}

	deploymentOwner := metav1.GetControllerOf(&replicaSet)
	if replicaSet.UID != podOwner.UID || deploymentOwner == nil || deploymentOwner.Kind != "Deployment" ||
		deploymentOwner.Name != deployment.Name || deploymentOwner.UID != deployment.UID {
		return false, nil
	}

	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue, nil
		}
	}

	return false, nil
}

// stampCABundle writes the serving CA into a registration.
//
// The operator has to do this because it now decides when the registration is
// created. The controller injects the CA once, at startup, and never retries;
// before this gate existed the registration was always created first, so that
// single attempt found it. A registration created after the controller has
// already started would keep an empty caBundle forever, which fails TLS
// verification: the webhooks would silently stop enforcing and the aggregated
// API would stay unavailable.
//
// The value is base64 in the serialized form, because CABundle is []byte in
// the typed API.
func stampCABundle(obj *unstructured.Unstructured, ca []byte) error {
	encoded := base64.StdEncoding.EncodeToString(ca)

	if obj.GetKind() == "APIService" {
		if err := unstructured.SetNestedField(obj.Object, encoded, "spec", "caBundle"); err != nil {
			return fmt.Errorf("set APIService caBundle: %w", err)
		}

		return nil
	}

	webhooks, found, err := unstructured.NestedSlice(obj.Object, "webhooks")
	if err != nil {
		return fmt.Errorf("get %s webhooks: %w", obj.GetName(), err)
	}

	if !found {
		return fmt.Errorf("%s %s declares no webhooks", obj.GetKind(), obj.GetName())
	}

	for i := range webhooks {
		webhook, ok := webhooks[i].(map[string]any)
		if !ok {
			return fmt.Errorf("%s webhooks[%d] is not an object", obj.GetName(), i)
		}

		clientConfig, ok := webhook["clientConfig"].(map[string]any)
		if !ok {
			return fmt.Errorf("%s webhooks[%d] has no clientConfig", obj.GetName(), i)
		}

		clientConfig["caBundle"] = encoded
	}

	if err := unstructured.SetNestedSlice(obj.Object, webhooks, "webhooks"); err != nil {
		return fmt.Errorf("set %s webhooks: %w", obj.GetName(), err)
	}

	return nil
}

// pendingRegistrations names the registrations that are being withheld and are
// not already usable in the cluster.
//
// It decides whether an unready backend is worth reporting, which is a separate
// question from whether to withhold. Withholding a registration that does not
// exist yet is a real difference between desired and actual state and the Site
// should say so. Withholding one that is already in place with a CA changes
// nothing, and reporting it would turn NetReady False for the duration of every
// net rollout, because the controller is host-networked with maxSurge: 0 and is
// therefore briefly unavailable by design on every upgrade.
//
// A registration that exists with an empty caBundle counts as pending: that is
// the broken state this gate exists to prevent, and it should be visible until
// the backend comes back and the apply can fix it.
func pendingRegistrations(ctx context.Context, env *component.Env, expectedCA []byte) ([]string, error) {
	var pending []string

	for _, registration := range registrations {
		live := &unstructured.Unstructured{}
		live.SetGroupVersionKind(registration.gvk)

		// Registrations are cluster-scoped, so the key carries no namespace.
		key := client.ObjectKey{Name: registration.name}

		switch err := env.LiveReader().Get(ctx, key, live); {
		case apierrors.IsNotFound(err):
			pending = append(pending, registration.name)

			continue
		case err != nil:
			return nil, fmt.Errorf("get %s %s: %w", registration.gvk.Kind, registration.name, err)
		}

		if !hasCABundle(live, expectedCA) {
			pending = append(pending, registration.name)
		}
	}

	return pending, nil
}

// hasCABundle reports whether every CA bundle a registration carries is
// populated and, when expectedCA is available, matches it. A registration is
// only as usable as its emptiest or stale bundle.
func hasCABundle(obj *unstructured.Unstructured, expectedCA ...[]byte) bool {
	matches := func(encoded string) bool {
		if encoded == "" {
			return false
		}

		if len(expectedCA) == 0 || len(expectedCA[0]) == 0 {
			return true
		}

		decoded, err := base64.StdEncoding.DecodeString(encoded)

		return err == nil && bytes.Equal(decoded, expectedCA[0])
	}

	if obj.GetKind() == "APIService" {
		bundle, _, err := unstructured.NestedString(obj.Object, "spec", "caBundle")

		return err == nil && matches(bundle)
	}

	webhooks, found, err := unstructured.NestedSlice(obj.Object, "webhooks")
	if err != nil || !found || len(webhooks) == 0 {
		return false
	}

	for i := range webhooks {
		webhook, ok := webhooks[i].(map[string]any)
		if !ok {
			return false
		}

		clientConfig, ok := webhook["clientConfig"].(map[string]any)
		if !ok {
			return false
		}

		bundle, _ := clientConfig["caBundle"].(string) //nolint:errcheck // an absent or non-string bundle is not a usable one
		if !matches(bundle) {
			return false
		}
	}

	return true
}
