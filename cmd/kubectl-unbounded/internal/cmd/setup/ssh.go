package setup

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/project-unbounded/unbounded-kube/cmd/kubectl-unbounded/internal/defaults"
	"github.com/project-unbounded/unbounded-kube/cmd/kubectl-unbounded/internal/util/utilk8s"
)

// generateSSHKeySecret generates an Ed25519 SSH key pair and returns the
// serialized YAML for a Secret in machina-system, labeled for auto-discovery
// by "kubectl unbounded create". It also returns the public key in
// authorized_keys format so the caller can persist it locally.
func generateSSHKeySecret() (secretYAML, pubKeyBytes []byte, err error) {
	_, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generating ed25519 key: %w", err)
	}

	privPEM, err := ssh.MarshalPrivateKey(privKey, "")
	if err != nil {
		return nil, nil, fmt.Errorf("marshalling private key: %w", err)
	}

	privBytes := pem.EncodeToMemory(privPEM)

	pubKey, err := ssh.NewPublicKey(privKey.Public())
	if err != nil {
		return nil, nil, fmt.Errorf("creating SSH public key: %w", err)
	}

	pubBytes := ssh.MarshalAuthorizedKey(pubKey)

	secret := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: defaults.SSHSecretNamespace,
			Name:      defaults.SSHSecretName,
			Labels: map[string]string{
				defaults.LabelKeyDefaultSSHSecret: defaults.LabelValueDefaultSSHSecret,
			},
		},
		Data: map[string][]byte{
			"ssh-privatekey": privBytes,
			"ssh-publickey":  pubBytes,
		},
	}

	yamlBytes, err := sigsyaml.Marshal(secret)
	if err != nil {
		return nil, nil, fmt.Errorf("marshalling SSH key secret: %w", err)
	}

	return append([]byte("---\n"), yamlBytes...), pubBytes, nil
}

// importSSHKeySecret reads an existing SSH private key from keyFile, validates
// it, derives the public key, and returns the serialized YAML for a Secret in
// machina-system with the same structure as generateSSHKeySecret.
func importSSHKeySecret(keyFile string) ([]byte, error) {
	privBytes, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("reading SSH private key file %s: %w", keyFile, err)
	}

	signer, err := ssh.ParsePrivateKey(privBytes)
	if err != nil {
		return nil, fmt.Errorf("parsing SSH private key from %s: %w", keyFile, err)
	}

	pubBytes := ssh.MarshalAuthorizedKey(signer.PublicKey())

	secret := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: defaults.SSHSecretNamespace,
			Name:      defaults.SSHSecretName,
			Labels: map[string]string{
				defaults.LabelKeyDefaultSSHSecret: defaults.LabelValueDefaultSSHSecret,
			},
		},
		Data: map[string][]byte{
			"ssh-privatekey": privBytes,
			"ssh-publickey":  pubBytes,
		},
	}

	yamlBytes, err := sigsyaml.Marshal(secret)
	if err != nil {
		return nil, fmt.Errorf("marshalling SSH key secret: %w", err)
	}

	return append([]byte("---\n"), yamlBytes...), nil
}

// ensureSSHKeySecret appends an SSH key secret YAML to data. When sshPrivateKey
// is set, the key is read from that file instead of generating a new one.
// Otherwise, a new Ed25519 key pair is generated and the public key is saved to
// ./unbounded_ed25519.pub. In both cases, if the secret already exists in the
// cluster (and not --print-only), the operation is skipped.
func ensureSSHKeySecret(ctx context.Context, configFlags *genericclioptions.ConfigFlags, printOnly bool, sshPrivateKey string, streams genericiooptions.IOStreams, data []byte) ([]byte, error) {
	if !printOnly {
		clientset, err := utilk8s.NewClientsetFromCLIOpts(configFlags)
		if err != nil {
			return nil, fmt.Errorf("creating kubernetes clientset: %w", err)
		}

		_, err = clientset.CoreV1().Secrets(defaults.SSHSecretNamespace).Get(ctx, defaults.SSHSecretName, metav1.GetOptions{})
		if err == nil {
			fmt.Fprintf(streams.ErrOut, "SSH key secret %s/%s already exists, skipping generation\n",
				defaults.SSHSecretNamespace, defaults.SSHSecretName)

			return data, nil
		}
	}

	// Import an existing private key from file.
	if sshPrivateKey != "" {
		sshData, err := importSSHKeySecret(sshPrivateKey)
		if err != nil {
			return nil, err
		}

		fmt.Fprintf(streams.ErrOut, "SSH private key imported from %s\n", sshPrivateKey)

		return append(data, sshData...), nil
	}

	// Generate a new key pair.
	sshData, pubKeyBytes, err := generateSSHKeySecret()
	if err != nil {
		return nil, err
	}

	// Save the public key locally so the user can provision it on target machines.
	pubKeyFile, err := filepath.Abs("unbounded_ed25519.pub")
	if err != nil {
		return nil, fmt.Errorf("resolving public key path: %w", err)
	}

	if err := os.WriteFile(pubKeyFile, pubKeyBytes, 0o644); err != nil {
		return nil, fmt.Errorf("writing public key to %s: %w", pubKeyFile, err)
	}

	fmt.Fprintf(streams.ErrOut, "SSH public key saved to %s\n", pubKeyFile)

	return append(data, sshData...), nil
}
