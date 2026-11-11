// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package azurevm

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/machineops"
	publicmachineops "github.com/Azure/unbounded/pkg/machineops"
)

func TestParseCapacityConfig(t *testing.T) {
	config, err := parseCapacityConfig(testConfig())
	require.NoError(t, err)
	require.Equal(t, int32(defaultOSDiskSizeGB), config.OSDiskSizeGB)
	require.Equal(t, int64(defaultMaxPods), config.MaxPods)
	require.Equal(t, "eastus2-1", config.offeringZone())

	tests := map[string]func(map[string]string){
		"unknown key": func(data map[string]string) {
			data["vmsize"] = "typo"
		},
		"missing required key": func(data map[string]string) {
			delete(data, ConfigSubnetID)
		},
		"unpinned image": func(data map[string]string) {
			data[ConfigImage] = "Canonical:ubuntu-24_04-lts:server:latest"
		},
		"subnet is not a subnet": func(data map[string]string) {
			data[ConfigSubnetID] = "/subscriptions/x/resourceGroups/y/providers/Microsoft.Network/virtualNetworks/vnet"
		},
		"identity is not a user assigned identity": func(data map[string]string) {
			data[ConfigWorkerIdentityResourceID] = testSubnetID
		},
		"partial price": func(data map[string]string) {
			delete(data, ConfigPriceObservedAt)
		},
		"invalid price": func(data map[string]string) {
			data[ConfigPricePerHour] = "-1"
		},
		"invalid currency": func(data map[string]string) {
			data[ConfigPriceCurrency] = "usd"
		},
		"invalid reserved resource": func(data map[string]string) {
			data[ConfigKubeReserved] = "gpu=1"
		},
		"invalid zone": func(data map[string]string) {
			data[ConfigZone] = "eastus2-1"
		},
		"max pods out of range": func(data map[string]string) {
			data[ConfigMaxPods] = "1000"
		},
		"gallery image without a version": func(data map[string]string) {
			data[ConfigImage] = testGalleryImage
		},
		"gallery image at the latest version": func(data map[string]string) {
			data[ConfigImage] = testGalleryImage + "/versions/latest"
		},
		"gallery image with a partial version": func(data map[string]string) {
			data[ConfigImage] = testGalleryImage + "/versions/1.0"
		},
		"gallery image version with a trailing slash": func(data map[string]string) {
			data[ConfigImage] = testGalleryImage + "/versions/1.0.0/"
		},
		"image ID of another resource type": func(data map[string]string) {
			data[ConfigImage] = "/subscriptions/" + testSubscription + "/resourceGroups/images/providers/Microsoft.Compute/disks/images/versions"
		},
		"subscription is not a GUID": func(data map[string]string) {
			data[ConfigSubscriptionID] = "my-subscription"
		},
		"subscription with a path": func(data map[string]string) {
			data[ConfigSubscriptionID] = testSubscription + "/resourceGroups/other"
		},
		"resource group with a slash": func(data map[string]string) {
			data[ConfigResourceGroup] = "workers/other"
		},
		"resource group ending with a period": func(data map[string]string) {
			data[ConfigResourceGroup] = "workers."
		},
		"resource group too long": func(data map[string]string) {
			data[ConfigResourceGroup] = strings.Repeat("a", 91)
		},
		"admin username root": func(data map[string]string) {
			data[ConfigAdminUsername] = "root"
		},
		"admin username admin": func(data map[string]string) {
			data[ConfigAdminUsername] = "admin"
		},
		"admin username administrator": func(data map[string]string) {
			data[ConfigAdminUsername] = "administrator"
		},
		"VM size with a space": func(data map[string]string) {
			data[ConfigVMSize] = "Standard D4s_v3"
		},
		"VM size ending with a hyphen": func(data map[string]string) {
			data[ConfigVMSize] = "Standard_E4-"
		},
		"identity with a trailing slash": func(data map[string]string) {
			data[ConfigWorkerIdentityResourceID] = testIdentityID + "/"
		},
		"subnet with a trailing slash": func(data map[string]string) {
			data[ConfigSubnetID] = testSubnetID + "/"
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			data := testConfig()
			mutate(data)

			_, err := parseCapacityConfig(data)
			require.Error(t, err)
		})
	}
}

