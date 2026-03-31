package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// writeTempSSHKey writes dummy SSH key content to a temp file and returns the path.
func writeTempSSHKey(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "id_ed25519")
	require.NoError(t, os.WriteFile(path, []byte("fake-ssh-private-key-content"), 0o600))

	return path
}

// writeTempKubeconfig writes a minimal kubeconfig to a temp file and returns the path.
func writeTempKubeconfig(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "kubeconfig")
	// Minimal valid kubeconfig content — only needs to be a readable file for validate().
	require.NoError(t, os.WriteFile(path, []byte("apiVersion: v1\nkind: Config\n"), 0o600))

	return path
}

// ---------------------------------------------------------------------------
// setDefaults() tests
// ---------------------------------------------------------------------------

func TestSiteAddMachineHandler_SetDefaults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		before siteAddMachineHandler
		check  func(t *testing.T, h *siteAddMachineHandler)
	}{
		{
			name: "name derived from host IP",
			before: siteAddMachineHandler{
				siteName: "dc1",
				host:     "10.0.0.5",
			},
			check: func(t *testing.T, h *siteAddMachineHandler) {
				require.Equal(t, "dc1-10.0.0.5", h.name)
			},
		},
		{
			name: "name derived from host IP:port",
			before: siteAddMachineHandler{
				siteName: "dc1",
				host:     "10.0.0.5:2222",
			},
			check: func(t *testing.T, h *siteAddMachineHandler) {
				require.Equal(t, "dc1-10.0.0.5-2222", h.name)
			},
		},
		{
			name: "name derived from hostname",
			before: siteAddMachineHandler{
				siteName: "dc1",
				host:     "my-server.example.com",
			},
			check: func(t *testing.T, h *siteAddMachineHandler) {
				require.Equal(t, "dc1-my-server.example.com", h.name)
			},
		},
		{
			name: "name derived from hostname with special chars",
			before: siteAddMachineHandler{
				siteName: "dc1",
				host:     "My_Server:2222",
			},
			check: func(t *testing.T, h *siteAddMachineHandler) {
				require.Equal(t, "dc1-my-server-2222", h.name)
			},
		},
		{
			name: "explicit name gets site prefix",
			before: siteAddMachineHandler{
				siteName: "dc1",
				name:     "worker-1",
				host:     "10.0.0.5",
			},
			check: func(t *testing.T, h *siteAddMachineHandler) {
				require.Equal(t, "dc1-worker-1", h.name)
			},
		},
		{
			name: "ssh secret name defaults to ssh-site",
			before: siteAddMachineHandler{
				siteName: "dc1",
				host:     "10.0.0.5",
			},
			check: func(t *testing.T, h *siteAddMachineHandler) {
				require.Equal(t, "ssh-dc1", h.sshSecretName)
			},
		},
		{
			name: "ssh secret name preserved when explicit",
			before: siteAddMachineHandler{
				siteName:      "dc1",
				host:          "10.0.0.5",
				sshSecretName: "my-custom-secret",
			},
			check: func(t *testing.T, h *siteAddMachineHandler) {
				require.Equal(t, "my-custom-secret", h.sshSecretName)
			},
		},
		{
			name: "bastion username defaults to host ssh username",
			before: siteAddMachineHandler{
				siteName:        "dc1",
				host:            "10.0.0.5",
				hostSSHUsername: "admin",
				bastionHost:     "5.6.7.8",
			},
			check: func(t *testing.T, h *siteAddMachineHandler) {
				require.Equal(t, "admin", h.bastionSSHUsername)
			},
		},
		{
			name: "bastion ssh private key defaults to host ssh private key",
			before: siteAddMachineHandler{
				siteName:          "dc1",
				host:              "10.0.0.5",
				hostSSHPrivateKey: "/path/to/key",
				bastionHost:       "5.6.7.8",
			},
			check: func(t *testing.T, h *siteAddMachineHandler) {
				require.Equal(t, "/path/to/key", h.bastionSSHPrivateKey)
			},
		},
		{
			name: "bastion secret name defaults to ssh secret name",
			before: siteAddMachineHandler{
				siteName:    "dc1",
				host:        "10.0.0.5",
				bastionHost: "5.6.7.8",
			},
			check: func(t *testing.T, h *siteAddMachineHandler) {
				require.Equal(t, "ssh-dc1", h.bastionSSHSecretName)
			},
		},
		{
			name: "no bastion defaults when no bastion host",
			before: siteAddMachineHandler{
				siteName:        "dc1",
				host:            "10.0.0.5",
				hostSSHUsername: "admin",
			},
			check: func(t *testing.T, h *siteAddMachineHandler) {
				require.Empty(t, h.bastionSSHUsername)
				require.Empty(t, h.bastionSSHPrivateKey)
				require.Empty(t, h.bastionSSHSecretName)
			},
		},
		{
			name: "bastion explicit values preserved",
			before: siteAddMachineHandler{
				siteName:             "dc1",
				host:                 "10.0.0.5",
				hostSSHUsername:      "admin",
				hostSSHPrivateKey:    "/path/to/host-key",
				bastionHost:          "5.6.7.8",
				bastionSSHUsername:   "bastion-user",
				bastionSSHPrivateKey: "/path/to/bastion-key",
				bastionSSHSecretName: "bastion-secret",
			},
			check: func(t *testing.T, h *siteAddMachineHandler) {
				require.Equal(t, "bastion-user", h.bastionSSHUsername)
				require.Equal(t, "/path/to/bastion-key", h.bastionSSHPrivateKey)
				require.Equal(t, "bastion-secret", h.bastionSSHSecretName)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := tt.before
			h.setDefaults()
			tt.check(t, &h)
		})
	}
}

