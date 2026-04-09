// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package azsdk_test

import (
	"reflect"
	"testing"

	"github.com/golang-jwt/jwt/v5"

	"github.com/Azure/unbounded-kube/hack/cmd/forge/forge/azsdk"
)

func Test_ParseTokenClaims(t *testing.T) {
	t.Parallel()

	t.Run("error_if_invalid_jwt", func(t *testing.T) {
		tok := "foo.bar.baz"
		if _, err := azsdk.GetTokenClaims(tok); err == nil {
			t.Fatal("expected error parsing invalid jwt")
		}
	})

	t.Run("parse_id_type", func(t *testing.T) {
		t.Parallel()

		expectedIDType := "aaa"

		tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"idtyp": expectedIDType})

		tokStr, err := tok.SignedString([]byte("foobar"))
		if err != nil {
			t.Fatal(err)
		}

		claims, err := azsdk.GetTokenClaims(tokStr)
		if err != nil {
			t.Fatalf("expected nil err, got: %v", err)
		}

		if claims.IDType != expectedIDType {
			t.Fatalf("expected claims.ObjectID == %q", expectedIDType)
		}
	})

	t.Run("parse_object_id", func(t *testing.T) {
		t.Parallel()

		expectedObjectID := "aaa"

		tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"oid": expectedObjectID})

		tokStr, err := tok.SignedString([]byte("foobar"))
		if err != nil {
			t.Fatal(err)
		}

		claims, err := azsdk.GetTokenClaims(tokStr)
		if err != nil {
			t.Fatalf("expected nil err, got: %v", err)
		}

		if claims.ObjectID != expectedObjectID {
			t.Fatalf("expected claims.ObjectID == %q", expectedObjectID)
		}
	})

	t.Run("parse_tenant_id_from_claims", func(t *testing.T) {
		t.Parallel()

		expectedTenantID := "aaa"

		tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"tid": expectedTenantID})

		tokStr, err := tok.SignedString([]byte("foobar"))
		if err != nil {
			t.Fatal(err)
		}

		claims, err := azsdk.GetTokenClaims(tokStr)
		if err != nil {
			t.Fatalf("expected nil err, got: %v", err)
		}

		if claims.TenantID != expectedTenantID {
			t.Fatalf("expected claims.TenantID == %q", expectedTenantID)
		}
	})

	t.Run("parse_groups", func(t *testing.T) {
		t.Parallel()

		expectedGroups := []string{"aaa", "bbb", "ccc"}

		tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"groups": expectedGroups})

		tokStr, err := tok.SignedString([]byte("foobar"))
		if err != nil {
			t.Fatal(err)
		}

		claims, err := azsdk.GetTokenClaims(tokStr)
		if err != nil {
			t.Fatalf("expected nil err, got: %v", err)
		}

		if !reflect.DeepEqual(claims.Groups, expectedGroups) {
			t.Fatalf("expected claims.Groups == %q", expectedGroups)
		}
	})
}
