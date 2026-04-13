// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package infra

import (
	"context"
	"log/slog"
	"net/http"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	azfake "github.com/Azure/azure-sdk-for-go/sdk/azcore/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/keyvault/armkeyvault"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/keyvault/armkeyvault/fake"
)

func newTestVaultsClient(t *testing.T, srv fake.VaultsServer) *armkeyvault.VaultsClient {
	t.Helper()

	transport := fake.NewVaultsServerTransport(&srv)

	client, err := armkeyvault.NewVaultsClient("sub-id", &azfake.TokenCredential{}, &arm.ClientOptions{
		ClientOptions: policy.ClientOptions{
			Transport: transport,
		},
	})
	if err != nil {
		t.Fatalf("failed to create vaults client: %v", err)
	}

	return client
}

func TestKeyVaultManager_CreateOrUpdate_NewVault(t *testing.T) {
	// When no active vault and no soft-deleted vault exist, CreateOrUpdate should
	// create a new vault without setting CreateMode.
	var capturedParams armkeyvault.VaultCreateOrUpdateParameters

	srv := fake.VaultsServer{
		Get: func(_ context.Context, _, _ string, _ *armkeyvault.VaultsClientGetOptions) (azfake.Responder[armkeyvault.VaultsClientGetResponse], azfake.ErrorResponder) {
			var errResp azfake.ErrorResponder
			errResp.SetResponseError(http.StatusNotFound, "ResourceNotFound")

			return azfake.Responder[armkeyvault.VaultsClientGetResponse]{}, errResp
		},
		GetDeleted: func(_ context.Context, _, _ string, _ *armkeyvault.VaultsClientGetDeletedOptions) (azfake.Responder[armkeyvault.VaultsClientGetDeletedResponse], azfake.ErrorResponder) {
			var errResp azfake.ErrorResponder
			errResp.SetResponseError(http.StatusNotFound, "DeletedVaultNotFound")

			return azfake.Responder[armkeyvault.VaultsClientGetDeletedResponse]{}, errResp
		},
		BeginCreateOrUpdate: func(_ context.Context, _, _ string, params armkeyvault.VaultCreateOrUpdateParameters, _ *armkeyvault.VaultsClientBeginCreateOrUpdateOptions) (azfake.PollerResponder[armkeyvault.VaultsClientCreateOrUpdateResponse], azfake.ErrorResponder) {
			capturedParams = params
			resp := armkeyvault.VaultsClientCreateOrUpdateResponse{
				Vault: armkeyvault.Vault{
					Name:     to.Ptr("test-vault"),
					Location: to.Ptr("eastus"),
				},
			}

			var pollerResp azfake.PollerResponder[armkeyvault.VaultsClientCreateOrUpdateResponse]
			pollerResp.SetTerminalResponse(http.StatusCreated, resp, nil)

			return pollerResp, azfake.ErrorResponder{}
		},
	}

	mgr := &KeyVaultManager{
		Client: newTestVaultsClient(t, srv),
		Logger: slog.Default(),
	}

	desired := armkeyvault.Vault{
		Name:     to.Ptr("test-vault"),
		Location: to.Ptr("eastus"),
		Properties: &armkeyvault.VaultProperties{
			TenantID: to.Ptr("tenant-id"),
			SKU: &armkeyvault.SKU{
				Family: to.Ptr(armkeyvault.SKUFamilyA),
				Name:   to.Ptr(armkeyvault.SKUNameStandard),
			},
		},
	}

	result, err := mgr.CreateOrUpdate(context.Background(), "rg", desired)
	if err != nil {
		t.Fatalf("CreateOrUpdate returned error: %v", err)
	}

	if result == nil {
		t.Fatal("CreateOrUpdate returned nil vault")
	}

	if capturedParams.Properties.CreateMode != nil {
		t.Errorf("expected CreateMode to be nil for new vault, got %v", *capturedParams.Properties.CreateMode)
	}
}