// ---------------------------------------------------------------------------
// validate() tests
// ---------------------------------------------------------------------------

func TestSiteAddMachineHandler_Validate(t *testing.T) {
	t.Parallel()

	sshKeyPath := writeTempSSHKey(t)
	kubeconfigPath := writeTempKubeconfig(t)

	tests := []struct {
		name      string
		handler   siteAddMachineHandler
		expectErr string
	}{
		{
			name: "valid: all required fields",
			handler: siteAddMachineHandler{
				siteName:          "dc1",
				name:              "dc1-10.0.0.5",
				host:              "10.0.0.5",
				hostSSHUsername:   "admin",
				hostSSHPrivateKey: sshKeyPath,
				sshSecretName:     "ssh-dc1",
				kubeconfigPath:    kubeconfigPath,
			},
		},
		{
			name: "valid: with bastion",
			handler: siteAddMachineHandler{
				siteName:             "dc1",
				name:                 "dc1-10.0.0.5",
				host:                 "10.0.0.5",
				hostSSHUsername:      "admin",
				hostSSHPrivateKey:    sshKeyPath,
				sshSecretName:        "ssh-dc1",
				bastionHost:          "5.6.7.8",
				bastionSSHUsername:   "admin",
				bastionSSHPrivateKey: sshKeyPath,
				bastionSSHSecretName: "ssh-dc1",
				kubeconfigPath:       kubeconfigPath,
			},
		},
		{
			name: "valid: bastion only (no host SSH key)",
			handler: siteAddMachineHandler{
				siteName:             "dc1",
				name:                 "dc1-10.0.0.5",
				host:                 "10.0.0.5",
				hostSSHUsername:      "admin",
				sshSecretName:        "ssh-dc1",
				bastionHost:          "5.6.7.8",
				bastionSSHUsername:   "admin",
				bastionSSHPrivateKey: sshKeyPath,
				bastionSSHSecretName: "bastion-ssh",
				kubeconfigPath:       kubeconfigPath,
			},
		},
		{
			name: "missing site name",
			handler: siteAddMachineHandler{
				host:              "10.0.0.5",
				hostSSHUsername:   "admin",
				hostSSHPrivateKey: sshKeyPath,
				kubeconfigPath:    kubeconfigPath,
			},
			expectErr: "site name is required",
		},
		{
			name: "missing host",
			handler: siteAddMachineHandler{
				siteName:          "dc1",
				hostSSHUsername:   "admin",
				hostSSHPrivateKey: sshKeyPath,
				kubeconfigPath:    kubeconfigPath,
			},
			expectErr: "host is required",
		},
		{
			name: "missing ssh username",
			handler: siteAddMachineHandler{
				siteName:          "dc1",
				host:              "10.0.0.5",
				hostSSHPrivateKey: sshKeyPath,
				kubeconfigPath:    kubeconfigPath,
			},
			expectErr: "ssh username is required",
		},
		{
			name: "missing ssh private key when no bastion",
			handler: siteAddMachineHandler{
				siteName:        "dc1",
				host:            "10.0.0.5",
				hostSSHUsername: "admin",
				kubeconfigPath:  kubeconfigPath,
			},
			expectErr: "--ssh-private-key is required",
		},
		{
			name: "ssh private key file not readable",
			handler: siteAddMachineHandler{
				siteName:          "dc1",
				host:              "10.0.0.5",
				hostSSHUsername:   "admin",
				hostSSHPrivateKey: "/nonexistent/key",
				kubeconfigPath:    kubeconfigPath,
			},
			expectErr: "is not readable",
		},
		{
			name: "bastion ssh private key file not readable",
			handler: siteAddMachineHandler{
				siteName:             "dc1",
				host:                 "10.0.0.5",
				hostSSHUsername:      "admin",
				hostSSHPrivateKey:    sshKeyPath,
				bastionHost:          "5.6.7.8",
				bastionSSHPrivateKey: "/nonexistent/bastion-key",
				kubeconfigPath:       kubeconfigPath,
			},
			expectErr: "is not readable",
		},
		{
			name: "kubeconfig not readable",
			handler: siteAddMachineHandler{
				siteName:          "dc1",
				host:              "10.0.0.5",
				hostSSHUsername:   "admin",
				hostSSHPrivateKey: sshKeyPath,
				kubeconfigPath:    "/nonexistent/kubeconfig",
			},
			expectErr: "is not readable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.handler.validate()

			if tt.expectErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.expectErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// execute() integration tests
// ---------------------------------------------------------------------------

// newBootstrapTokenSecret creates a bootstrap token secret for the given site.
func newBootstrapTokenSecret(siteName string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bootstrap-token-abc123",
			Namespace: metav1.NamespaceSystem,
			Labels: map[string]string{
				"unbounded-kube.io/site": siteName,
			},
		},
		Type: corev1.SecretTypeBootstrapToken,
		Data: map[string][]byte{
			"token-id":                       []byte("abc123"),
			"token-secret":                   []byte("0123456789abcdef"),
			"usage-bootstrap-authentication": []byte("true"),
			"usage-bootstrap-signing":        []byte("true"),
		},
	}
}

func TestSiteAddMachineHandler_Execute_SSHOnly(t *testing.T) {
	t.Parallel()

	sshKeyPath := writeTempSSHKey(t)

	// Track what was applied via the controller-runtime fake.
	var appliedObjects []runtime.ApplyConfiguration

	kubeResourcesCli := fakeclient.NewClientBuilder().
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				appliedObjects = append(appliedObjects, obj)
				return nil
			},
		}).
		Build()

	kubeCli := fake.NewClientset(newBootstrapTokenSecret("dc1"))

	h := &siteAddMachineHandler{
		siteName:          "dc1",
		host:              "10.0.0.5",
		hostSSHUsername:   "admin",
		hostSSHPrivateKey: sshKeyPath,
		kubeCli:           kubeCli,
		kubeResourcesCli:  kubeResourcesCli,
		logger:            discardLogger(),
	}

	h.setDefaults()

	// Bypass validate() since it checks kubeconfig readability which
	// is irrelevant when clients are pre-injected.
	err := h.executeAfterValidation(context.Background())
	require.NoError(t, err)

	// Verify SSH secret was created.
	secret, err := kubeCli.CoreV1().Secrets(machinaNamespace).Get(context.Background(), "ssh-dc1", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, []byte("fake-ssh-private-key-content"), secret.Data["ssh-private-key"])

	// Verify a Machine was applied.
	require.Len(t, appliedObjects, 1, "expected exactly one object applied via controller-runtime")

	// Verify the machine name.
	require.Equal(t, "dc1-10.0.0.5", h.name)
}

