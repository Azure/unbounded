// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

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
	// yet serving while a registration is still missing, unusable, or of
	// unknown state.
	//
	// Polling is necessary rather than lazy: readiness lives in Deployment
	// status and in Endpoints, and neither produces an event this operator
	// sees. The workload predicate deliberately filters status-only updates,
	// because reacting to them would re-apply every manifest on every pod
	// restart, and Endpoints are not watched at all.
	backendPollInterval = 15 * time.Second

	// backendIdlePollInterval is how often a pass re-checks a backend that is
	// not serving when every registration is already in place with the current
	// CA, so withholding changes nothing.
	//
	// The two intervals differ because the poll is not free. A cluster
	// component is planned on every Site request, so a RequeueAfter re-applies
	// net's whole manifest set once per Site per interval, indefinitely. At
	// backendPollInterval that is the right trade only while there is drift to
	// converge, which is a transient state on a rollout or a fresh install.
	// With nothing pending there is no work to do and no reason to pay for it
	// four times a minute forever, and "forever" is reachable: a Deployment
	// scaled to zero, a Deployment whose pods cannot be scheduled, and a leader
	// whose site controller never reports ready all leave the backend
	// permanently not serving with every registration already correct.
	backendIdlePollInterval = 2 * time.Minute
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
	// certificate. It is nil until the controller has published one, and on
	// every readErr path, which discards it even where the ConfigMap read had
	// already succeeded. Nothing reads it in that case: registrationVerdict
	// returns on readErr before it is needed.
	caBundle []byte

	// ready reports that a backend is actually serving: the current Deployment
	// spec has rolled out and the Service has an endpoint behind it.
	ready bool

	// reason names whichever precondition is unmet, for the Site condition: the
	// serving CA published, the Deployment present, its rollout finished, an
	// endpoint registered. It is empty when readErr is set, because a state
	// that could not be read has no precondition to name and registrationVerdict
	// reports the error itself instead.
	reason string

	// readErr is set when the answer could not be established at all, as
	// opposed to established as "not serving".
	//
	// The distinction matters to the Site condition and to whether the pass is
	// a reconcile error, but not to what the pass emits: an unknown backend is
	// withheld from exactly as a known-down one is. See readBackendState for
	// why a failed read is not allowed to fail the plan.
	readErr error
}