func TestParseCapacityConfigAcceptsValidForms(t *testing.T) {
	tests := map[string]func(map[string]string){
		"hyphenated constrained-core VM size": func(data map[string]string) {
			data[ConfigVMSize] = "Standard_E4-2ads_v5"
		},
		"pinned gallery image version": func(data map[string]string) {
			data[ConfigImage] = testGalleryImage + "/versions/1.2.3"
		},
		"gallery image ID in another case": func(data map[string]string) {
			data[ConfigImage] = strings.ToLower(testGalleryImage) + "/VERSIONS/2024.10.16"
		},
		"managed image": func(data map[string]string) {
			data[ConfigImage] = "/subscriptions/" + testSubscription + "/resourceGroups/images/providers/Microsoft.Compute/images/worker"
		},
		"upper case subscription GUID": func(data map[string]string) {
			data[ConfigSubscriptionID] = "ABCDEF00-0000-0000-0000-000000000001"
		},
		"resource group with punctuation": func(data map[string]string) {
			data[ConfigResourceGroup] = "Workers_(east).2-rg"
		},
		"admin username that only contains a reserved name": func(data map[string]string) {
			data[ConfigAdminUsername] = "rootless"
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			data := testConfig()
			mutate(data)

			_, err := parseCapacityConfig(data)
			require.NoError(t, err)
		})
	}
}

func TestResolveClassPublishesObservedOffering(t *testing.T) {
	client := newFakeCapacityClient()
	provider := newTestProvider(client)

	offering := resolveTestOffering(t, provider)

	require.Equal(t, "standard_d4s_v3-eastus2-1-on-demand", offering.Name)
	require.Equal(t, "Standard_D4s_v3", offering.InstanceType)
	require.Equal(t, "eastus2-1", offering.Zone)
	require.Equal(t, unboundedv1alpha3.CapacityTypeOnDemand, offering.CapacityType)
	require.Equal(t, unboundedv1alpha3.OfferingAvailable, offering.Availability)
	require.Equal(t, map[string]string{
		corev1.LabelArchStable:              "amd64",
		corev1.LabelOSStable:                "linux",
		corev1.LabelTopologyRegion:          "eastus2",
		corev1.LabelTopologyZone:            "eastus2-1",
		corev1.LabelInstanceTypeStable:      "Standard_D4s_v3",
		unboundedv1alpha3.LabelCapacityType: "OnDemand",
	}, offering.Labels)
	require.Equal(t, testNow, offering.ObservedAt.Time)
	require.Equal(t, testNow.Add(offeringTTL), offering.ValidUntil.Time)

	require.Equal(t, int64(4), offering.Capacity.Cpu().Value())
	require.Equal(t, int64(110), offering.Capacity.Pods().Value())
	requireQuantity(t, "15155Mi", offering.Capacity[corev1.ResourceMemory])
	requireQuantity(t, "100m", offering.Overhead.KubeReserved[corev1.ResourceCPU])
	requireQuantity(t, "100Mi", offering.Overhead.EvictionThreshold[corev1.ResourceMemory])

	// Whatever is reserved leaves room for workloads.
	for _, reserved := range []corev1.ResourceList{
		offering.Overhead.KubeReserved,
		offering.Overhead.SystemReserved,
		offering.Overhead.EvictionThreshold,
	} {
		for name, quantity := range reserved {
			capacity, ok := offering.Capacity[name]
			require.True(t, ok, name)
			require.Negative(t, quantity.Cmp(capacity), name)
		}
	}

	require.NotNil(t, offering.Price)
	require.Equal(t, "USD", offering.Price.Currency)
	require.Equal(t, "0.192", offering.Price.PerHour)
	require.Equal(t, "azure-retail-prices-api", offering.Price.Source)
	require.Equal(t, time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC), offering.Price.ObservedAt.UTC())
}

