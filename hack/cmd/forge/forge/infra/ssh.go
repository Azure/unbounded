// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package infra

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

type SSHKeyPair struct {
	PublicKeyPath  string
	PrivateKeyPath string
	Logger         *slog.Logger
}

func (kp *SSHKeyPair) GetOrGenerate() error {
	if kp.PrivateKeyPath == "" {
		kp.PrivateKeyPath = kp.privateKeyPath()
	}

	var err error

	if kp.PublicKeyPath, err = resolvePath(kp.PublicKeyPath); err != nil {
		return fmt.Errorf("resolve public key path: %w", err)
	}

	if kp.PrivateKeyPath, err = resolvePath(kp.PrivateKeyPath); err != nil {
		return fmt.Errorf("resolve private key path: %w", err)
	}

	l := kp.Logger.With("publicKey", kp.PublicKeyPath, "privateKey", kp.PrivateKeyPath)

	if KeyPairExists(kp.PrivateKeyPath) {
		l.Info("SSH key pair already exists")
		return nil
	}

	sshDir := filepath.Dir(kp.PublicKeyPath)
	privateKeyName := filepath.Base(kp.PrivateKeyPath)

	l.Info("Generating new SSH key pair")

	if _, _, err = CreateKeyPair(4096, sshDir, privateKeyName); err != nil {
		return fmt.Errorf("failed to generate SSH key pair: %w", err)
	}

	return nil
}

func (kp *SSHKeyPair) PublicKey() ([]byte, error) {
	return readFile(kp.PublicKeyPath)
}

func (kp *SSHKeyPair) PrivateKey() ([]byte, error) {
	return readFile(kp.privateKeyPath())
}

func (kp *SSHKeyPair) privateKeyPath() string {
	return strings.TrimSuffix(kp.PublicKeyPath, ".pub")
}

func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func resolvePath(path string) (string, error) {
	if strings.HasPrefix(path, "~") {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("failed to get user home directory: %w", err)
		}

		path = strings.Replace(path, "~", homeDir, 1)
	}

	return path, nil
}

func CreateKeyPair(bits int, sshDir, privateKeyName string) (string, string, error) {
	if strings.HasPrefix(sshDir, "~") {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return "", "", fmt.Errorf("failed to get user home directory: %w", err)
		}

		sshDir = strings.Replace(sshDir, "~", homeDir, 1)
	}

	privateKeyFile := filepath.Join(sshDir, privateKeyName)

	if KeyPairExists(privateKeyFile) {
		return privateKeyFile, publicKeyPath(privateKeyFile), nil
	}

	private, err := createPrivateKey(bits)
	if err != nil {
		return "", "", fmt.Errorf("failed to create private key: %w", err)
	}

	privatePEM := privateKeyToPEM(private)

	publicPEM, err := createPublicKey(&private.PublicKey)
	if err != nil {
		return "", "", fmt.Errorf("failed to create public key: %w", err)
	}

	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		return "", "", fmt.Errorf("failed to create ssh dir %s: %w", sshDir, err)
	}

	publicKeyFile := publicKeyPath(privateKeyFile)

	var writeErr error

	defer func() {
		// cleanup partially created files (e.g. private key if public key write fails)
		if writeErr != nil {
			if err := os.Remove(privateKeyFile); err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to clean up private key file %s: %v\n", privateKeyFile, err)
			}

			if err := os.Remove(publicKeyFile); err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to clean up public key file %s: %v\n", publicKeyFile, err)
			}
		}
	}()

	if writeErr = writeKeyToFile(privateKeyFile, privatePEM); writeErr != nil {
		return "", "", fmt.Errorf("failed to write private key to %s: %w", privateKeyFile, writeErr)
	}

	if writeErr = writeKeyToFile(publicKeyFile, publicPEM); writeErr != nil {
		return "", "", fmt.Errorf("failed to write public key to %s: %w", publicKeyFile, writeErr)
	}

	return privateKeyFile, publicKeyFile, nil
}

func KeyPairExists(privateKeyPath string) bool {
	var (
		privateKeyFound = false
		publicKeyFound  = false
	)

	if _, err := os.Stat(privateKeyPath); err == nil {
		privateKeyFound = true
	}

	if _, err := os.Stat(publicKeyPath(privateKeyPath)); err == nil {
		publicKeyFound = true
	}

	return privateKeyFound && publicKeyFound
}

func createPrivateKey(bits int) (*rsa.PrivateKey, error) {
	private, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		return nil, err
	}

	if err := private.Validate(); err != nil {
		return nil, err
	}

	return private, err
}

func createPublicKey(pk *rsa.PublicKey) ([]byte, error) {
	public, err := ssh.NewPublicKey(pk)
	if err != nil {
		return nil, err
	}

	return ssh.MarshalAuthorizedKey(public), nil
}

func privateKeyToPEM(pk *rsa.PrivateKey) []byte {
	pemBlock := pem.Block{
		Type:    "RSA PRIVATE KEY",
		Headers: nil,
		Bytes:   x509.MarshalPKCS1PrivateKey(pk),
	}

	return pem.EncodeToMemory(&pemBlock)
}

func publicKeyPath(privateKeyPath string) string {
	return fmt.Sprintf("%s.pub", privateKeyPath)
}

func writeKeyToFile(f string, b []byte) error {
	return os.WriteFile(f, b, 0o600)
}
