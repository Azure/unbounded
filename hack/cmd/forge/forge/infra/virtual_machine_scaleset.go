package infra

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/project-unbounded/unbounded-kube/hack/cmd/forge/forge/azsdk"
	"github.com/project-unbounded/unbounded-kube/hack/cmd/forge/forge/validate"
)

type VirtualMachineScaleSetManager struct {
	Client *armcompute.VirtualMachineScaleSetsClient
	Logger *slog.Logger
}

func (m *VirtualMachineScaleSetManager) CreateOrUpdate(ctx context.Context, rgName string, desired armcompute.VirtualMachineScaleSet) (*armcompute.VirtualMachineScaleSet, error) {
	if err := validate.NilOrEmpty(desired.Name, "name"); err != nil {
		return nil, fmt.Errorf("VirtualMachineManager.CreateOrUpdate: %w", err)
	}

	if err := validate.NilOrEmpty(desired.Location, "location"); err != nil {
		return nil, fmt.Errorf("VirtualMachineManager.CreateOrUpdate: %w", err)
	}

	l := m.logger(*desired.Name)

	current, err := m.Get(ctx, rgName, *desired.Name)
	if err != nil && !azsdk.IsNotFoundError(err) {
		return nil, fmt.Errorf("VirtualMachineManager.CreateOrUpdate: get virtual machine: %w", err)
	}

	needCreateOrUpdate := current == nil

	if current != nil {
		l.Info("Found existing virtual machine, applying modifications as necessary")
		// Apply any mutations to desired here
		needCreateOrUpdate = true
	}

	if !needCreateOrUpdate {
		l.Info("Virtual machine already up-to-date")
		return current, nil
	}

	p, err := m.Client.BeginCreateOrUpdate(ctx, rgName, *desired.Name, desired, nil)
	if err != nil {
		return nil, fmt.Errorf("VirtualMachineManager.CreateOrUpdate: update virtual machine: %w", err)
	}

	cuResp, err := p.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("VirtualMachineManager.CreateOrUpdate: %w", err)
	}

	return &cuResp.VirtualMachineScaleSet, nil
}

func (m *VirtualMachineScaleSetManager) Get(ctx context.Context, rg, name string) (*armcompute.VirtualMachineScaleSet, error) {
	r, err := m.Client.Get(ctx, rg, name, nil)
	if err != nil {
		return nil, err
	}

	return &r.VirtualMachineScaleSet, nil
}

func (m *VirtualMachineScaleSetManager) logger(name string) *slog.Logger {
	return m.Logger.With("virtual_machine", name)
}
