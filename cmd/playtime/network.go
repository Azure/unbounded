// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/curve25519"
)

// ensureKeypair loads the WireGuard keypair from the state directory, creating
// a new Curve25519 keypair if one does not already exist. It returns the base64
// public key.
func ensureKeypair(cfg Config) (string, error) {
	if data, err := os.ReadFile(cfg.pubKeyPath()); err == nil {
		return strings.TrimSpace(string(data)), nil
	}

	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return "", fmt.Errorf("create state dir: %w", err)
	}

	var priv [32]byte
	if _, err := rand.Read(priv[:]); err != nil {
		return "", fmt.Errorf("generate private key: %w", err)
	}

	priv[0] &= 248
	priv[31] &= 127
	priv[31] |= 64

	pub, err := curve25519.X25519(priv[:], curve25519.Basepoint)
	if err != nil {
		return "", fmt.Errorf("derive public key: %w", err)
	}

	privB64 := base64.StdEncoding.EncodeToString(priv[:])
	pubB64 := base64.StdEncoding.EncodeToString(pub)

	if err := os.WriteFile(cfg.privKeyPath(), []byte(privB64+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write private key: %w", err)
	}

	if err := os.WriteFile(cfg.pubKeyPath(), []byte(pubB64+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write public key: %w", err)
	}

	return pubB64, nil
}

// loadPrivateKeyHex reads the stored base64 WireGuard private key and returns it
// hex-encoded for the wireguard-go UAPI.
func loadPrivateKeyHex(cfg Config) (string, error) {
	data, err := os.ReadFile(cfg.privKeyPath())
	if err != nil {
		return "", fmt.Errorf("read private key: %w", err)
	}

	return wgKeyBase64ToHex(strings.TrimSpace(string(data)))
}
