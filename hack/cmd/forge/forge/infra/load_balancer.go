package infra

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v7"
	"github.com/project-unbounded/unbounded-kube/hack/cmd/forge/forge/azsdk"
	"github.com/project-unbounded/unbounded-kube/hack/cmd/forge/forge/validate"
)

type LoadBalancerManager struct {
	Client *armnetwork.LoadBalancersClient
	Logger *slog.Logger
}

func (m *LoadBalancerManager) CreateOrUpdate(ctx context.Context, rgName string, desired armnetwork.LoadBalancer) (*armnetwork.LoadBalancer, error) {
	if err := validate.NilOrEmpty(desired.Name, "name"); err != nil {
		return nil, fmt.Errorf("LoadBalancerManager.CreateOrUpdate: %w", err)
	}

	if err := validate.NilOrEmpty(desired.Location, "location"); err != nil {
		return nil, fmt.Errorf("LoadBalancerManager.CreateOrUpdate: %w", err)
	}

	l := m.logger(*desired.Name)

	current, err := m.Get(ctx, rgName, *desired.Name)
	if err != nil && !azsdk.IsNotFoundError(err) {
		return nil, fmt.Errorf("LoadBalancerManager.CreateOrUpdate: get load balancer: %w", err)
	}

	needCreateOrUpdate := current == nil

	if current != nil {
		l.Info("Found existing load balancer, applying modifications as necessary")

		if len(current.Properties.BackendAddressPools) != len(desired.Properties.BackendAddressPools) {
			needCreateOrUpdate = true
		}

		if len(current.Properties.InboundNatRules) != len(desired.Properties.InboundNatRules) {
			needCreateOrUpdate = true
		}

		if len(current.Properties.OutboundRules) != len(desired.Properties.OutboundRules) {
			needCreateOrUpdate = true
		}
	}

	if !needCreateOrUpdate {
		l.Info("Load balancer already up-to-date")
		return current, nil
	}

	p, err := m.Client.BeginCreateOrUpdate(ctx, rgName, *desired.Name, desired, nil)
	if err != nil {
		return nil, fmt.Errorf("LoadBalancerManager.CreateOrUpdate: update load balancer: %w", err)
	}

	cuResp, err := p.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("LoadBalancerManager.CreateOrUpdate: poll until done: %w", err)
	}

	return &cuResp.LoadBalancer, nil
}

func (m *LoadBalancerManager) Get(ctx context.Context, rgName, name string) (*armnetwork.LoadBalancer, error) {
	r, err := m.Client.Get(ctx, rgName, name, nil)
	if err != nil {
		return nil, fmt.Errorf("LoadBalancerManager.Get: %w", err)
	}

	return &r.LoadBalancer, nil
}

func (m *LoadBalancerManager) logger(name string) *slog.Logger {
	return m.Logger.With("load_balancer", name)
}
