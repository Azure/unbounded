// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package orcaseed

import (
	"context"
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
)

// azuriteWellKnownDevKey is the documented well-known shared key for
// Azurite's default account ('devstoreaccount1'). It is a public
// constant baked into Azurite, not a secret. Documented at
// https://learn.microsoft.com/azure/storage/common/storage-use-azurite.
const azuriteWellKnownDevKey = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="

// globalFlags carries the connection-shape flags that every subcommand
// honours. The defaults target the in-cluster Azurite emulator exposed
// to the host via the dev harness's NodePort 30100.
type globalFlags struct {
	endpoint        string
	account         string
	accountKey      string
	containerName   string
	ensureContainer bool
}

func defaultGlobalFlags() *globalFlags {
	return &globalFlags{
		endpoint:        "http://localhost:30100/devstoreaccount1/",
		account:         "devstoreaccount1",
		accountKey:      azuriteWellKnownDevKey,
		containerName:   "orca-test",
		ensureContainer: true,
	}
}

// newClients constructs the azblob service + container clients from
// the global flags, applies the ensure-container behaviour if
// requested, and returns the container client ready for blob
// operations.
func (g *globalFlags) newClients(ctx context.Context) (*azblob.Client, *container.Client, error) {
	if g.endpoint == "" {
		return nil, nil, fmt.Errorf("--endpoint is required")
	}

	if g.account == "" {
		return nil, nil, fmt.Errorf("--account is required")
	}

	if g.accountKey == "" {
		return nil, nil, fmt.Errorf("--account-key is required")
	}

	if g.containerName == "" {
		return nil, nil, fmt.Errorf("--container is required")
	}

	cred, err := azblob.NewSharedKeyCredential(g.account, g.accountKey)
	if err != nil {
		return nil, nil, fmt.Errorf("shared-key credential: %w", err)
	}
	// Trim a trailing slash so containerURL concatenation produces
	// the expected single-slash boundary.
	endpoint := strings.TrimRight(g.endpoint, "/")

	svc, err := azblob.NewClientWithSharedKeyCredential(endpoint, cred, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("azblob client: %w", err)
	}

	cc := svc.ServiceClient().NewContainerClient(g.containerName)

	if g.ensureContainer {
		if err := ensureContainer(ctx, cc); err != nil {
			return nil, nil, fmt.Errorf("ensure container %q: %w", g.containerName, err)
		}
	}

	return svc, cc, nil
}

// ensureContainer creates the container if it does not exist.
// ContainerAlreadyExists is treated as success so callers can invoke
// this idempotently on every run.
func ensureContainer(ctx context.Context, cc *container.Client) error {
	_, err := cc.Create(ctx, nil)
	if err == nil {
		return nil
	}

	if bloberror.HasCode(err, bloberror.ContainerAlreadyExists) {
		return nil
	}

	return err
}