func TestResolveClassWithoutZoneOrPrice(t *testing.T) {
	data := testConfig()
	for _, key := range []string{ConfigZone, ConfigPriceCurrency, ConfigPricePerHour, ConfigPriceSource, ConfigPriceObservedAt} {
		delete(data, key)
	}

	client := newFakeCapacityClient()
	provider := newTestProvider(client)

	resolution, err := provider.ResolveClass(t.Context(), publicmachineops.ClassRequest{
		ProviderConfig: publicmachineops.ProviderConfig{Data: data},
	})
	require.NoError(t, err)
	require.Len(t, resolution.Offerings, 1)

	// A regional VM still has a zone a consumer can schedule against.
	offering := resolution.Offerings[0]
	require.Equal(t, "standard_d4s_v3-eastus2-on-demand", offering.Name)
	require.Equal(t, "eastus2", offering.Zone)
	require.Equal(t, "eastus2", offering.Labels[corev1.LabelTopologyZone])
	require.Equal(t, unboundedv1alpha3.OfferingAvailable, offering.Availability)
	require.Nil(t, offering.Price)

	request := testOperationRequest(t, unboundedv1alpha3.OperationHostAllocate, offering)
	request.Allocation.ProviderConfig = publicmachineops.ProviderConfig{Data: data}

	_, err = provider.BeginAllocate(t.Context(), request)
	require.NoError(t, err)
	require.Len(t, client.creates, 1)
	require.Empty(t, client.creates[0].Zones)
}

func TestResolveClassReportsArm64(t *testing.T) {
	client := newFakeCapacityClient()
	client.skus["standard_d4s_v3"].Capabilities[2].Value = new("Arm64")

	offering := resolveTestOffering(t, newTestProvider(client))
	require.Equal(t, "arm64", offering.Labels[corev1.LabelArchStable])
}

func TestResolveClassCachesSKUObservation(t *testing.T) {
	client := newFakeCapacityClient()
	provider := newTestProvider(client)

	resolveTestOffering(t, provider)
	resolveTestOffering(t, provider)
	require.Equal(t, 1, client.skuLists)

	provider.Now = func() time.Time {
		return testNow.Add(skuCacheTTL)
	}

	offering := resolveTestOffering(t, provider)
	require.Equal(t, 2, client.skuLists)
	require.Equal(t, testNow.Add(skuCacheTTL), offering.ObservedAt.Time)
}

func TestResolveClassReportsRestrictions(t *testing.T) {
	tests := map[string]func(*armcompute.ResourceSKU){
		"zone restricted": func(sku *armcompute.ResourceSKU) {
			sku.Restrictions = []*armcompute.ResourceSKURestrictions{
				{
					Type: new(armcompute.ResourceSKURestrictionsTypeZone),
					RestrictionInfo: &armcompute.ResourceSKURestrictionInfo{
						Zones: []*string{
							new("1"),
						},
					},
				},
			}
		},
		"location restricted": func(sku *armcompute.ResourceSKU) {
			sku.Restrictions = []*armcompute.ResourceSKURestrictions{
				{
					Type: new(armcompute.ResourceSKURestrictionsTypeLocation),
					Values: []*string{
						new("eastus2"),
					},
				},
			}
		},
		"zone not offered": func(sku *armcompute.ResourceSKU) {
			sku.LocationInfo[0].Zones = []*string{
				new("2"),
			}
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			client := newFakeCapacityClient()
			mutate(client.skus["standard_d4s_v3"])

			// An Unavailable offering still describes the machine.
			offering := resolveTestOffering(t, newTestProvider(client))
			require.Equal(t, unboundedv1alpha3.OfferingUnavailable, offering.Availability)
			require.Equal(t, int64(4), offering.Capacity.Cpu().Value())
			require.Positive(t, offering.Capacity.Memory().Value())
			require.Positive(t, offering.Capacity.Pods().Value())
		})
	}
}

