// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build integrationtest

package inttest

// Pinned container image tags. Bump centrally when upgrading.
const (
	// garageImage is the Garage image backing the S3-compatible store
	// used for both the origin (awss3) and cachestore (s3) backends.
	// Garage persists to disk and implements plain GET/PUT/HEAD, which
	// is all Orca's stat-then-put commit needs.
	garageImage = "dxflrs/garage:v1.0.1"

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
