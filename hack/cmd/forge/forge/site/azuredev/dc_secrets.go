// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package azuredev

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"math/big"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/keyvault/armkeyvault"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"

	"github.com/Azure/unbounded/hack/cmd/forge/forge/azsdk"
	"github.com/Azure/unbounded/hack/cmd/forge/forge/infra"
)

const (
	tagSSHUserSecret       = "ssh.user"
	tagSSHPublicKeySecret  = "ssh.public-key"
	tagSSHPrivateKeySecret = "ssh.private-key"
)

type datacenterSecretsManager struct {
	azureCli      *azsdk.ClientSet
	resourceGroup *armresources.ResourceGroup
	logger        *slog.Logger
}

func (m *datacenterSecretsManager) keyVaultName() string {
	return fmt.Sprintf("main-%s", uniqueID(*m.resourceGroup.ID))
}

func (m *datacenterSecretsManager) PutSSHSecrets(ctx context.Context, names map[string]*string, username string, keyPair *infra.SSHKeyPair) error {
	if err := m.CreateOrUpdate(ctx); err != nil {
		return fmt.Errorf("apply secrets: %w", err)
	}

	secretMan := infra.SecretsManager{
		SecretsClient: m.azureCli.KeyVaultSecretsClientV2,
		Logger:        m.logger,
	}

	usernameSecret := armkeyvault.SecretCreateOrUpdateParameters{
		Properties: &armkeyvault.SecretProperties{
			Attributes: &armkeyvault.SecretAttributes{
				Enabled: to.Ptr(true),
			},
			Value: to.Ptr(username),
		},
	}

	publicKey, err := keyPair.PublicKey()
	if err != nil {
		return fmt.Errorf("get public key: %w", err)
	}

	publicKeySecret := armkeyvault.SecretCreateOrUpdateParameters{
		Properties: &armkeyvault.SecretProperties{
			Attributes: &armkeyvault.SecretAttributes{
				Enabled: to.Ptr(true),
			},
			Value: to.Ptr(string(publicKey)),
		},
	}

	privateKey, err := keyPair.PrivateKey()
	if err != nil {
		return fmt.Errorf("get private key: %w", err)
	}

	privateKeySecret := armkeyvault.SecretCreateOrUpdateParameters{
		Properties: &armkeyvault.SecretProperties{
			Attributes: &armkeyvault.SecretAttributes{
				Enabled: to.Ptr(true),
			},
			Value: to.Ptr(string(privateKey)),
		},
	}

	for _, secret := range []struct {
		name   string
		secret armkeyvault.SecretCreateOrUpdateParameters
	}{
		{name: *names[tagSSHUserSecret], secret: usernameSecret},
		{name: *names[tagSSHPublicKeySecret], secret: publicKeySecret},
		{name: *names[tagSSHPrivateKeySecret], secret: privateKeySecret},
	} {
		if err := secretMan.SetSecret(ctx, *m.resourceGroup.Name, m.keyVaultName(), secret.name, secret.secret); err != nil {
			return fmt.Errorf("put secret %q: %w", secret.name, err)
		}
	}

	return nil
}

func (m *datacenterSecretsManager) CreateOrUpdate(ctx context.Context) error {
	m.logger.Info("Applying datacenter key vault")

	_, err := m.createOrUpdateKeyVault(ctx)
	if err != nil {
		return fmt.Errorf("create or update key vault: %w", err)
	}

	return nil
}

func (m *datacenterSecretsManager) createOrUpdateKeyVault(ctx context.Context) (*armkeyvault.Vault, error) {
	desired := armkeyvault.Vault{
		Name:     to.Ptr(m.keyVaultName()),
		Location: m.resourceGroup.Location,
		Properties: &armkeyvault.VaultProperties{
			AccessPolicies: []*armkeyvault.AccessPolicyEntry{
				{
					TenantID: to.Ptr(m.azureCli.TenantID),
					ObjectID: to.Ptr(m.azureCli.CurrentIdentityObjectID()),
					Permissions: &armkeyvault.Permissions{
						Secrets: []*armkeyvault.SecretPermissions{
							to.Ptr(armkeyvault.SecretPermissionsGet),
							to.Ptr(armkeyvault.SecretPermissionsList),
							to.Ptr(armkeyvault.SecretPermissionsSet),
						},
					},
				},
			},
			EnabledForDeployment:         to.Ptr(false),
			EnabledForDiskEncryption:     to.Ptr(false),
			EnabledForTemplateDeployment: to.Ptr(false),
			SoftDeleteRetentionInDays:    to.Ptr(int32(90)),
			PublicNetworkAccess:          to.Ptr(string(armkeyvault.PublicNetworkAccessEnabled)),
			SKU: &armkeyvault.SKU{
				Family: to.Ptr(armkeyvault.SKUFamilyA),
				Name:   to.Ptr(armkeyvault.SKUNameStandard),
			},
			TenantID: to.Ptr(m.azureCli.TenantID),
		},
	}

	kvm := infra.KeyVaultManager{
		Client: m.azureCli.KeyVaultClientV2,
		Logger: m.logger,
	}

	applied, err := kvm.CreateOrUpdate(ctx, *m.resourceGroup.Name, desired)
	if err != nil {
		return nil, err
	}

	return applied, nil
}

func uniqueID(s string) string {
	hash := sha256.Sum256([]byte(strings.ToLower(s)))
	return new(big.Int).SetBytes(hash[:]).Text(36)[:10]
}
