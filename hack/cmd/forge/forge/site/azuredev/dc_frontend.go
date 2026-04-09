// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package azuredev

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v7"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"

	"github.com/Azure/unbounded-kube/hack/cmd/forge/forge/azsdk"
	"github.com/Azure/unbounded-kube/hack/cmd/forge/forge/infra"
)

const (
	frontendLBName = "frontend"
)

type datacenterFrontendManager struct {
	azureCli      *azsdk.ClientSet
	logger        *slog.Logger
	resourceGroup *armresources.ResourceGroup
}

type datacenterFrontend struct {
	loadBalancer *armnetwork.LoadBalancer
}

func (m *datacenterFrontendManager) CreateOrUpdate(ctx context.Context) (*datacenterFrontend, error) {
	m.logger.Info("Applying datacenter frontend load balancer")

	loadBalancer, err := m.createOrUpdateLoadBalancer(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("create or update frontend load balancer: %w", err)
	}

	return &datacenterFrontend{
		// publicIP:     publicIP,
		loadBalancer: loadBalancer,
	}, nil
}

func (m *datacenterFrontendManager) GetFrontend(ctx context.Context) (*datacenterFrontend, error) {
	lbMan := infra.LoadBalancerManager{
		Client: m.azureCli.NetworkLoadBalancersClientV2,
		Logger: m.logger,
	}

	lb, err := lbMan.Get(ctx, *m.resourceGroup.Name, frontendLBName)
	if err != nil {
		return nil, fmt.Errorf("get frontend load balancer: %w", err)
	}

	return &datacenterFrontend{
		// publicIP:     ip,
		loadBalancer: lb,
	}, nil
}

func (m *datacenterFrontendManager) CreateOrUpdatePublicIP(ctx context.Context, name string) (*armnetwork.PublicIPAddress, error) {
	desired := armnetwork.PublicIPAddress{
		Name:     to.Ptr(name),
		Location: m.resourceGroup.Location,
		SKU: &armnetwork.PublicIPAddressSKU{
			Name: to.Ptr(armnetwork.PublicIPAddressSKUNameStandard),
			Tier: to.Ptr(armnetwork.PublicIPAddressSKUTierRegional),
		},
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{
			PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
		},
	}

	ipMan := infra.PublicIPAddressManager{
		Client: m.azureCli.NetworkPublicIPAddressesClientV2,
		Logger: m.logger,
	}

	applied, err := ipMan.CreateOrUpdate(ctx, *m.resourceGroup.Name, desired)
	if err != nil {
		return nil, fmt.Errorf("create or update public IP addresses: %w", err)
	}

	return applied, nil
}

func (m *datacenterFrontendManager) createOrUpdateLoadBalancer(ctx context.Context, _ *armnetwork.PublicIPAddress) (*armnetwork.LoadBalancer, error) {
	loadBalancerName := frontendLBName

	desired := armnetwork.LoadBalancer{
		Name:     to.Ptr(loadBalancerName),
		Location: m.resourceGroup.Location,
		SKU: &armnetwork.LoadBalancerSKU{
			Name: to.Ptr(armnetwork.LoadBalancerSKUNameStandard),
			Tier: to.Ptr(armnetwork.LoadBalancerSKUTierRegional),
		},
		Properties: &armnetwork.LoadBalancerPropertiesFormat{},
	}

	lbMan := infra.LoadBalancerManager{
		Client: m.azureCli.NetworkLoadBalancersClientV2,
		Logger: m.logger,
	}

	applied, err := lbMan.CreateOrUpdate(ctx, *m.resourceGroup.Name, desired)
	if err != nil {
		return nil, fmt.Errorf("create or update public IP addresses: %w", err)
	}

	return applied, nil
}

func getLoadBalancerFrontendByName(name string, frontends []*armnetwork.FrontendIPConfiguration) *armnetwork.FrontendIPConfiguration {
	for _, p := range frontends {
		if *p.Name == name {
			return p
		}
	}

	return nil
}

