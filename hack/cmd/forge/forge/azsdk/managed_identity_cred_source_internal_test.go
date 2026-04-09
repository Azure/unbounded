// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package azsdk

import (
	"reflect"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

func Test_ManagedIdentityCredential_Configure(t *testing.T) {
	t.Parallel()

	tt := map[string]struct {
		clientID     string
		expectedKind azidentity.ManagedIDKind
		imdsTimeout  time.Duration
	}{
		"byClientID": {
			clientID:     "client-id",
			expectedKind: azidentity.ClientID("client-id"),
		},
		"byResourceID": {
			clientID:     "/subscriptions/sub-id/resourceGroups/rg-name/providers/Microsoft.ManagedIdentity/userAssignedIdentities/identity-name",
			expectedKind: azidentity.ResourceID("/subscriptions/sub-id/resourceGroups/rg-name/providers/Microsoft.ManagedIdentity/userAssignedIdentities/identity-name"),
			imdsTimeout:  time.Second * 5,
		},
		"emptyClientID": {
			clientID:     "",
			expectedKind: nil,
			imdsTimeout:  time.Second * 5,
		},
	}

	for name, tc := range tt {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			c := &ManagedIdentityCredential{
				ClientID:    tc.clientID,
				IMDSTimeout: tc.imdsTimeout,
			}

			credential, err := c.Configure(AuthConfig{})
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			// Get the internal properties of the credential
			wrapper := credential.(*managedIdentityCredentialWrapper)

			if tc.expectedKind == nil && wrapper.id != nil {
				t.Errorf("Configure() got id = %v, want nil", wrapper.id)
			}

			if wrapper.id != nil && reflect.TypeOf(wrapper.id) != reflect.TypeOf(tc.expectedKind) {
				t.Errorf("Configure() got type = %T, want %T", wrapper.id, tc.expectedKind)
			}

			if wrapper.id != nil && wrapper.id.String() != tc.expectedKind.String() {
				t.Errorf("Configure() got = %v, want %v", wrapper.id, tc.expectedKind)
			}

			if wrapper.imdsTimeout != tc.imdsTimeout {
				t.Errorf("Configure() got timeout = %v, want %v", wrapper.imdsTimeout, tc.imdsTimeout)
			}
		})
	}
}
