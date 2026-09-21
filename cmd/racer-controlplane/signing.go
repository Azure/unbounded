// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pb "github.com/Azure/unbounded/api/racer"
)

// Generate a new key pair without replacing any existing key material.
func generateKey(dir string) (err error) {
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		return err
	}
	defer clear(private)

	if err = os.Mkdir(dir, 0o700); err != nil {
		return err
	}

	defer func() {
		if err != nil {
			err = errors.Join(err, os.RemoveAll(dir))
		}
	}()

	seed := private.Seed()
	defer clear(seed)

	if err = os.WriteFile(filepath.Join(dir, "seed"), seed, 0o600); err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(dir, "public"), public, 0o644)
}

func managedSigningEnabled() bool {
	return true
}

func signerFromEnv(ctx context.Context, c client.Client, namespace string) (*signer, error) {
	if _, set := os.LookupEnv("RACER_SIGNING_KEY"); set {
		return nil, errors.New("controller RACER_SIGNING_KEY is removed; use managed signing Secrets")
	}

	return ensureManagedSigning(ctx, c, namespace)
}

// A signer is immutable once installed in a Server.
type signer struct {
	key ed25519.PrivateKey
	id  [sha256.Size]byte
}

func readSigner(r io.Reader) (*signer, error) {
	seed, err := io.ReadAll(io.LimitReader(r, ed25519.SeedSize+1))
	defer clear(seed)

	if err != nil {
		return nil, err
	}

	if len(seed) != ed25519.SeedSize {
		return nil, errors.New("signing key must contain exactly 32 raw bytes")
	}

	key := ed25519.NewKeyFromSeed(seed)
	id := sha256.Sum256(append([]byte("racer/public-key/v2"), key[ed25519.SeedSize:]...))

	return &signer{key: key, id: id}, nil
}

func (s *signer) sign(snapshot []byte) []byte {
	return s.signDomain("racer/config/v2", snapshot)
}

func (s *signer) signDomain(domain string, snapshot []byte) []byte {
	message := make([]byte, 0, 9+8+len(domain)+8+len(snapshot))
	message = append(message, "RACERSIG2"...)
	message = binary.BigEndian.AppendUint64(message, uint64(len(domain)))
	message = append(message, domain...)
	message = binary.BigEndian.AppendUint64(message, uint64(len(snapshot)))
	message = append(message, snapshot...)

	return append(append([]byte(nil), s.id[:]...), ed25519.Sign(s.key, message)...)
}

func configuration(snapshot []byte, key *signer) ([]byte, error) {
	if key == nil {
		// Configuration.snapshot is field 1. Embed the already serialized
		// message verbatim, just as SignedSnapshot.snapshot does below.
		return protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), snapshot), nil
	}

	return (proto.MarshalOptions{Deterministic: true}).Marshal(&pb.Configuration{
		Contents: &pb.Configuration_Signed{Signed: &pb.SignedSnapshot{
			Snapshot: snapshot, Signature: key.sign(snapshot),
		}},
	})
}
