// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"flag"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/service"

	"github.com/Azure/unbounded/internal/racerobject"
)

func validateProviderOptions(o options, fs *flag.FlagSet) error {
	if o.provider != "s3" && o.provider != "azure" {
		return errors.New("--provider must be s3 or azure")
	}

	var conflict error

	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "region", "endpoint", "path-style":
			if o.provider == "azure" {
				conflict = errors.New("--region, --endpoint, and --path-style are only valid for s3")
			}
		case "account-url", "azure-credential":
			if o.provider == "s3" {
				conflict = errors.New("--account-url and --azure-credential are only valid for azure")
			}
		}
	})

	if conflict != nil {
		return conflict
	}

	if o.provider == "azure" {
		return racerobject.ValidateAzureAccountURL(o.accountURL, o.azureCredential)
	}

	return nil
}

func originAzureClientOptions(o options) azcore.ClientOptions {
	return azcore.ClientOptions{
		Transport: originHTTPClient(o),
		Telemetry: policy.TelemetryOptions{ApplicationID: "racer-object"},
	}
}

func originAzureClient(o options) (*service.Client, error) {
	if err := racerobject.ValidateAzureAccountURL(o.accountURL, o.azureCredential); err != nil {
		return nil, err
	}

	clientOptions := originAzureClientOptions(o)
	storageOptions := &service.ClientOptions{ClientOptions: clientOptions}

	var (
		client *service.Client
		err    error
	)

	if o.azureCredential == "anonymous" {
		client, err = service.NewClientWithNoCredential(o.accountURL, storageOptions)
	} else {
		credential, credentialErr := azidentity.NewDefaultAzureCredential(&azidentity.DefaultAzureCredentialOptions{ClientOptions: clientOptions})
		if credentialErr != nil {
			return nil, errors.New("could not configure Azure default credential")
		}

		client, err = service.NewClient(o.accountURL, credential, storageOptions)
	}

	if err != nil {
		return nil, errors.New("could not configure Azure client")
	}

	return client, nil
}
