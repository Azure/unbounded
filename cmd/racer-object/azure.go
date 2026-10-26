// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"flag"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/service"

	"github.com/Azure/unbounded/internal/racerobject"
)

// validateProviderOptions checks provider-specific origin flags. For azure it
// sets o.bucket to the container named by --container-url.
func validateProviderOptions(o *options, fs *flag.FlagSet) error {
	if o.provider != "s3" && o.provider != "azure" {
		return errors.New("--provider must be s3 or azure")
	}

	var conflict error

	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "region", "endpoint", "path-style", "bucket":
			if o.provider == "azure" {
				conflict = errors.New("--bucket, --region, --endpoint, and --path-style are only valid for s3")
			}
		case "container-url", "azure-credential":
			if o.provider == "s3" {
				conflict = errors.New("--container-url and --azure-credential are only valid for azure")
			}
		}
	})

	if conflict != nil {
		return conflict
	}

	if o.provider == "azure" {
		if o.containerURL == "" {
			return errors.New("--container-url is required for azure")
		}

		_, container, err := racerobject.ParseAzureContainerURL(o.containerURL, o.azureCredential)
		if err != nil {
			return fmt.Errorf("invalid --container-url: %w", err)
		}

		o.bucket = bucketFlag(container)

		return nil
	}

	if o.bucket == "" {
		return errors.New("--bucket is required for s3")
	}

	if err := racerobject.ValidateBucket(string(o.bucket)); err != nil {
		return errors.New("invalid --bucket")
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
	accountURL, _, err := racerobject.ParseAzureContainerURL(o.containerURL, o.azureCredential)
	if err != nil {
		return nil, fmt.Errorf("invalid --container-url: %w", err)
	}

	clientOptions := originAzureClientOptions(o)
	storageOptions := &service.ClientOptions{ClientOptions: clientOptions}

	var client *service.Client

	if o.azureCredential == "anonymous" {
		client, err = service.NewClientWithNoCredential(accountURL, storageOptions)
	} else {
		credential, credentialErr := azidentity.NewDefaultAzureCredential(&azidentity.DefaultAzureCredentialOptions{ClientOptions: clientOptions})
		if credentialErr != nil {
			return nil, errors.New("could not configure Azure default credential")
		}

		client, err = service.NewClient(accountURL, credential, storageOptions)
	}

	if err != nil {
		return nil, errors.New("could not configure Azure client")
	}

	return client, nil
}
