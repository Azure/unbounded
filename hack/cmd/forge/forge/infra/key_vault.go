package infra

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/keyvault/armkeyvault"

	"github.com/project-unbounded/unbounded-kube/hack/cmd/forge/forge/azsdk"
	"github.com/project-unbounded/unbounded-kube/hack/cmd/forge/forge/validate"
)

type SecretsManager struct {
	*armkeyvault.SecretsClient
	Logger *slog.Logger
}

func (m *SecretsManager) SetSecret(ctx context.Context, rgName, vaultName, secretName string, secret armkeyvault.SecretCreateOrUpdateParameters) error {
	_, err := m.CreateOrUpdate(ctx, rgName, vaultName, secretName, secret, nil)
	if err != nil {
		return fmt.Errorf("SecretsManager.SetSecret: %w", err)
	}

	return nil
}

type KeyVaultManager struct {
	Client *armkeyvault.VaultsClient
	Logger *slog.Logger
}

func (m *KeyVaultManager) CreateOrUpdate(ctx context.Context, rgName string, desired armkeyvault.Vault) (*armkeyvault.Vault, error) {
	if err := validate.NilOrEmpty(desired.Name, "name"); err != nil {
		return nil, fmt.Errorf("KeyVaultManager.CreateOrUpdate: %w", err)
	}

	if err := validate.NilOrEmpty(desired.Location, "location"); err != nil {
		return nil, fmt.Errorf("KeyVaultManager.CreateOrUpdate: %w", err)
	}

	l := m.logger(*desired.Name)

	current, err := m.Get(ctx, rgName, *desired.Name)
	if err != nil && !azsdk.IsNotFoundError(err) {
		return nil, fmt.Errorf("KeyVaultManager.CreateOrUpdate: get key vault: %w", err)
	}

	needCreateOrUpdate := current == nil

	if current != nil {
		l.Info("Found existing key vault, applying modifications as necessary")
		// Apply any mutations to desired here
		// needCreateOrUpdate = true
	}

	if !needCreateOrUpdate {
		l.Info("Key vault already up-to-date")
		return current, nil
	}

	// Convert Vault to VaultCreateOrUpdateParameters
	params := armkeyvault.VaultCreateOrUpdateParameters{
		Location:   desired.Location,
		Tags:       desired.Tags,
		Properties: desired.Properties,
	}

	p, err := m.Client.BeginCreateOrUpdate(ctx, rgName, *desired.Name, params, nil)
	if err != nil {
		return nil, fmt.Errorf("KeyVaultManager.CreateOrUpdate: update key vault: %w", err)
	}

	cuResp, err := p.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("KeyVaultManager.CreateOrUpdate: %w", err)
	}

	return &cuResp.Vault, nil
}

func (m *KeyVaultManager) Get(ctx context.Context, rgName, name string) (*armkeyvault.Vault, error) {
	r, err := m.Client.Get(ctx, rgName, name, nil)
	if err != nil {
		return nil, fmt.Errorf("KeyVaultManager.Get: %w", err)
	}

	return &r.Vault, nil
}

func (m *KeyVaultManager) Delete(ctx context.Context, rgName, name string) error {
	if err := validate.Empty(name, "name"); err != nil {
		return fmt.Errorf("KeyVaultManager.Delete: %w", err)
	}

	m.logger(name)

	_, err := m.Client.Delete(ctx, rgName, name, nil)
	if err != nil {
		if azsdk.IsNotFoundError(err) {
			return nil
		}

		return fmt.Errorf("KeyVaultManager.Delete: %w", err)
	}

	return nil
}

func (m *KeyVaultManager) logger(name string) *slog.Logger {
	return m.Logger.With("key_vault", name)
}