func putLoadBalancerFrontend(lb *armnetwork.LoadBalancer, frontend *armnetwork.FrontendIPConfiguration) {
	if lb.Properties == nil {
		lb.Properties = &armnetwork.LoadBalancerPropertiesFormat{}
	}

	if lb.Properties.FrontendIPConfigurations == nil {
		lb.Properties.FrontendIPConfigurations = []*armnetwork.FrontendIPConfiguration{}
	}

	for i, p := range lb.Properties.FrontendIPConfigurations {
		if p.Name != nil && frontend.Name != nil && *p.Name == *frontend.Name {
			lb.Properties.FrontendIPConfigurations[i] = frontend
			return
		}
	}

	lb.Properties.FrontendIPConfigurations = append(lb.Properties.FrontendIPConfigurations, frontend)
}

func getLoadBalancerBackendPoolByName(name string, pools []*armnetwork.BackendAddressPool) *armnetwork.BackendAddressPool {
	for _, p := range pools {
		if *p.Name == name {
			return p
		}
	}

	return nil
}

func putLoadBalancerBackendPool(lb *armnetwork.LoadBalancer, backend *armnetwork.BackendAddressPool) {
	if lb.Properties == nil {
		lb.Properties = &armnetwork.LoadBalancerPropertiesFormat{}
	}

	if lb.Properties.BackendAddressPools == nil {
		lb.Properties.BackendAddressPools = []*armnetwork.BackendAddressPool{}
	}

	// Check if pool with same name exists and replace it
	for i, p := range lb.Properties.BackendAddressPools {
		if p.Name != nil && backend.Name != nil && *p.Name == *backend.Name {
			lb.Properties.BackendAddressPools[i] = backend
			return
		}
	}

	// Pool doesn't exist, append it
	lb.Properties.BackendAddressPools = append(lb.Properties.BackendAddressPools, backend)
}

func getLoadBalancerInboundNatRuleByName(name string, rules []*armnetwork.InboundNatRule) *armnetwork.InboundNatRule {
	for _, r := range rules {
		if *r.Name == name {
			return r
		}
	}

	return nil
}

func putLoadBalancerInboundNatRule(lb *armnetwork.LoadBalancer, rule *armnetwork.InboundNatRule) {
	if lb.Properties == nil {
		lb.Properties = &armnetwork.LoadBalancerPropertiesFormat{}
	}

	if lb.Properties.InboundNatRules == nil {
		lb.Properties.InboundNatRules = []*armnetwork.InboundNatRule{}
	}

	// Check if rule with same name exists and replace it
	for i, r := range lb.Properties.InboundNatRules {
		if r.Name != nil && rule.Name != nil && *r.Name == *rule.Name {
			lb.Properties.InboundNatRules[i] = rule
			return
		}
	}

	// Rule doesn't exist, append it
	lb.Properties.InboundNatRules = append(lb.Properties.InboundNatRules, rule)
}

func getLoadBalancerOutboundRuleByName(name string, rules []*armnetwork.OutboundRule) *armnetwork.OutboundRule {
	for _, r := range rules {
		if *r.Name == name {
			return r
		}
	}

	return nil
}

func putLoadBalancerOutboundRule(lb *armnetwork.LoadBalancer, rule *armnetwork.OutboundRule) {
	if lb.Properties == nil {
		lb.Properties = &armnetwork.LoadBalancerPropertiesFormat{}
	}

	if lb.Properties.OutboundRules == nil {
		lb.Properties.OutboundRules = []*armnetwork.OutboundRule{}
	}

	// Check if rule with same name exists and replace it
	for i, r := range lb.Properties.OutboundRules {
		if r.Name != nil && rule.Name != nil && *r.Name == *rule.Name {
			lb.Properties.OutboundRules[i] = rule
			return
		}
	}

	// Rule doesn't exist, append it
	lb.Properties.OutboundRules = append(lb.Properties.OutboundRules, rule)
}