func TestSiteAddMachineHandler_Execute_WithBastion(t *testing.T) {
	t.Parallel()

	hostKeyPath := writeTempSSHKey(t)

	// Create a separate bastion key file.
	bastionDir := t.TempDir()
	bastionKeyPath := filepath.Join(bastionDir, "bastion_key")
	require.NoError(t, os.WriteFile(bastionKeyPath, []byte("bastion-key-content"), 0o600))

	var appliedObjects []runtime.ApplyConfiguration

	kubeResourcesCli := fakeclient.NewClientBuilder().
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				appliedObjects = append(appliedObjects, obj)
				return nil
			},
		}).
		Build()

	kubeCli := fake.NewClientset(newBootstrapTokenSecret("dc1"))

	h := &siteAddMachineHandler{
		siteName:             "dc1",
		host:                 "10.0.0.5:2222",
		hostSSHUsername:      "admin",
		hostSSHPrivateKey:    hostKeyPath,
		bastionHost:          "5.6.7.8",
		bastionSSHSecretName: "bastion-ssh",
		bastionSSHPrivateKey: bastionKeyPath,
		kubeCli:              kubeCli,
		kubeResourcesCli:     kubeResourcesCli,
		logger:               discardLogger(),
	}

	h.setDefaults()

	err := h.executeAfterValidation(context.Background())
	require.NoError(t, err)

	// Verify host SSH secret.
	hostSecret, err := kubeCli.CoreV1().Secrets(machinaNamespace).Get(context.Background(), "ssh-dc1", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, []byte("fake-ssh-private-key-content"), hostSecret.Data["ssh-private-key"])

	// Verify bastion SSH secret (different name).
	bastionSecret, err := kubeCli.CoreV1().Secrets(machinaNamespace).Get(context.Background(), "bastion-ssh", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, []byte("bastion-key-content"), bastionSecret.Data["ssh-private-key"])

	// Verify a Machine was applied.
	require.Len(t, appliedObjects, 1, "expected exactly one object applied via controller-runtime")

	// Bastion username should have defaulted to host SSH username.
	require.Equal(t, "admin", h.bastionSSHUsername)

	// Machine name should include the port.
	require.Equal(t, "dc1-10.0.0.5-2222", h.name)
}

