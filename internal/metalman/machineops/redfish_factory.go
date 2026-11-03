// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/metalman/redfish"
)

// RedfishPowerClientFactory creates PowerClients backed by the shared Redfish pool.
type RedfishPowerClientFactory struct {
	Reader client.Reader
	Pool   *redfish.Pool
}

func (f *RedfishPowerClientFactory) ForMachine(ctx context.Context, machine *v1alpha3.Machine) (PowerClient, error) {
	if machine.Spec.Netboot() == nil || machine.Spec.Netboot().Redfish == nil {
		return nil, fmt.Errorf("machine %s has no Redfish config", machine.Name)
	}

	rf := machine.Spec.Netboot().Redfish

	fingerprint := ""
	if machine.Status.Redfish != nil {
		fingerprint = machine.Status.Redfish.CertFingerprint
	}

	if fingerprint == "" {
		return nil, fmt.Errorf("machine %s has no Redfish certificate fingerprint", machine.Name)
	}

	password, err := redfishPassword(ctx, f.Reader, rf.PasswordRef)
	if err != nil {
		return nil, err
	}

	c, err := f.Pool.Get(ctx, rf.URL, fingerprint, rf.Username, password, rf.DeviceID)
	if err != nil {
		return nil, err
	}

	return c, nil
}

// redfishPassword reads the Redfish password from the Secret that ref selects.
// An empty key falls back to v1alpha3.DefaultRedfishPasswordKey, matching the
// CRD default.
func redfishPassword(ctx context.Context, reader client.Reader, ref v1alpha3.RedfishPasswordSelector) (string, error) {
	var secret corev1.Secret
	if err := reader.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: ref.Namespace}, &secret); err != nil {
		return "", fmt.Errorf("get Redfish password secret: %w", err)
	}

	key := ref.KeyOrDefault()

	password, ok := secret.Data[key]
	if !ok {
		return "", fmt.Errorf("redfish password secret %s/%s missing key %q; set passwordRef.key to the key that holds the password", ref.Namespace, ref.Name, key)
	}

	return string(password), nil
}
