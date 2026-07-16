// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/Azure/unbounded/hack/acr-credential-provider/internal/acrcredentialprovider"
)

func main() {
	credentialSource, err := acrcredentialprovider.NewDefaultAzureCredentialSource()
	if err != nil {
		fmt.Fprintf(os.Stderr, "create Azure credential source: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	provider := acrcredentialprovider.Provider{CredentialSource: credentialSource}
	if err := provider.Handle(ctx, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "handle credential provider request: %v\n", err)
		os.Exit(1)
	}
}