func TestSiteAddMachineHandler_Execute_BastionSharedSecret(t *testing.T) {
	t.Parallel()

	sshKeyPath := writeTempSSHKey(t)

	var appliedObjects []runtime.ApplyConfiguration

	kubeResourcesCli := fakeclient.NewClientBuilder().
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				appliedObjects = append(appliedObjects, obj)
				return nil
			},
		}).
		Build()

	kubeCli := fake.NewClientset(newBootstrapTokenSecret("dc1"))

	h := &siteAddMachineHandler{
		siteName:          "dc1",
		host:              "10.0.0.5",
		hostSSHUsername:   "admin",
		hostSSHPrivateKey: sshKeyPath,
		bastionHost:       "5.6.7.8",
		// bastionSSHSecretName left empty — should default to sshSecretName
		// bastionSSHPrivateKey left empty — should default to hostSSHPrivateKey
		kubeCli:          kubeCli,
		kubeResourcesCli: kubeResourcesCli,
		logger:           discardLogger(),
	}

	h.setDefaults()

	require.Equal(t, h.sshSecretName, h.bastionSSHSecretName, "bastion secret should default to host secret")

	err := h.executeAfterValidation(context.Background())
	require.NoError(t, err)

	// Only one secret should be created (shared between host and bastion).
	secrets, err := kubeCli.CoreV1().Secrets(machinaNamespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, secrets.Items, 1, "only one secret should be created for shared host/bastion key")

	// Machine should still be applied.
	require.Len(t, appliedObjects, 1)
}