func TestKeyVaultManager_CreateOrUpdate_RecoverSoftDeleted(t *testing.T) {
	// When no active vault exists but a soft-deleted vault does, CreateOrUpdate
	// should set CreateMode to "recover" before calling BeginCreateOrUpdate.
	var capturedParams armkeyvault.VaultCreateOrUpdateParameters

	srv := fake.VaultsServer{
		Get: func(_ context.Context, _, _ string, _ *armkeyvault.VaultsClientGetOptions) (azfake.Responder[armkeyvault.VaultsClientGetResponse], azfake.ErrorResponder) {
			var errResp azfake.ErrorResponder
			errResp.SetResponseError(http.StatusNotFound, "ResourceNotFound")

			return azfake.Responder[armkeyvault.VaultsClientGetResponse]{}, errResp
		},
		GetDeleted: func(_ context.Context, _, _ string, _ *armkeyvault.VaultsClientGetDeletedOptions) (azfake.Responder[armkeyvault.VaultsClientGetDeletedResponse], azfake.ErrorResponder) {
			resp := armkeyvault.VaultsClientGetDeletedResponse{
				DeletedVault: armkeyvault.DeletedVault{
					Name: to.Ptr("test-vault"),
				},
			}

			var r azfake.Responder[armkeyvault.VaultsClientGetDeletedResponse]
			r.SetResponse(http.StatusOK, resp, nil)

			return r, azfake.ErrorResponder{}
		},
		BeginCreateOrUpdate: func(_ context.Context, _, _ string, params armkeyvault.VaultCreateOrUpdateParameters, _ *armkeyvault.VaultsClientBeginCreateOrUpdateOptions) (azfake.PollerResponder[armkeyvault.VaultsClientCreateOrUpdateResponse], azfake.ErrorResponder) {
			capturedParams = params
			resp := armkeyvault.VaultsClientCreateOrUpdateResponse{
				Vault: armkeyvault.Vault{
					Name:     to.Ptr("test-vault"),
					Location: to.Ptr("eastus"),
				},
			}

			var pollerResp azfake.PollerResponder[armkeyvault.VaultsClientCreateOrUpdateResponse]
			pollerResp.SetTerminalResponse(http.StatusOK, resp, nil)

			return pollerResp, azfake.ErrorResponder{}
		},
	}

	mgr := &KeyVaultManager{
		Client: newTestVaultsClient(t, srv),
		Logger: slog.Default(),
	}

	desired := armkeyvault.Vault{
		Name:     to.Ptr("test-vault"),
		Location: to.Ptr("eastus"),
		Properties: &armkeyvault.VaultProperties{
			TenantID: to.Ptr("tenant-id"),
			SKU: &armkeyvault.SKU{
				Family: to.Ptr(armkeyvault.SKUFamilyA),
				Name:   to.Ptr(armkeyvault.SKUNameStandard),
			},
		},
	}

	result, err := mgr.CreateOrUpdate(context.Background(), "rg", desired)
	if err != nil {
		t.Fatalf("CreateOrUpdate returned error: %v", err)
	}

	if result == nil {
		t.Fatal("CreateOrUpdate returned nil vault")
	}

	if capturedParams.Properties.CreateMode == nil {
		t.Fatal("expected CreateMode to be set, got nil")
	}

	if *capturedParams.Properties.CreateMode != armkeyvault.CreateModeRecover {
		t.Errorf("expected CreateMode to be %q, got %q", armkeyvault.CreateModeRecover, *capturedParams.Properties.CreateMode)
	}
}

func TestKeyVaultManager_CreateOrUpdate_ExistingVault(t *testing.T) {
	// When the vault already exists in active state, CreateOrUpdate should
	// return it without calling BeginCreateOrUpdate.
	createCalled := false

	srv := fake.VaultsServer{
		Get: func(_ context.Context, _, _ string, _ *armkeyvault.VaultsClientGetOptions) (azfake.Responder[armkeyvault.VaultsClientGetResponse], azfake.ErrorResponder) {
			resp := armkeyvault.VaultsClientGetResponse{
				Vault: armkeyvault.Vault{
					Name:     to.Ptr("test-vault"),
					Location: to.Ptr("eastus"),
				},
			}

			var r azfake.Responder[armkeyvault.VaultsClientGetResponse]
			r.SetResponse(http.StatusOK, resp, nil)

			return r, azfake.ErrorResponder{}
		},
		BeginCreateOrUpdate: func(_ context.Context, _, _ string, _ armkeyvault.VaultCreateOrUpdateParameters, _ *armkeyvault.VaultsClientBeginCreateOrUpdateOptions) (azfake.PollerResponder[armkeyvault.VaultsClientCreateOrUpdateResponse], azfake.ErrorResponder) {
			createCalled = true
			return azfake.PollerResponder[armkeyvault.VaultsClientCreateOrUpdateResponse]{}, azfake.ErrorResponder{}
		},
	}

	mgr := &KeyVaultManager{
		Client: newTestVaultsClient(t, srv),
		Logger: slog.Default(),
	}

	desired := armkeyvault.Vault{
		Name:     to.Ptr("test-vault"),
		Location: to.Ptr("eastus"),
		Properties: &armkeyvault.VaultProperties{
			TenantID: to.Ptr("tenant-id"),
			SKU: &armkeyvault.SKU{
				Family: to.Ptr(armkeyvault.SKUFamilyA),
				Name:   to.Ptr(armkeyvault.SKUNameStandard),
			},
		},
	}

	result, err := mgr.CreateOrUpdate(context.Background(), "rg", desired)
	if err != nil {
		t.Fatalf("CreateOrUpdate returned error: %v", err)
	}

	if result == nil {
		t.Fatal("CreateOrUpdate returned nil vault")
	}

	if createCalled {
		t.Error("expected BeginCreateOrUpdate to not be called for existing vault")
	}
}
