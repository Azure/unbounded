// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package authority_test

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/Azure/unbounded/internal/racer/authority"
	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestPublicAuthorityScaffoldAndOpaqueValues(t *testing.T) {
	a := authority.New(authority.Config{}, authority.Dependencies{})
	if !errors.Is(a.TrustReady(), wire.Unavailable) || !errors.Is(a.PublicationReady(), wire.Unavailable) {
		t.Fatal("constructor granted authority")
	}

	if !errors.Is(a.Recover(t.Context(), nil), wire.InvalidRequest) {
		t.Fatal("zero configuration reached I/O")
	}

	if _, err := a.Issue(t.Context(), authority.NodeIdentity{}, wire.BootstrapRequest{}); !errors.Is(err, wire.Unauthenticated) {
		t.Fatal("zero identity accepted", err)
	}

	if _, err := a.Wait(t.Context(), authority.NodeIdentity{}, nil); !errors.Is(err, wire.Unauthenticated) {
		t.Fatal("zero poll identity accepted", err)
	}

	var handle authority.PublicationHandle
	if _, _, err := handle.WriteContext(t.Context()); !errors.Is(err, wire.Unavailable) {
		t.Fatal("zero publication handle accepted", err)
	}

	if _, err := handle.ForBase("").WriteTo(context.Background(), io.Discard); !errors.Is(err, wire.Forbidden) {
		t.Fatal("unguarded response accepted", err)
	}

	for _, value := range []any{authority.NodeIdentity{}, authority.ReplicaIdentity{}, authority.PublicationHandle{}, authority.KeyringHandle{}, authority.Response{}, *a} {
		typeOf := reflect.TypeOf(value)
		for i := range typeOf.NumField() {
			if typeOf.Field(i).IsExported() {
				t.Fatalf("%s exposes mutable field %s", typeOf.Name(), typeOf.Field(i).Name)
			}
		}
	}
}