func TestResolveClassFailures(t *testing.T) {
	resolve := func(client *fakeCapacityClient, data map[string]string) error {
		_, err := newTestProvider(client).ResolveClass(t.Context(), publicmachineops.ClassRequest{
			ProviderConfig: publicmachineops.ProviderConfig{Data: data},
		})

		return err
	}

	t.Run("invalid config is permanent", func(t *testing.T) {
		data := testConfig()
		delete(data, ConfigVMSize)

		requirePermanent(t, resolve(newFakeCapacityClient(), data), unboundedv1alpha3.FailureInvalidRequest)
	})

	t.Run("SSH key with a second line is permanent", func(t *testing.T) {
		data := testConfig()
		data[ConfigSSHPublicKey] += "\nssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIextra another"

		err := resolve(newFakeCapacityClient(), data)
		requirePermanent(t, err, unboundedv1alpha3.FailureInvalidRequest)
		require.ErrorContains(t, err, "on one line")
	})

	t.Run("VM size not offered in the region is permanent", func(t *testing.T) {
		client := newFakeCapacityClient()
		delete(client.skus, "standard_d4s_v3")

		err := resolve(client, testConfig())
		requirePermanent(t, err, unboundedv1alpha3.FailureInvalidRequest)
		require.ErrorContains(t, err, "Standard_D4s_v3 is not offered")
	})

	t.Run("reservations larger than the VM are permanent", func(t *testing.T) {
		data := testConfig()
		data[ConfigSystemReserved] = "cpu=8"

		err := resolve(newFakeCapacityClient(), data)
		requirePermanent(t, err, unboundedv1alpha3.FailureInvalidRequest)
		require.ErrorContains(t, err, "must be less than the cpu capacity")
	})

	t.Run("reservations equal to the VM are permanent", func(t *testing.T) {
		data := testConfig()
		data[ConfigKubeReserved] = "cpu=1"
		data[ConfigSystemReserved] = "cpu=3"

		err := resolve(newFakeCapacityClient(), data)
		requirePermanent(t, err, unboundedv1alpha3.FailureInvalidRequest)
		require.ErrorContains(t, err, "must be less than the cpu capacity")
	})

	t.Run("reservations just below the VM are accepted", func(t *testing.T) {
		data := testConfig()
		data[ConfigKubeReserved] = "cpu=1"
		data[ConfigSystemReserved] = "cpu=2999m"

		require.NoError(t, resolve(newFakeCapacityClient(), data))
	})

	t.Run("premium OS disk on a size without premium storage is permanent", func(t *testing.T) {
		client := newFakeCapacityClient()
		sku := client.skus["standard_d4s_v3"]
		sku.Capabilities = append(sku.Capabilities, &armcompute.ResourceSKUCapabilities{
			Name:  new("PremiumIO"),
			Value: new("False"),
		})

		err := resolve(client, testConfig())
		requirePermanent(t, err, unboundedv1alpha3.FailureInvalidRequest)
		require.ErrorContains(t, err, "premium storage")

		// A standard disk on the same size is fine, and so is a premium disk
		// when the SKU says it supports premium storage.
		data := testConfig()
		data[ConfigOSDiskType] = "StandardSSD_LRS"
		require.NoError(t, resolve(client, data))

		sku.Capabilities[len(sku.Capabilities)-1].Value = new("True")

		require.NoError(t, resolve(client, testConfig()))
	})

	t.Run("unreadable SKU capabilities are retried", func(t *testing.T) {
		client := newFakeCapacityClient()
		client.skus["standard_d4s_v3"].Capabilities = nil

		requireTransient(t, resolve(client, testConfig()))
	})

	t.Run("SKU list errors are retried", func(t *testing.T) {
		client := newFakeCapacityClient()
		client.skuErr = errors.New("throttled")

		requireTransient(t, resolve(client, testConfig()))
	})
}

func TestCredentialCacheKeyOmitsSecret(t *testing.T) {
	auth := &machineops.OperationAuth{
		Mode: unboundedv1alpha3.MachineOperationCredentialAuthExternalPlugin,
		SecretData: map[string]string{
			"tenantID":     "tenant",
			"clientID":     "client",
			"clientSecret": "super-secret-value",
		},
	}

	key, _, err := credentialSource(auth)
	require.NoError(t, err)
	require.NotContains(t, key, "super-secret-value")

	defaultKey, _, err := credentialSource(nil)
	require.NoError(t, err)
	require.Equal(t, "default", defaultKey)

	delete(auth.SecretData, "clientSecret")

	_, _, err = credentialSource(auth)
	require.ErrorContains(t, err, "clientSecret")

	_, _, err = credentialSource(&machineops.OperationAuth{Mode: "Unknown"})
	require.ErrorContains(t, err, "unsupported Azure VM auth mode")
}

func TestCredentialCacheReusesCredential(t *testing.T) {
	var cache credentialCache

	auth := &machineops.OperationAuth{
		Mode: unboundedv1alpha3.MachineOperationCredentialAuthExternalPlugin,
		SecretData: map[string]string{
			"tenantID":     "00000000-0000-0000-0000-000000000002",
			"clientID":     "client",
			"clientSecret": "secret",
		},
	}

	first, err := cache.credential(auth)
	require.NoError(t, err)

	second, err := cache.credential(auth)
	require.NoError(t, err)
	require.Same(t, first, second)

	auth.SecretData["clientSecret"] = "rotated"

	rotated, err := cache.credential(auth)
	require.NoError(t, err)
	require.NotSame(t, first, rotated)
}