// readBackendState reports whether the net controller can serve the traffic its
// registrations would send it.
//
// This exists because ordering is not readiness. tierActivation already runs
// the registrations after the Deployment they point at, but "after" only means
// the apply call returned: a Deployment object accepted by the apiserver has no
// pods yet. Registering a failurePolicy: Ignore webhook against a backend that
// is not listening does not fail loudly, it silently stops enforcing, and an
// APIService in the same state makes the aggregated API return errors for a
// type the cluster believes is served.
//
// It answers for the state the previous pass left behind, which is what a plan
// is entitled to read. The controller's serving CA is persisted in the
// unbounded-net-serving-cert Secret with a ten-year lifetime, so a rollout does
// not change the value stamped into a registration; the CA ConfigMap watch and
// backendPollInterval close the remaining gap in one pass.
//
// Every read goes through LiveReader. The cache is the wrong source twice over:
// it lags the apiserver, so a rollout that has just started still reads as the
// settled previous one, and reading endpoints and pods through it would cache
// every one of them in the namespace to answer a question asked once per pass.
//
// It returns no error. A read it cannot complete becomes backendState.readErr
// and leaves ready false, because a failure to answer must not stop net
// reconciling. The endpoint, pod and ReplicaSet reads are of objects the
// operator neither owns nor applies, over RBAC granted separately from the
// rules it writes with, so a 403 during an upgrade whose ClusterRole has not
// landed yet is a real and recoverable state. Returning an error here would
// abort the whole plan, taking the ConfigMap, the Deployment and the node
// DaemonSet with it and leaving the dataplane unable to converge on a question
// that only governs three registrations. Withholding is the safe direction on
// an unknown; refusing to write anything is not.
func readBackendState(ctx context.Context, env *component.Env) backendState {
	reader := env.LiveReader()

	var configMap corev1.ConfigMap

	caKey := client.ObjectKey{Namespace: env.Namespace, Name: servingCAName}

	switch err := reader.Get(ctx, caKey, &configMap); {
	case apierrors.IsNotFound(err):
		return backendState{reason: "the controller has not published its serving CA yet"}
	case err != nil:
		return backendState{readErr: fmt.Errorf("get net serving CA %s/%s: %w", caKey.Namespace, caKey.Name, err)}
	}

	ca := []byte(configMap.Data[servingCAKey])
	if len(ca) == 0 {
		return backendState{reason: "the serving CA ConfigMap carries no " + servingCAKey}
	}

	var deployment appsv1.Deployment

	deployKey := client.ObjectKey{Namespace: env.Namespace, Name: controllerName}

	switch err := reader.Get(ctx, deployKey, &deployment); {
	case apierrors.IsNotFound(err):
		return backendState{caBundle: ca, reason: "the controller Deployment does not exist yet"}
	case err != nil:
		return backendState{readErr: fmt.Errorf("get net controller %s/%s: %w", deployKey.Namespace, deployKey.Name, err)}
	}

	if reason, rolled := rolloutComplete(&deployment); !rolled {
		return backendState{caBundle: ca, reason: reason}
	}

	serving, err := serviceHasEndpoint(ctx, reader, env.Namespace, &deployment)
	if err != nil {
		return backendState{readErr: err}
	}

	if !serving {
		// The Service has no selector: its leader pod writes the Endpoints and
		// EndpointSlice itself, and only once it has both won the lease and
		// finished starting its site controller. So this is not a restatement
		// of the Deployment check, it is the difference between a pod that is
		// running and a pod that has taken leadership and is answering.
		return backendState{caBundle: ca, reason: "no endpoint is registered for the controller Service"}
	}

	return backendState{caBundle: ca, ready: true}
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
	// what distinguishes "an old replica is still serving" from "the spec the
	// Deployment currently declares is serving". A registration is only worth
	// creating against a backend that has finished becoming the backend.
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
// EndpointSlice is authoritative on every supported version, so a slice that
// exists settles the question either way. The legacy Endpoints object is
// consulted only when no slice exists at all: the controller writes both, and
// an apiserver old enough to resolve an APIService through Endpoints alone is
// exactly the case it keeps writing that one for.
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

// hasHTTPSPort and hasLegacyHTTPSPort identify the controller's serving port by
// name.
//
// The number is not looked at. The controller publishes controller.healthPort
// from its own config, while the name is a literal set by the same code, so the
// name is the half that cannot drift. Matching the number would turn any
// healthPort override into a backend that never reads as serving, which
// withholds the registrations permanently and pins NetReady False. An
// EndpointSlice port may also be nil, which the API defines as unrestricted, so
// requiring one to be present would reject a legal shape for the sake of a
// value that is not compared.
//
// That the registration manifests still hardcode 9999 in their clientConfig is
// a separate inconsistency, tracked in Azure/unbounded#628.
func hasHTTPSPort(ports []discoveryv1.EndpointPort) bool {
	for _, port := range ports {
		if port.Name != nil && *port.Name == "https" &&
			(port.Protocol == nil || *port.Protocol == corev1.ProtocolTCP) {
			return true
		}
	}

	return false
}

func hasLegacyHTTPSPort(ports []corev1.EndpointPort) bool {
	for _, port := range ports {
		if port.Name == "https" && port.Protocol == corev1.ProtocolTCP {
			return true
		}
	}

	return false
}

// endpointTargetsReadyPod reports whether an endpoint address is a live pod of
// the current controller Deployment.
//
// A nil targetRef is accepted rather than rejected, on the strength of the
// rollout check the caller has already passed. Controllers released before the
// operator gated on backend readiness publish their Endpoints and EndpointSlice
// without one, and a workload override may pin the controller image to such a
// version indefinitely. Rejecting those would leave the registrations frozen at
// whatever the cluster already had, silently: nothing would be pending, so
// NetReady would stay true while manifest changes were never applied. What
// rolloutComplete has established by this point is that the Deployment's current
// revision is fully Ready and Available, so an address published against it is a
// running pod; the targetRef checks below only add that it belongs to that
// revision rather than being a stale record of the last one.
func endpointTargetsReadyPod(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	deployment *appsv1.Deployment,
	target *corev1.ObjectReference,
	addresses []string,
) (bool, error) {
	if target == nil {
		return len(addresses) > 0, nil
	}

	if target.Kind != "Pod" || target.Name == "" || target.UID == "" ||
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

// pendingRegistrations names the registrations that are not already usable in
// the cluster: absent, or carrying a caBundle that is empty or no longer the
// published CA.
//
// It is the input to registrationVerdict's reporting decision rather than the
// decision itself; see there for why a withheld registration that is already in
// place is not worth reporting. A registration that exists with an empty
// caBundle counts as pending because that is the broken state this gate exists
// to prevent, and it should stay visible until the backend comes back and the
// apply can fix it.
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
// populated and, when expectedCA is non-empty, matches it. A registration is
// only as usable as its emptiest or stalest bundle.
func hasCABundle(obj *unstructured.Unstructured, expectedCA []byte) bool {
	matches := func(encoded string) bool {
		if encoded == "" {
			return false
		}

		if len(expectedCA) == 0 {
			return true
		}

		decoded, err := base64.StdEncoding.DecodeString(encoded)

		return err == nil && bytes.Equal(decoded, expectedCA)
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

		// An absent or non-string bundle is not a usable one, so the comma-ok
		// result is checked rather than discarded.
		bundle, ok := clientConfig["caBundle"].(string)
		if !ok || !matches(bundle) {
			return false
		}
	}

	return true
}
