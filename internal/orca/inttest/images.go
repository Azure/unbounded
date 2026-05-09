// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integrationtest

package inttest

// Pinned container image tags. Bump centrally when upgrading.
const (
	// localstackImage is the LocalStack image used for both the origin
	// (awss3) and cachestore (s3) backends. Pinned to 3.8 because
	// later LocalStack tags require the AWS SDK CRC64NVME checksum
	// opt-out (which the cachestore/s3 driver and this harness's S3
	// client builder both apply).
	localstackImage = "localstack/localstack:3.8"

	// azuriteImage is the Azurite (Azure Blob emulator) image. We pin
	// to a specific minor for reproducibility.
	azuriteImage = "mcr.microsoft.com/azure-storage/azurite:3.34.0"

	// azuritePort is the blob-service port published by Azurite.
	azuritePort = "10000"

	// azuriteAccountName is the well-known Azurite dev account.
	azuriteAccountName = "devstoreaccount1"

	// azuriteAccountKey is the well-known Azurite dev account key. It
	// is hard-coded by the emulator; not a secret.
	azuriteAccountKey = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="
)
