// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package redfish

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLicenseRequired(t *testing.T) {
	for _, messageID := range []string{
		"iLO.2.14.LicenseKeyRequired",
		"Base.1.19.LicenseRequired",
		"SMC.1.0.OemLicenseNotPassed",
		"SMC.2.3.OemLicenseNotPassed",
	} {
		for _, envelope := range []string{
			`{"error":{"code":"%s"}}`,
			`{"error":{"code":"Base.1.0.GeneralError","@Message.ExtendedInfo":[{"MessageId":"Base.1.0.GeneralError"},{"MessageId":"%s"}]}}`,
			`{"error":{"code":"Base.1.0.GeneralError"},"@Message.ExtendedInfo":[{"MessageId":"%s"}]}`,
		} {
			t.Run(messageID+"/"+envelope, func(t *testing.T) {
				require.True(t, licenseRequired([]byte(fmt.Sprintf(envelope, messageID))))
			})
		}
	}

	for _, body := range []string{
		"", "invalid", "null", `{}`,
		`{"error":{"code":"Base.1.19.InsufficientPrivilege"}}`,
		`{"error":{"message":"OemLicenseNotPassed"}}`,
		`{"error":{"@Message.ExtendedInfo":[{"MessageId":"SMC.1.0.NotOemLicenseNotPassed"}]}}`,
		`{"@Message.ExtendedInfo":[{"MessageId":"SMC.1.0.OemLicenseNotPassed.Extra"}]}`,
	} {
		t.Run(body, func(t *testing.T) {
			require.False(t, licenseRequired([]byte(body)))
			err := actionError(http.MethodPost, "/action", sessionResponse{status: http.StatusForbidden, data: []byte(body)})
			require.Error(t, err)
			require.NotErrorIs(t, err, ErrUnsupported)
		})
	}
}
