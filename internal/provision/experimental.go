// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package provision

import (
	"fmt"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// CheckIsolationEnabled returns an error when mode is experimental and the
// provisioner does not enable it. Every provisioner that writes an agent
// configuration (machina, metalman, machine-ops-controller) refuses to
// provision such a Machine, since the agent would refuse the mode without the
// Experimental.ContainerIsolation opt-in those provisioners write.
//
// component names the provisioner ("machina") and enable says how its operator
// turns the mode on ("set experimentalContainerIsolation: true in the machina
// configuration"); both only shape the message.
func CheckIsolationEnabled(mode v1alpha3.IsolationMode, containerEnabled bool, component, enable string) error {
	if mode == v1alpha3.IsolationContainer && !containerEnabled {
		return fmt.Errorf("spec.isolation %q is experimental and not enabled on this %s; %s to use it", mode, component, enable)
	}

	return nil
}
