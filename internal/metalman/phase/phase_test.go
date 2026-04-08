package phase

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha3 "github.com/project-unbounded/unbounded-kube/api/v1alpha3"
)

func TestDeriveRebooting(t *testing.T) {
	machine := &v1alpha3.Machine{
		Spec: v1alpha3.MachineSpec{
			Operations: &v1alpha3.OperationsSpec{
				RebootCounter: 2,
			},
		},
		Status: v1alpha3.MachineStatus{
			Operations: &v1alpha3.OperationsStatus{
				RebootCounter: 1,
			},
		},
	}

	phase, msg := Derive(machine)
	if phase != v1alpha3.MachinePhaseRebooting {
		t.Fatalf("expected Rebooting, got %s", phase)
	}

	if msg != "Rebooting" {
		t.Fatalf("expected message 'Rebooting', got %q", msg)
	}
}

func TestDeriveRebootingForReimage(t *testing.T) {
	machine := &v1alpha3.Machine{
		Spec: v1alpha3.MachineSpec{
			Operations: &v1alpha3.OperationsSpec{
				RebootCounter:  2,
				ReimageCounter: 1,
			},
		},
		Status: v1alpha3.MachineStatus{
			Operations: &v1alpha3.OperationsStatus{
				RebootCounter: 1,
			},
		},
	}

	phase, msg := Derive(machine)
	if phase != v1alpha3.MachinePhaseRebooting {
		t.Fatalf("expected Rebooting, got %s", phase)
	}

	if msg != "Rebooting for reimage" {
		t.Fatalf("expected message 'Rebooting for reimage', got %q", msg)
	}
}

func TestDeriveProvisioning(t *testing.T) {
	machine := &v1alpha3.Machine{
		Spec: v1alpha3.MachineSpec{
			Operations: &v1alpha3.OperationsSpec{
				RebootCounter:  1,
				ReimageCounter: 1,
			},
		},
		Status: v1alpha3.MachineStatus{
			Operations: &v1alpha3.OperationsStatus{
				RebootCounter: 1,
			},
			Conditions: []metav1.Condition{
				{
					Type:   v1alpha3.MachineConditionReimaged,
					Status: metav1.ConditionFalse,
					Reason: "Pending",
				},
			},
		},
	}

	phase, msg := Derive(machine)
	if phase != v1alpha3.MachinePhaseProvisioning {
		t.Fatalf("expected Provisioning, got %s", phase)
	}

	if msg != "PXE booting into new image" {
		t.Fatalf("expected message 'PXE booting into new image', got %q", msg)
	}
}

func TestDeriveTransitionsAwayAfterReimage(t *testing.T) {
	// After reimage completes (Reimaged=True, counters match), the phase
	// derivation should transition away from Provisioning to Pending so the
	// machina controller can take over.
	machine := &v1alpha3.Machine{
		Spec: v1alpha3.MachineSpec{
			Operations: &v1alpha3.OperationsSpec{
				RebootCounter:  1,
				ReimageCounter: 1,
			},
		},
		Status: v1alpha3.MachineStatus{
			Phase:   v1alpha3.MachinePhaseProvisioning,
			Message: "PXE booting into new image",
			Operations: &v1alpha3.OperationsStatus{
				RebootCounter:  1,
				ReimageCounter: 1,
			},
			Conditions: []metav1.Condition{
				{
					Type:   v1alpha3.MachineConditionReimaged,
					Status: metav1.ConditionTrue,
					Reason: "Succeeded",
				},
			},
		},
	}

	phase, msg := Derive(machine)
	if phase != v1alpha3.MachinePhasePending {
		t.Fatalf("expected Pending after reimage completes, got %s", phase)
	}

	if msg != "Operation complete" {
		t.Fatalf("expected message 'Operation complete', got %q", msg)
	}
}

func TestDerivePreservesExistingPhase(t *testing.T) {
	machine := &v1alpha3.Machine{
		Status: v1alpha3.MachineStatus{
			Phase:   v1alpha3.MachinePhaseJoining,
			Message: "Waiting for node",
		},
	}

	phase, msg := Derive(machine)
	if phase != v1alpha3.MachinePhaseJoining {
		t.Fatalf("expected Joining (unchanged), got %s", phase)
	}

	if msg != "Waiting for node" {
		t.Fatalf("expected message 'Waiting for node', got %q", msg)
	}
}

