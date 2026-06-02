// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integrationtest

package inttest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/pageblob"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Azurite is a running Azurite container with helper accessors for
// constructing azblob clients pointed at the well-known dev account.
type Azurite struct {
	container testcontainers.Container
	endpoint  string // http://host:port/devstoreaccount1
}

// Endpoint returns the Azurite blob-service URL including the
// devstoreaccount1 path segment.
func (az *Azurite) Endpoint() string { return az.endpoint }

// AccountName returns the well-known Azurite dev account name.
func (az *Azurite) AccountName() string { return azuriteAccountName }

// AccountKey returns the well-known Azurite dev account key.
func (az *Azurite) AccountKey() string { return azuriteAccountKey }

// StartAzurite launches an Azurite container and returns once the
// blob-service port is reachable. Caller terminates via Terminate or
// t.Cleanup.
func StartAzurite(ctx context.Context) (*Azurite, error) {
	req := testcontainers.ContainerRequest{
		Image:        azuriteImage,
		ExposedPorts: []string{azuritePort + "/tcp"},
		// `azurite-blob` listens on 0.0.0.0 by default; --skipApiVersionCheck
		// keeps the SDK happy for newer client versions.
		Cmd:        []string{"azurite-blob", "--blobHost", "0.0.0.0", "--skipApiVersionCheck"},
		WaitingFor: wait.ForListeningPort(azuritePort + "/tcp"),
	}

	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return nil, fmt.Errorf("start azurite: %w", err)
	}

	host, err := c.Host(ctx)
	if err != nil {
		_ = c.Terminate(ctx) //nolint:errcheck // best-effort cleanup
		return nil, fmt.Errorf("azurite host: %w", err)
	}

	port, err := c.MappedPort(ctx, azuritePort+"/tcp")
	if err != nil {
		_ = c.Terminate(ctx) //nolint:errcheck // best-effort cleanup
		return nil, fmt.Errorf("azurite port: %w", err)
	}

	endpoint := fmt.Sprintf("http://%s:%s/%s", host, port.Port(), azuriteAccountName)

	return &Azurite{
		container: c,
		endpoint:  endpoint,
	}, nil
}

// Terminate stops and removes the Azurite container.
func (az *Azurite) Terminate(ctx context.Context) error {
	return az.container.Terminate(ctx)
}

// NewServiceClient returns an azblob.Client authenticated with the
// well-known Azurite dev creds.
func (az *Azurite) NewServiceClient(t *testing.T) *azblob.Client {
	t.Helper()

	cred, err := azblob.NewSharedKeyCredential(az.AccountName(), az.AccountKey())
	if err != nil {
		t.Fatalf("azurite shared key cred: %v", err)
	}

	cli, err := azblob.NewClientWithSharedKeyCredential(az.endpoint, cred, nil)
	if err != nil {
		t.Fatalf("azurite client: %v", err)
	}

	return cli
}

// NewContainer creates a fresh container and registers a cleanup. The
// container name is returned.
func (az *Azurite) NewContainer(ctx context.Context, t *testing.T, prefix string) string {
	t.Helper()

	cli := az.NewServiceClient(t)
	name := uniqueName(prefix)

	if _, err := cli.CreateContainer(ctx, name, nil); err != nil {
		t.Fatalf("create container %s: %v", name, err)
	}

	t.Cleanup(func() {
		_, _ = cli.DeleteContainer(context.Background(), name, nil) //nolint:errcheck // best-effort cleanup
	})

	return name
}

// UploadBlockBlob uploads bytes as a block blob to (container, name).
func (az *Azurite) UploadBlockBlob(ctx context.Context, t *testing.T, ctr, name string, data []byte) {
	t.Helper()

	cli := az.NewServiceClient(t)
	if _, err := cli.UploadBuffer(ctx, ctr, name, data, nil); err != nil {
		t.Fatalf("upload block blob %s/%s: %v", ctr, name, err)
	}
}

// UploadPageBlob uploads bytes as a page blob (used to exercise the
// unsupported-blob-type rejection path in the azureblob driver). Size
// must be a multiple of 512.
func (az *Azurite) UploadPageBlob(ctx context.Context, t *testing.T, ctr, name string, size int64) {
	t.Helper()

	cred, err := azblob.NewSharedKeyCredential(az.AccountName(), az.AccountKey())
	if err != nil {
		t.Fatalf("azurite shared key cred: %v", err)
	}

	containerCli, err := container.NewClientWithSharedKeyCredential(
		fmt.Sprintf("%s/%s", az.endpoint, ctr), cred, nil)
	if err != nil {
		t.Fatalf("container client: %v", err)
	}

	pbCli := containerCli.NewPageBlobClient(name)
	if _, err := pbCli.Create(ctx, size, &pageblob.CreateOptions{
		HTTPHeaders: &blob.HTTPHeaders{},
	}); err != nil {
		t.Fatalf("create page blob: %v", err)
	}
	// Page blobs created here are zero-filled; tests don't read content
	// because the azureblob driver rejects non-Block-Blob types before
	// the GET stage.
}

// uniqueName returns a short random-suffixed name suitable for
// Garage buckets and Azurite containers.
func uniqueName(prefix string) string {
	var b [4]byte

	_, _ = rand.Read(b[:]) //nolint:errcheck // crypto/rand never fails on linux

	return fmt.Sprintf("%s-%s", prefix, hex.EncodeToString(b[:]))
}
