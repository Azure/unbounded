// Package phase derives the human-readable Machine Phase from conditions
// and operation counters. The Phase field is intended only for human
// consumption; it must never be used to drive controller logic.
package phase

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha3 "github.com/project-unbounded/unbounded-kube/api/v1alpha3"
)

// Derive computes the Phase for a Machine based on its conditions and
// operation counters. It returns the derived phase and a human-readable
// message. The result is purely informational and must not be used in
// controller decision logic.
func Derive(machine *v1alpha3.Machine) (v1alpha3.MachinePhase, string) {
	specOps := machine.Spec.Operations
	statusOps := machine.Status.Operations

	var specReboot, specReimage, statusReboot, statusReimage int64
	if specOps != nil {
		specReboot = specOps.RebootCounter
		specReimage = specOps.ReimageCounter
	}

	if statusOps != nil {
		statusReboot = statusOps.RebootCounter
		statusReimage = statusOps.ReimageCounter
	}

	pendingReimage := specReimage > statusReimage
	pendingReboot := specReboot > statusReboot

	// A Reimaged=False condition means the machine was rebooted into PXE
	// and is currently installing the image. We reuse the Provisioning
	// phase since reimaging is semantically identical to provisioning
	// from a human-readable perspective.
	reimagedCond := meta.FindStatusCondition(machine.Status.Conditions, v1alpha3.MachineConditionReimaged)
	if reimagedCond != nil && reimagedCond.Status == metav1.ConditionFalse {
		return v1alpha3.MachinePhaseProvisioning, "PXE booting into new image"
	}

	// A pending reboot means the Redfish controller is cycling power.
	if pendingReboot {
		if pendingReimage {
			return v1alpha3.MachinePhaseRebooting, "Rebooting for reimage"
		}

		return v1alpha3.MachinePhaseRebooting, "Rebooting"
	}

	// No metalman-specific operation in progress. If the phase was
	// previously set to a metalman-owned value, transition to Pending
	// so the machina controller can take over.
	//
	// Rebooting is always metalman-owned.
	//
	// Provisioning is shared: metalman sets it during PXE reimaging
	// (signalled by a Reimaged condition) and machina sets it during
	// SSH provisioning. When the Reimaged condition is True the PXE
	// reimage has completed and we should hand off to machina by
	// transitioning to Pending. If the Reimaged condition is absent,
	// machina owns the Provisioning phase and we must not touch it.
	switch machine.Status.Phase {
	case v1alpha3.MachinePhaseRebooting:
		return v1alpha3.MachinePhasePending, "Operation complete"
	case v1alpha3.MachinePhaseProvisioning:
		if reimagedCond != nil && reimagedCond.Status == metav1.ConditionTrue {
			return v1alpha3.MachinePhasePending, "Operation complete"
		}

		return machine.Status.Phase, machine.Status.Message
	default:
		return machine.Status.Phase, machine.Status.Message
	}
}

// Set updates the Phase and Message on the machine status in-place.
// Callers should invoke this immediately before persisting a status update.
func Set(machine *v1alpha3.Machine) {
	machine.Status.Phase, machine.Status.Message = Derive(machine)
}
