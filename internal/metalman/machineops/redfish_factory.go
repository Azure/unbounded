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

	var secret corev1.Secret
	if err := f.Reader.Get(ctx, types.NamespacedName{Name: rf.PasswordRef.Name, Namespace: rf.PasswordRef.Namespace}, &secret); err != nil {
		return nil, fmt.Errorf("get Redfish password secret: %w", err)
	}

	passwordBytes, ok := secret.Data[rf.PasswordRef.Key]
	if !ok {
		return nil, fmt.Errorf("redfish password secret %s/%s missing key %q", rf.PasswordRef.Namespace, rf.PasswordRef.Name, rf.PasswordRef.Key)
	}

	password := string(passwordBytes)

	c, err := f.Pool.Get(ctx, rf.URL, fingerprint, rf.Username, password, rf.DeviceID)
	if err != nil {
		return nil, err
	}

	return c, nil
}
