// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineconfigs

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// ResolveVersionFromRef returns the MachineConfigurationVersion referenced by
// ref. If ref omits Version, the highest version for the named
// MachineConfiguration is returned.
func ResolveVersionFromRef(
	ctx context.Context,
	c client.Client,
	ref *v1alpha3.MachineConfigurationRef,
) (*v1alpha3.MachineConfigurationVersion, error) {
	if ref.Version != nil {
		var mcv v1alpha3.MachineConfigurationVersion

		name := v1alpha3.MachineConfigurationVersionName(ref.Name, *ref.Version)
		if err := c.Get(ctx, client.ObjectKey{Name: name}, &mcv); err != nil {
			return nil, fmt.Errorf("get MachineConfigurationVersion %s: %w", name, err)
		}

		return &mcv, nil
	}

	return ResolveLatestVersion(ctx, c, ref.Name)
}

// ResolveLatestVersion returns the highest MachineConfigurationVersion for the
// named MachineConfiguration.
func ResolveLatestVersion(
	ctx context.Context,
	c client.Client,
	configurationName string,
) (*v1alpha3.MachineConfigurationVersion, error) {
	var list v1alpha3.MachineConfigurationVersionList
	if err := c.List(ctx, &list, client.MatchingLabels{
		v1alpha3.MCVConfigurationLabelKey: configurationName,
	}); err != nil {
		return nil, fmt.Errorf("list MachineConfigurationVersions for %s: %w", configurationName, err)
	}

	if len(list.Items) == 0 {
		return nil, apierrors.NewNotFound(
			v1alpha3.GroupVersion.WithResource("machineconfigurationversions").GroupResource(),
			configurationName,
		)
	}

	latest := list.Items[0]
	for i := 1; i < len(list.Items); i++ {
		if list.Items[i].Spec.Version > latest.Spec.Version {
			latest = list.Items[i]
		}
	}

	return &latest, nil
}