func TestSiteAddMachineHandler_Execute_NoBootstrapToken(t *testing.T) {
	t.Parallel()

	sshKeyPath := writeTempSSHKey(t)

	kubeResourcesCli := fakeclient.NewClientBuilder().
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, _ runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				return nil
			},
		}).
		Build()

	// No bootstrap token pre-seeded.
	kubeCli := fake.NewClientset()

	h := &siteAddMachineHandler{
		siteName:          "dc1",
		host:              "10.0.0.5",
		hostSSHUsername:   "admin",
		hostSSHPrivateKey: sshKeyPath,
		kubeCli:           kubeCli,
		kubeResourcesCli:  kubeResourcesCli,
		logger:            discardLogger(),
	}

	h.setDefaults()

	err := h.executeAfterValidation(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "bootstrap token")
}

func TestSiteAddMachineHandler_Execute_BastionOnlyNoHostKey(t *testing.T) {
	t.Parallel()

	bastionKeyPath := writeTempSSHKey(t)

	var appliedObjects []runtime.ApplyConfiguration

	kubeResourcesCli := fakeclient.NewClientBuilder().
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				appliedObjects = append(appliedObjects, obj)
				return nil
			},
		}).
		Build()

	kubeCli := fake.NewClientset(newBootstrapTokenSecret("dc1"))

	h := &siteAddMachineHandler{
		siteName:             "dc1",
		host:                 "10.0.0.5",
		hostSSHUsername:      "admin",
		bastionHost:          "5.6.7.8",
		bastionSSHPrivateKey: bastionKeyPath,
		bastionSSHSecretName: "bastion-ssh",
		kubeCli:              kubeCli,
		kubeResourcesCli:     kubeResourcesCli,
		logger:               discardLogger(),
	}

	h.setDefaults()

	err := h.executeAfterValidation(context.Background())
	require.NoError(t, err)

	// No host SSH secret should be created since hostSSHPrivateKey is empty.
	_, err = kubeCli.CoreV1().Secrets(machinaNamespace).Get(context.Background(), "ssh-dc1", metav1.GetOptions{})
	require.Error(t, err, "host SSH secret should not exist when no host key is provided")

	// Bastion secret should be created.
	bastionSecret, err := kubeCli.CoreV1().Secrets(machinaNamespace).Get(context.Background(), "bastion-ssh", metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, bastionSecret)

	// Machine should be applied.
	require.Len(t, appliedObjects, 1)
}

// ---------------------------------------------------------------------------
// siteAddMachineCommand() cobra wiring test
// ---------------------------------------------------------------------------

func TestSiteAddMachineCommand(t *testing.T) {
	t.Parallel()

	cmd := siteAddMachineCommand()

	require.Equal(t, "add-machine", cmd.Use)
	require.NotNil(t, cmd.RunE)

	// Verify required flags exist.
	for _, flagName := range []string{"site", "host", "ssh-username"} {
		f := cmd.Flags().Lookup(flagName)
		require.NotNilf(t, f, "flag --%s should exist", flagName)
	}

	// Verify optional flags exist.
	for _, flagName := range []string{
		"name", "ssh-private-key", "ssh-secret-name",
		"bastion-host", "bastion-ssh-secret-name",
		"bastion-ssh-username", "bastion-ssh-private-key",
		"kubeconfig",
	} {
		f := cmd.Flags().Lookup(flagName)
		require.NotNilf(t, f, "flag --%s should exist", flagName)
	}
}
