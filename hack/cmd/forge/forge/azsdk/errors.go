// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package azsdk

import (
	"errors"
	"net/http"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
)

// IsNotFoundError checks if a 404 / Not Found error from the Azure Resource Manager. Also supports the Key Vault
// DeletedVaultNotFound error.
func IsNotFoundError(err error) bool {
	if err == nil {
		return false
	}

	if exportedResponseError, ok := azureSDKErrorIs(err, http.StatusNotFound); ok || (exportedResponseError != nil && exportedResponseError.ErrorCode == "DeletedVaultNotFound") {
		return true
	}

	return false
}

func azureSDKErrorIs(err error, code int) (*azcore.ResponseError, bool) {
	re := &azcore.ResponseError{}

	if errors.As(err, &re) {
		return re, re.StatusCode == code
	}

	return nil, false
}
