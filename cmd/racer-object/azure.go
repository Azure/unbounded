// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"flag"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/service"
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
		return validateAzureAccountURL(o.accountURL, o.azureCredential)
	}

	return nil
}

func validateAzureAccountURL(value, credential string) error {
	if credential != "default" && credential != "anonymous" {
		return errors.New("--azure-credential must be default or anonymous")
	}

	invalid := errors.New("--account-url must be an HTTP(S) account URL without credentials, query, fragment, or more than one simple path segment")

	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
		u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(value, "#") ||
		strings.Contains(u.Host, "%") || strings.HasSuffix(u.Host, ":") {
		return invalid
	}

	if strings.Contains(u.Hostname(), ":") || strings.HasPrefix(u.Host, "[") {
		if !strings.HasPrefix(u.Host, "[") || net.ParseIP(u.Hostname()) == nil {
			return invalid
		}
	}

	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("invalid --account-url port")
		}
	}
	// Only plain account names are allowed in an Azurite path prefix.
	segment := strings.TrimSuffix(strings.TrimPrefix(u.EscapedPath(), "/"), "/")
	if strings.IndexFunc(segment, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_'
	}) != -1 || u.EscapedPath() == "//" {
		return invalid
	}

	if credential == "default" && u.Scheme != "https" {
		return errors.New("--account-url requires HTTPS with --azure-credential=default")
	}

	return nil
}

func originAzureClientOptions(o options) azcore.ClientOptions {
	return azcore.ClientOptions{
		Transport: &http.Client{Timeout: o.requestTimeout},
		Telemetry: policy.TelemetryOptions{ApplicationID: "racer-object"},
	}
}

func originAzureClient(o options) (*service.Client, error) {
	if err := validateAzureAccountURL(o.accountURL, o.azureCredential); err != nil {
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