func TestDeriveNilOperations(t *testing.T) {
	machine := &v1alpha3.Machine{
		Status: v1alpha3.MachineStatus{
			Phase:   v1alpha3.MachinePhasePending,
			Message: "Waiting",
		},
	}

	phase, msg := Derive(machine)
	if phase != v1alpha3.MachinePhasePending {
		t.Fatalf("expected Pending (unchanged), got %s", phase)
	}

	if msg != "Waiting" {
		t.Fatalf("expected message 'Waiting', got %q", msg)
	}
}

func TestSetUpdatesInPlace(t *testing.T) {
	machine := &v1alpha3.Machine{
		Spec: v1alpha3.MachineSpec{
			Operations: &v1alpha3.OperationsSpec{
				RebootCounter: 1,
			},
		},
		Status: v1alpha3.MachineStatus{
			Operations: &v1alpha3.OperationsStatus{},
			Phase:      v1alpha3.MachinePhasePending,
			Message:    "old message",
		},
	}

	Set(machine)

	if machine.Status.Phase != v1alpha3.MachinePhaseRebooting {
		t.Fatalf("expected Phase to be updated to Rebooting, got %s", machine.Status.Phase)
	}

	if machine.Status.Message != "Rebooting" {
		t.Fatalf("expected Message to be updated, got %q", machine.Status.Message)
	}
}

func TestDeriveProvisioningTakesPrecedenceOverReboot(t *testing.T) {
	// When both reboot is pending AND Reimaged=False, Provisioning should
	// take precedence because the machine has already been rebooted
	// and is now PXE booting.
	machine := &v1alpha3.Machine{
		Spec: v1alpha3.MachineSpec{
			Operations: &v1alpha3.OperationsSpec{
				RebootCounter:  2,
				ReimageCounter: 1,
			},
		},
		Status: v1alpha3.MachineStatus{
			Operations: &v1alpha3.OperationsStatus{
				RebootCounter: 1,
			},
			Conditions: []metav1.Condition{
				{
					Type:   v1alpha3.MachineConditionReimaged,
					Status: metav1.ConditionFalse,
					Reason: "Pending",
				},
			},
		},
	}

	phase, _ := Derive(machine)
	if phase != v1alpha3.MachinePhaseProvisioning {
		t.Fatalf("expected Provisioning to take precedence over Rebooting, got %s", phase)
	}
}

func TestDeriveTransitionsAwayFromRebootingWhenIdle(t *testing.T) {
	// When the reboot counter has caught up and the phase is still
	// Rebooting, Derive should transition to Pending.
	machine := &v1alpha3.Machine{
		Spec: v1alpha3.MachineSpec{
			Operations: &v1alpha3.OperationsSpec{
				RebootCounter: 1,
			},
		},
		Status: v1alpha3.MachineStatus{
			Phase:   v1alpha3.MachinePhaseRebooting,
			Message: "Rebooting",
			Operations: &v1alpha3.OperationsStatus{
				RebootCounter: 1,
			},
		},
	}

	phase, msg := Derive(machine)
	if phase != v1alpha3.MachinePhasePending {
		t.Fatalf("expected Pending after reboot completes, got %s", phase)
	}

	if msg != "Operation complete" {
		t.Fatalf("expected message 'Operation complete', got %q", msg)
	}
}

func TestDerivePreservesMachinaProvisioning(t *testing.T) {
	// When machina sets Phase=Provisioning for SSH provisioning (no
	// Reimaged condition), metalman's Derive should preserve it.
	machine := &v1alpha3.Machine{
		Status: v1alpha3.MachineStatus{
			Phase:   v1alpha3.MachinePhaseProvisioning,
			Message: "Running install script",
		},
	}

	phase, msg := Derive(machine)
	if phase != v1alpha3.MachinePhaseProvisioning {
		t.Fatalf("expected Provisioning (preserved), got %s", phase)
	}

	if msg != "Running install script" {
		t.Fatalf("expected message 'Running install script', got %q", msg)
	}
}
