package infra

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v7"

	"github.com/project-unbounded/unbounded-kube/hack/cmd/forge/forge/azsdk"
	"github.com/project-unbounded/unbounded-kube/hack/cmd/forge/forge/validate"
)

type NetworkSecurityGroupManager struct {
	Client *armnetwork.SecurityGroupsClient
	Logger *slog.Logger
}

func (m *NetworkSecurityGroupManager) CreateOrUpdate(ctx context.Context, rgName string, desired armnetwork.SecurityGroup) (*armnetwork.SecurityGroup, error) {
	if err := validate.NilOrEmpty(desired.Name, "name"); err != nil {
		return nil, fmt.Errorf("NetworkSecurityGroupManager.CreateOrUpdate: %w", err)
	}

	if err := validate.NilOrEmpty(desired.Location, "location"); err != nil {
		return nil, fmt.Errorf("NetworkSecurityGroupManager.CreateOrUpdate: %w", err)
	}

	l := m.logger(*desired.Name)

	current, err := m.Get(ctx, rgName, *desired.Name)
	if err != nil && !azsdk.IsNotFoundError(err) {
		return nil, fmt.Errorf("NetworkSecurityGroupManager.CreateOrUpdate: get network security group: %w", err)
	}

	needCreateOrUpdate := current == nil

	if current != nil {
		l.Info("Found existing network security group, merging rules")

		// Merge rules: desired rules override existing rules by name
		mergedRules := mergeSecurityRules(current.Properties.SecurityRules, desired.Properties.SecurityRules, l)

		// Check if rules have changed
		if !securityRulesEqual(current.Properties.SecurityRules, mergedRules) {
			desired.Properties.SecurityRules = mergedRules
			needCreateOrUpdate = true

			l.Info("Security rules changed, updating NSG", "totalRules", len(mergedRules))
		} else {
			l.Info("Security rules unchanged")
		}
	}

	if !needCreateOrUpdate {
		l.Info("Network security group already up-to-date")
		return current, nil
	}

	p, err := m.Client.BeginCreateOrUpdate(ctx, rgName, *desired.Name, desired, nil)
	if err != nil {
		return nil, fmt.Errorf("NetworkSecurityGroupManager.CreateOrUpdate: update network security group: %w", err)
	}

	cuResp, err := p.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("NetworkSecurityGroupManager.CreateOrUpdate: poll until done: %w", err)
	}

	return &cuResp.SecurityGroup, nil
}

func (m *NetworkSecurityGroupManager) Get(ctx context.Context, rgName, name string) (*armnetwork.SecurityGroup, error) {
	r, err := m.Client.Get(ctx, rgName, name, nil)
	if err != nil {
		return nil, fmt.Errorf("NetworkSecurityGroupManager.Get: %w", err)
	}

	return &r.SecurityGroup, nil
}

func (m *NetworkSecurityGroupManager) logger(name string) *slog.Logger {
	return m.Logger.With("network_security_group", name)
}

// mergeSecurityRules merges existing rules with desired rules.
// Desired rules take precedence when there's a name conflict.
// Returns a new slice containing all rules.
func mergeSecurityRules(existing, desired []*armnetwork.SecurityRule, logger *slog.Logger) []*armnetwork.SecurityRule {
	if existing == nil {
		return desired
	}

	if desired == nil {
		return existing
	}

	// Create a map of desired rules by name for quick lookup
	desiredMap := make(map[string]*armnetwork.SecurityRule)

	for _, rule := range desired {
		if rule != nil && rule.Name != nil {
			desiredMap[*rule.Name] = rule
		}
	}

	// Start with all desired rules
	merged := make([]*armnetwork.SecurityRule, 0, len(desired)+len(existing))
	merged = append(merged, desired...)

	// Add existing rules that don't conflict with desired rules
	for _, existingRule := range existing {
		if existingRule == nil || existingRule.Name == nil {
			continue
		}

		if _, conflict := desiredMap[*existingRule.Name]; !conflict {
			merged = append(merged, existingRule)
			logger.Debug("Preserving existing rule", "rule", *existingRule.Name)
		} else {
			logger.Debug("Overriding existing rule with desired rule", "rule", *existingRule.Name)
		}
	}

	return merged
}

// securityRulesEqual checks if two rule slices are equivalent.
func securityRulesEqual(a, b []*armnetwork.SecurityRule) bool {
	if len(a) != len(b) {
		return false
	}

	// Create maps by name for comparison
	aMap := make(map[string]*armnetwork.SecurityRule)

	for _, rule := range a {
		if rule != nil && rule.Name != nil {
			aMap[*rule.Name] = rule
		}
	}

	bMap := make(map[string]*armnetwork.SecurityRule)

	for _, rule := range b {
		if rule != nil && rule.Name != nil {
			bMap[*rule.Name] = rule
		}
	}

	// Check if all rules in a exist in b with same configuration
	for name, aRule := range aMap {
		bRule, exists := bMap[name]
		if !exists {
			return false
		}

		// Compare key properties of the rules
		if !securityRuleEqual(aRule, bRule) {
			return false
		}
	}

	return true
}

// securityRuleEqual compares two security rules for equality.
func securityRuleEqual(a, b *armnetwork.SecurityRule) bool {
	if a == nil && b == nil {
		return true
	}

	if a == nil || b == nil {
		return false
	}

	// Compare names
	if !stringPtrEqual(a.Name, b.Name) {
		return false
	}

	// Compare properties if they exist
	if a.Properties == nil && b.Properties == nil {
		return true
	}

	if a.Properties == nil || b.Properties == nil {
		return false
	}

	ap := a.Properties
	bp := b.Properties

	// Compare key fields
	return stringPtrEqual((*string)(ap.Protocol), (*string)(bp.Protocol)) &&
		stringPtrEqual(ap.SourcePortRange, bp.SourcePortRange) &&
		stringPtrEqual(ap.DestinationPortRange, bp.DestinationPortRange) &&
		stringPtrEqual(ap.SourceAddressPrefix, bp.SourceAddressPrefix) &&
		stringPtrEqual(ap.DestinationAddressPrefix, bp.DestinationAddressPrefix) &&
		stringPtrEqual((*string)(ap.Access), (*string)(bp.Access)) &&
		int32PtrEqual(ap.Priority, bp.Priority) &&
		stringPtrEqual((*string)(ap.Direction), (*string)(bp.Direction))
}

// stringPtrEqual compares two string pointers for equality.
func stringPtrEqual(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}

	if a == nil || b == nil {
		return false
	}

	return *a == *b
}

// int32PtrEqual compares two int32 pointers for equality.
func int32PtrEqual(a, b *int32) bool {
	if a == nil && b == nil {
		return true
	}

	if a == nil || b == nil {
		return false
	}

	return *a == *b
}
