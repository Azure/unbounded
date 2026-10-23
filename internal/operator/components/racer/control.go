// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	manifests "github.com/Azure/unbounded/deploy/racer"
	"github.com/Azure/unbounded/internal/operator/component"
)

const (
	stateName   = "racer-state"
	webhookName = "racer-webhook"

	// stateUIDAnnotation records, on the dataplane DaemonSet and
	// ServiceAccount, the racer-state Secret the installation runs on. A
	// missing or replaced Secret then fails closed instead of silently
	// starting a new cluster identity.
	stateUIDAnnotation = "unbounded-cloud.io/racer-state-uid"
)

func objectMeta(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name} }

// controlState is what planning learned about the runtime state Secret.
type controlState struct {
	// uid is empty when racer-state does not exist yet.
	uid string
}

// planControl adds the webhook Service and creates the state Secret, Leases,
// and EndpointSlice when absent. Existing runtime-owned objects are never
// written. It fails closed when racer-state is gone or replaced after the
// installation recorded it.
func planControl(ctx context.Context, env *component.Env, plan *component.Plan, recorded string) (controlState, error) {
	objects, err := env.DecodeManifestFiles(manifests.Manifests, []string{"control.yaml"}, nil)
	if err != nil {
		return controlState{}, err
	}

	var state controlState

	for _, obj := range objects {
		if obj.GetKind() == "Service" {
			op, err := ownedOperation(ctx, env, component.Operation{Kind: component.OpApply, Object: obj, Component: name})
			if err != nil {
				return controlState{}, err
			}

			plan.Add(op)

			continue
		}

		current := obj.DeepCopy()

		err := env.LiveReader().Get(ctx, client.ObjectKeyFromObject(obj), current)
		if apierrors.IsNotFound(err) {
			if obj.GetKind() == "Secret" && recorded != "" {
				return controlState{}, fmt.Errorf("secret %s holding the Racer cluster identity %s is missing; restore it, or delete both DaemonSet and ServiceAccount %s to start a new cluster identity", stateName, recorded, dataplaneName)
			}

			plan.Add(component.Operation{Kind: component.OpCreateIfAbsent, Object: obj, Component: name})

			continue
		}

		if err != nil {
			return controlState{}, err
		}

		if err := validateAdoptable(ctx, env, current); err != nil {
			return controlState{}, err
		}

		if obj.GetKind() == "Secret" {
			state.uid = string(current.GetUID())
			if recorded != "" && recorded != state.uid {
				return controlState{}, fmt.Errorf("secret %s was replaced (UID %s, installation recorded %s); restore it, or delete both DaemonSet and ServiceAccount %s to start a new cluster identity", stateName, state.uid, recorded, dataplaneName)
			}
		}
	}

	if err := planEndpointSlice(ctx, env, plan); err != nil {
		return controlState{}, err
	}

	return state, nil
}

// planEndpointSlice creates the empty racer-webhook EndpointSlice. A slice
// holds one address family, and the address type is immutable, so it follows
// the primary family of the selector-less Service: the family of its
// ClusterIP, which the API server dials for the webhook. That family may
// differ from status.podIP, for example on dual-stack hostNetwork Pods, so
// seats serve the webhook on each Pod IP family and the primary publishes the
// one matching this slice. It waits for the Service to exist.
func planEndpointSlice(ctx context.Context, env *component.Env, plan *component.Plan) error {
	current := &discoveryv1.EndpointSlice{}

	err := env.LiveReader().Get(ctx, objectKey(env, webhookName), current)
	if err == nil {
		current.SetGroupVersionKind(discoveryv1.SchemeGroupVersion.WithKind("EndpointSlice"))
		return validateAdoptable(ctx, env, current)
	}

	if !apierrors.IsNotFound(err) {
		return err
	}

	service := &corev1.Service{}

	err = env.LiveReader().Get(ctx, objectKey(env, webhookName), service)
	if apierrors.IsNotFound(err) {
		return nil
	}

	if err != nil {
		return err
	}

	family := discoveryv1.AddressTypeIPv4
	if len(service.Spec.IPFamilies) != 0 && service.Spec.IPFamilies[0] == corev1.IPv6Protocol {
		family = discoveryv1.AddressTypeIPv6
	}

	slice := &discoveryv1.EndpointSlice{
		TypeMeta: metav1.TypeMeta{APIVersion: discoveryv1.SchemeGroupVersion.String(), Kind: "EndpointSlice"},
		ObjectMeta: metav1.ObjectMeta{
			Name: webhookName, Namespace: env.Namespace,
			Labels: map[string]string{
				discoveryv1.LabelServiceName: webhookName,
				// Keeps the EndpointSlice controller and mirroring away.
				discoveryv1.LabelManagedBy: "racer.unbounded-cloud.io",
				managerLabel:               component.FieldOwner,
			},
		},
		AddressType: family,
		Ports:       []discoveryv1.EndpointPort{{Name: ptr.To("webhook"), Port: ptr.To(int32(9443)), Protocol: ptr.To(corev1.ProtocolTCP)}},
		Endpoints:   []discoveryv1.Endpoint{},
	}

	plan.Add(component.Operation{Kind: component.OpCreateIfAbsent, Object: component.ToUnstructured(slice), Component: name})

	return nil
}
