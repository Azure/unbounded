// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package redfish

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCompletionChecksAllVendors(t *testing.T) {
	for _, nested := range []bool{false, true} {
		for _, tc := range []struct {
			name string
			oem  string
			want string
		}{
			{"successThenFailure", `"A":{"CompletionCode":"Success"},"Z":{"CompletionCode":"Failure","Description":"last vendor failed"}`, "last vendor failed"},
			{"failureThenSuccess", `"A":{"CompletionCode":"Failure","Description":"first vendor failed"},"Z":{"CompletionCode":"Success"}`, "first vendor failed"},
			{"firstFailureInKeyOrder", `"Z":{"CompletionCode":"Failure","Description":"last vendor failed"},"A":{"CompletionCode":"Failure","Description":"first vendor failed"}`, "first vendor failed"},
			{"laterMessage", `"A":{"CompletionCode":"Success"}}},{"Oem":{"Z":{"CompletionCode":"Failure","Description":"later message failed"}`, "later message failed"},
			{"invalidCode", `"A":{"CompletionCode":"Success"},"Z":{"CompletionCode":4}`, "parsing Z completion code"},
			{"nullCode", `"A":{"CompletionCode":"Success"},"Z":{"CompletionCode":null}`, "parsing Z completion code"},
			{"unknownCode", `"A":{"CompletionCode":"Unknown"}`, `unexpected A completion code "Unknown"`},
			{"emptyCode", `"A":{"CompletionCode":""},"Z":{"CompletionCode":"Success"}`, `unexpected A completion code ""`},
			{"unknownLaterCode", `"A":{"CompletionCode":"Success"},"Z":{"CompletionCode":"Unknown"}`, `unexpected Z completion code "Unknown"`},
		} {
			t.Run(fmt.Sprintf("%s/nested=%t", tc.name, nested), func(t *testing.T) {
				body := `{"@Message.ExtendedInfo":[{"Oem":{` + tc.oem + `}}]}`
				if nested {
					body = `{"error":` + body + `}`
				}

				require.ErrorContains(t, checkCompletion([]byte(body)), tc.want)

				srv := rackServer(t, func(w http.ResponseWriter, _ *http.Request) {
					_, _ = w.Write([]byte(body))
				})
				c := dialRack(t, srv, "7")
				_, err := rackRequest(t.Context(), c.session, http.MethodPatch, rackSystemPath, nil)

				var response *responseError
				require.ErrorAs(t, err, &response)
				require.ErrorContains(t, response.Unwrap(), tc.want)
			})
		}
	}
}

func TestCompletionErrorEnvelopePolicy(t *testing.T) {
	for _, tc := range []struct {
		name      string
		oem       string
		wantError bool
	}{
		{"success", `"A":{"CompletionCode":"Success"},"Z":{"CompletionCode":"Success"}`, false},
		{"noCode", `"A":{}`, true},
		{"optionalDescription", `"A":{"CompletionCode":"Success","Description":4}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// An error envelope without a code is only rejected for rack managers.
			body := `{"error":{"@Message.ExtendedInfo":[{"Oem":{` + tc.oem + `}}]}}`
			require.NoError(t, checkCompletion([]byte(body)))
			srv := rackServer(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			})
			c := dialRack(t, srv, "7")

			_, err := rackRequest(t.Context(), c.session, http.MethodPatch, rackSystemPath, nil)
			if tc.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
