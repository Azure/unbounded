// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	unboundedv1alpha3 "github.com/Azure/unbounded-kube/api/machina/v1alpha3"
	"github.com/Azure/unbounded-kube/internal/cloudprovider"
	"github.com/Azure/unbounded-kube/internal/provision"
)

// sshTestServer is an in-process SSH server used for integration tests.
type sshTestServer struct {
	listener net.Listener
	config   *ssh.ServerConfig
	host     string
	port     int

	mu               sync.Mutex
	executedCommands []sshExecutedCommand
	exitCode         int
}

type sshExecutedCommand struct {
	command string
	stdin   []byte
}

func newSSHTestServer(t *testing.T) *sshTestServer {
	t.Helper()

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	hostSigner, err := ssh.NewSignerFromKey(rsaKey)
	require.NoError(t, err)

	config := &ssh.ServerConfig{
		NoClientAuth: true,
	}
	config.AddHostKey(hostSigner)

	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr := listener.Addr().(*net.TCPAddr)

	srv := &sshTestServer{
		listener: listener,
		config:   config,
		host:     "127.0.0.1",
		port:     addr.Port,
	}

	go srv.serve(t)

	t.Cleanup(func() {
		_ = listener.Close()
	})

	return srv
}

func newSSHTestServerWithAuth(t *testing.T, authorizedKey ssh.PublicKey) *sshTestServer {
	t.Helper()

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	hostSigner, err := ssh.NewSignerFromKey(rsaKey)
	require.NoError(t, err)

	config := &ssh.ServerConfig{
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(key.Marshal(), authorizedKey.Marshal()) {
				return &ssh.Permissions{}, nil
			}

			return nil, fmt.Errorf("unknown public key for %q", conn.User())
		},
	}
	config.AddHostKey(hostSigner)

	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr := listener.Addr().(*net.TCPAddr)

	srv := &sshTestServer{
		listener: listener,
		config:   config,
		host:     "127.0.0.1",
		port:     addr.Port,
	}

	go srv.serve(t)

	t.Cleanup(func() {
		_ = listener.Close()
	})

	return srv
}

func (s *sshTestServer) serve(t *testing.T) {
	t.Helper()

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}

		go s.handleConnection(t, conn)
	}
}

func (s *sshTestServer) handleConnection(t *testing.T, conn net.Conn) {
	t.Helper()

	serverConn, chans, reqs, err := ssh.NewServerConn(conn, s.config)
	if err != nil {
		_ = conn.Close()
		return
	}
	defer serverConn.Close()

	go ssh.DiscardRequests(reqs)

	for newChan := range chans {
		switch newChan.ChannelType() {
		case "session":
			go s.handleSession(t, newChan)
		case "direct-tcpip":
			go s.handleDirectTCPIP(t, newChan)
		default:
			_ = newChan.Reject(ssh.UnknownChannelType, "unknown channel type")
		}
	}
}

func (s *sshTestServer) handleSession(t *testing.T, newChan ssh.NewChannel) {
	t.Helper()

	channel, requests, err := newChan.Accept()
	if err != nil {
		return
	}
	defer channel.Close()

	for req := range requests {
		switch req.Type {
		case "env":
			if req.WantReply {
				_ = req.Reply(true, nil)
			}

		case "exec":
			if len(req.Payload) < 4 {
				if req.WantReply {
					_ = req.Reply(false, nil)
				}

				continue
			}

			cmdLen := int(req.Payload[0])<<24 | int(req.Payload[1])<<16 | int(req.Payload[2])<<8 | int(req.Payload[3])
			if len(req.Payload) < 4+cmdLen {
				if req.WantReply {
					_ = req.Reply(false, nil)
				}

				continue
			}

			command := string(req.Payload[4 : 4+cmdLen])

			if req.WantReply {
				_ = req.Reply(true, nil)
			}

			var stdinBuf bytes.Buffer

			done := make(chan struct{})

			go func() {
				_, _ = io.Copy(&stdinBuf, channel)

				close(done)
			}()

			select {
			case <-done:
			case <-time.After(2 * time.Second):
			}

			s.mu.Lock()
			s.executedCommands = append(s.executedCommands, sshExecutedCommand{
				command: command,
				stdin:   stdinBuf.Bytes(),
			})
			exitCode := s.exitCode
			s.mu.Unlock()

			exitPayload := []byte{0, 0, 0, byte(exitCode)}
			_, _ = channel.SendRequest("exit-status", false, exitPayload)

			return
		default:
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}
}

func (s *sshTestServer) handleDirectTCPIP(t *testing.T, newChan ssh.NewChannel) {
	t.Helper()

	type directTCPIPData struct {
		DestAddr   string
		DestPort   uint32
		OriginAddr string
		OriginPort uint32
	}

	var data directTCPIPData
	if err := ssh.Unmarshal(newChan.ExtraData(), &data); err != nil {
		_ = newChan.Reject(ssh.ConnectionFailed, "failed to parse direct-tcpip data")
		return
	}

	targetAddr := net.JoinHostPort(data.DestAddr, fmt.Sprintf("%d", data.DestPort))

	targetConn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(context.Background(), "tcp", targetAddr)
	if err != nil {
		_ = newChan.Reject(ssh.ConnectionFailed, fmt.Sprintf("failed to connect to %s: %v", targetAddr, err))
		return
	}

	channel, requests, err := newChan.Accept()
	if err != nil {
		_ = targetConn.Close()
		return
	}

	go ssh.DiscardRequests(requests)

	go func() {
		defer channel.Close()
		defer targetConn.Close()

		_, _ = io.Copy(channel, targetConn)
	}()

	go func() {
		defer channel.Close()
		defer targetConn.Close()

		_, _ = io.Copy(targetConn, channel)
	}()
}

func (s *sshTestServer) getExecutedCommands() []sshExecutedCommand {
	s.mu.Lock()
	defer s.mu.Unlock()

	result := make([]sshExecutedCommand, len(s.executedCommands))
	copy(result, s.executedCommands)

	return result
}

func (s *sshTestServer) setExitCode(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.exitCode = code
}

func (s *sshTestServer) address() string {
	return fmt.Sprintf("%s:%d", s.host, s.port)
}

func generateTestRSAKey(t *testing.T) (*rsa.PrivateKey, ssh.Signer) {
	t.Helper()

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	signer, err := ssh.NewSignerFromKey(rsaKey)
	require.NoError(t, err)

	return rsaKey, signer
}

func marshalPrivateKeyPEM(t *testing.T, key *rsa.PrivateKey) []byte {
	t.Helper()

	derBytes := x509.MarshalPKCS1PrivateKey(key)

	pemBlock := &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: derBytes,
	}

	return pem.EncodeToMemory(pemBlock)
}

func sshTestClientConfig(signer ssh.Signer) *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User:            "testuser",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec
		Timeout:         5 * time.Second,
	}
}

func noAuthClientConfig() *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User:            "testuser",
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec
		Timeout:         5 * time.Second,
	}
}

// ---------------------------------------------------------------------------
// SSH integration tests
// ---------------------------------------------------------------------------

func TestSSH_DirectConnection(t *testing.T) {
	t.Parallel()

	srv := newSSHTestServer(t)

	client, err := ssh.Dial("tcp", srv.address(), noAuthClientConfig())
	require.NoError(t, err)

	defer client.Close()

	session, err := client.NewSession()
	require.NoError(t, err)

	defer session.Close()

	err = session.Run("echo hello")
	require.NoError(t, err)

	commands := srv.getExecutedCommands()
	require.Len(t, commands, 1)
	require.Equal(t, "echo hello", commands[0].command)
}

func TestSSH_DirectConnectionWithAuth(t *testing.T) {
	t.Parallel()

	_, signer := generateTestRSAKey(t)
	srv := newSSHTestServerWithAuth(t, signer.PublicKey())

	client, err := ssh.Dial("tcp", srv.address(), sshTestClientConfig(signer))
	require.NoError(t, err)

	defer client.Close()

	session, err := client.NewSession()
	require.NoError(t, err)

	defer session.Close()

	err = session.Run("whoami")
	require.NoError(t, err)

	commands := srv.getExecutedCommands()
	require.Len(t, commands, 1)
	require.Equal(t, "whoami", commands[0].command)
}

func TestSSH_AuthRejectedWithWrongKey(t *testing.T) {
	t.Parallel()

	_, authorizedSigner := generateTestRSAKey(t)
	_, wrongSigner := generateTestRSAKey(t)

	srv := newSSHTestServerWithAuth(t, authorizedSigner.PublicKey())

	_, err := ssh.Dial("tcp", srv.address(), sshTestClientConfig(wrongSigner))
	require.Error(t, err, "should fail with wrong key")
}

func TestSSH_BastionConnection(t *testing.T) {
	t.Parallel()

	targetSrv := newSSHTestServer(t)
	bastionSrv := newSSHTestServer(t)

	bastionClient, err := ssh.Dial("tcp", bastionSrv.address(), noAuthClientConfig())
	require.NoError(t, err)

	defer bastionClient.Close()

	conn, err := bastionClient.Dial("tcp", targetSrv.address())
	require.NoError(t, err)

	ncc, chans, reqs, err := ssh.NewClientConn(conn, targetSrv.address(), noAuthClientConfig())
	require.NoError(t, err)

	targetClient := ssh.NewClient(ncc, chans, reqs)
	defer targetClient.Close()

	session, err := targetClient.NewSession()
	require.NoError(t, err)

	defer session.Close()

	err = session.Run("uname -a")
	require.NoError(t, err)

	commands := targetSrv.getExecutedCommands()
	require.Len(t, commands, 1)
	require.Equal(t, "uname -a", commands[0].command)
}

func TestSSH_StdinPipeForScriptCopy(t *testing.T) {
	t.Parallel()

	srv := newSSHTestServer(t)

	client, err := ssh.Dial("tcp", srv.address(), noAuthClientConfig())
	require.NoError(t, err)

	defer client.Close()

	scriptContent := "#!/bin/bash\necho installing agent\napt-get install -y agent"

	session, err := client.NewSession()
	require.NoError(t, err)

	session.Stdin = bytes.NewBufferString(scriptContent)

	err = session.Run(fmt.Sprintf("cat > %s && chmod +x %s", remoteScriptPath, remoteScriptPath))
	require.NoError(t, err)
	session.Close()

	commands := srv.getExecutedCommands()
	require.Len(t, commands, 1)
	require.Contains(t, commands[0].command, remoteScriptPath)
	require.Equal(t, scriptContent, string(commands[0].stdin))
}

func TestSSH_CommandFailureExitCode(t *testing.T) {
	t.Parallel()

	srv := newSSHTestServer(t)
	srv.setExitCode(1)

	client, err := ssh.Dial("tcp", srv.address(), noAuthClientConfig())
	require.NoError(t, err)

	defer client.Close()

	session, err := client.NewSession()
	require.NoError(t, err)

	defer session.Close()

	err = session.Run("false")
	require.Error(t, err, "should fail with exit code 1")

	var exitErr *ssh.ExitError
	require.ErrorAs(t, err, &exitErr)
	require.Equal(t, 1, exitErr.ExitStatus())
}

func TestSSH_MultiSessionScriptFlow(t *testing.T) {
	t.Parallel()

	srv := newSSHTestServer(t)

	client, err := ssh.Dial("tcp", srv.address(), noAuthClientConfig())
	require.NoError(t, err)

	defer client.Close()

	// Step 1: Copy script.
	copySession, err := client.NewSession()
	require.NoError(t, err)

	copySession.Stdin = bytes.NewBufferString("#!/bin/bash\nexit 1")

	err = copySession.Run(fmt.Sprintf("cat > %s && chmod +x %s", remoteScriptPath, remoteScriptPath))
	require.NoError(t, err)
	copySession.Close()

	// Step 2: Execute script (fail).
	srv.setExitCode(1)

	execSession, err := client.NewSession()
	require.NoError(t, err)

	err = execSession.Run(fmt.Sprintf("sudo -E bash %s", remoteScriptPath))
	require.Error(t, err)
	execSession.Close()

	// Step 3: Cleanup.
	srv.setExitCode(0)

	cleanupSession, err := client.NewSession()
	require.NoError(t, err)

	err = cleanupSession.Run(fmt.Sprintf("rm -f %s", remoteScriptPath))
	require.NoError(t, err)
	cleanupSession.Close()

	// Verify all three commands.
	commands := srv.getExecutedCommands()
	require.Len(t, commands, 3)
	require.Contains(t, commands[0].command, "cat >")
	require.Contains(t, commands[1].command, "sudo -E bash")
	require.Contains(t, commands[2].command, "rm -f")
}

// ---------------------------------------------------------------------------
// Full provisionMachine integration test
// ---------------------------------------------------------------------------

func TestProvisionMachine_EndToEnd(t *testing.T) {
	t.Parallel()

	srv := newSSHTestServer(t)

	_, signer := generateTestRSAKey(t)

	s := runtime.NewScheme()
	require.NoError(t, unboundedv1alpha3.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))

	machine := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-machine",
		},
		Spec: unboundedv1alpha3.MachineSpec{
			SSH: &unboundedv1alpha3.SSHSpec{
				Host:     fmt.Sprintf("%s:%d", srv.host, srv.port),
				Username: "testuser",
				PrivateKeyRef: unboundedv1alpha3.SecretKeySelector{
					Name: "ssh-key-secret",
					Key:  "ssh-privatekey",
				},
			},
			Kubernetes: &unboundedv1alpha3.KubernetesSpec{
				Version: "1.34.0",
				BootstrapTokenRef: unboundedv1alpha3.LocalObjectReference{
					Name: "bootstrap-token-abc123",
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine).
		Build()

	reconciler := &MachineReconciler{
		Client: fakeClient,
		Scheme: s,
		ClusterInfo: &ClusterInfo{
			APIServer:    "api.example.com:443",
			CACertBase64: "dGVzdC1jYQ==",
			ClusterDNS:   "10.0.0.10",
			KubeVersion:  "v1.34.2",
		},
	}

	sshConfig := sshTestClientConfig(signer)

	err := reconciler.provisionMachine(context.Background(), machine, sshConfig, "abc123.secret")
	require.NoError(t, err)

	commands := srv.getExecutedCommands()
	require.GreaterOrEqual(t, len(commands), 4, "expected at least copy + config upload + exec + cleanup commands")

	// Find commands by their characteristics rather than hardcoded indices,
	// so the test doesn't break if intermediate steps are added.
	var copyCmd, configCmd, execCmd, cleanupCmd *sshExecutedCommand

	for i := range commands {
		switch {
		case strings.Contains(commands[i].command, "cat >") && strings.Contains(commands[i].command, remoteScriptPath):
			copyCmd = &commands[i]
		case strings.Contains(commands[i].command, "cat >") && strings.Contains(commands[i].command, remoteConfigPath):
			configCmd = &commands[i]
		case strings.Contains(commands[i].command, "sudo -E bash") && strings.Contains(commands[i].command, remoteScriptPath):
			execCmd = &commands[i]
		case strings.Contains(commands[i].command, "rm -f"):
			cleanupCmd = &commands[i]
		}
	}

	// Command: script copy — verify a script was sent, not its exact contents.
	require.NotNil(t, copyCmd, "expected a script copy command")
	require.Contains(t, copyCmd.command, "chmod +x")
	require.NotEmpty(t, copyCmd.stdin, "script content should have been piped via stdin")
	require.Contains(t, string(copyCmd.stdin), "#!/bin/bash", "script should start with a shebang")

	// Command: config upload — verify JSON config was sent.
	require.NotNil(t, configCmd, "expected a config upload command")

	var agentConfig provision.AgentConfig
	require.NoError(t, json.Unmarshal(configCmd.stdin, &agentConfig), "config stdin should be valid JSON")
	require.Equal(t, "test-machine", agentConfig.MachineName)
	require.Equal(t, "dGVzdC1jYQ==", agentConfig.Cluster.CaCertBase64)
	require.Equal(t, "10.0.0.10", agentConfig.Cluster.ClusterDNS)
	require.Equal(t, "v1.34.0", agentConfig.Cluster.Version) // Machine spec overrides cluster version.
	require.Equal(t, "api.example.com:443", agentConfig.Kubelet.ApiServer)
	require.Equal(t, "abc123.secret", agentConfig.Kubelet.BootstrapToken)

	// Command: script execution with UNBOUNDED_AGENT_CONFIG_FILE.
	require.NotNil(t, execCmd, "expected a script execution command")
	require.Contains(t, execCmd.command, "UNBOUNDED_AGENT_CONFIG_FILE")
	require.Contains(t, execCmd.command, remoteConfigPath)

	// Command: cleanup — should remove both script and config.
	require.NotNil(t, cleanupCmd, "expected a cleanup command")
	require.Contains(t, cleanupCmd.command, remoteScriptPath)
	require.Contains(t, cleanupCmd.command, remoteConfigPath)
}

func TestProvisionMachine_CleanupAlwaysRuns(t *testing.T) {
	t.Parallel()

	srv := newSSHTestServer(t)

	_, signer := generateTestRSAKey(t)

	s := runtime.NewScheme()
	require.NoError(t, unboundedv1alpha3.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))

	machine := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "test-machine"},
		Spec: unboundedv1alpha3.MachineSpec{
			SSH: &unboundedv1alpha3.SSHSpec{
				Host:          fmt.Sprintf("%s:%d", srv.host, srv.port),
				Username:      "testuser",
				PrivateKeyRef: unboundedv1alpha3.SecretKeySelector{Name: "ssh-key-secret"},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(machine).Build()

	reconciler := &MachineReconciler{
		Client:      fakeClient,
		Scheme:      s,
		ClusterInfo: &ClusterInfo{},
	}

	sshConfig := sshTestClientConfig(signer)

	err := reconciler.provisionMachine(context.Background(), machine, sshConfig, "")
	require.NoError(t, err)

	commands := srv.getExecutedCommands()
	require.GreaterOrEqual(t, len(commands), 4, "copy + config upload + exec + cleanup should all run")

	hasCleanup := false

	for _, cmd := range commands {
		if strings.Contains(cmd.command, "rm -f") && strings.Contains(cmd.command, remoteScriptPath) && strings.Contains(cmd.command, remoteConfigPath) {
			hasCleanup = true
			break
		}
	}

	require.True(t, hasCleanup, "expected a cleanup command with rm -f for both script and config")
}

func TestProvisionMachine_ConfigFile(t *testing.T) {
	t.Parallel()

	srv := newSSHTestServer(t)

	_, signer := generateTestRSAKey(t)

	s := runtime.NewScheme()
	require.NoError(t, unboundedv1alpha3.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))

	machine := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "my-special-machine"},
		Spec: unboundedv1alpha3.MachineSpec{
			SSH: &unboundedv1alpha3.SSHSpec{
				Host:          fmt.Sprintf("%s:%d", srv.host, srv.port),
				Username:      "testuser",
				PrivateKeyRef: unboundedv1alpha3.SecretKeySelector{Name: "ssh-key-secret"},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(machine).Build()

	reconciler := &MachineReconciler{
		Client: fakeClient,
		Scheme: s,
		ClusterInfo: &ClusterInfo{
			APIServer:    "k8s.example.com:6443",
			CACertBase64: "Y2VydC1kYXRh",
			ClusterDNS:   "10.96.0.10",
			KubeVersion:  "v1.33.1",
		},
	}

	sshConfig := sshTestClientConfig(signer)

	err := reconciler.provisionMachine(context.Background(), machine, sshConfig, "tok123.secret456")
	require.NoError(t, err)

	commands := srv.getExecutedCommands()

	// Find the config upload command.
	var configCmd *sshExecutedCommand

	for i := range commands {
		if strings.Contains(commands[i].command, "cat >") && strings.Contains(commands[i].command, remoteConfigPath) {
			configCmd = &commands[i]
			break
		}
	}

	require.NotNil(t, configCmd, "expected a config upload command")

	var agentConfig provision.AgentConfig
	require.NoError(t, json.Unmarshal(configCmd.stdin, &agentConfig))

	require.Equal(t, "my-special-machine", agentConfig.MachineName)
	require.Equal(t, "Y2VydC1kYXRh", agentConfig.Cluster.CaCertBase64)
	require.Equal(t, "10.96.0.10", agentConfig.Cluster.ClusterDNS)
	require.Equal(t, "v1.33.1", agentConfig.Cluster.Version)
	require.Equal(t, "k8s.example.com:6443", agentConfig.Kubelet.ApiServer)
	require.Equal(t, "tok123.secret456", agentConfig.Kubelet.BootstrapToken)
}

func TestProvisionMachine_NilClusterInfo(t *testing.T) {
	t.Parallel()

	srv := newSSHTestServer(t)

	_, signer := generateTestRSAKey(t)

	s := runtime.NewScheme()
	require.NoError(t, unboundedv1alpha3.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))

	machine := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "test-machine"},
		Spec: unboundedv1alpha3.MachineSpec{
			SSH: &unboundedv1alpha3.SSHSpec{
				Host:          fmt.Sprintf("%s:%d", srv.host, srv.port),
				Username:      "testuser",
				PrivateKeyRef: unboundedv1alpha3.SecretKeySelector{Name: "ssh-key-secret"},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(machine).Build()

	reconciler := &MachineReconciler{
		Client:      fakeClient,
		Scheme:      s,
		ClusterInfo: nil,
	}

	sshConfig := sshTestClientConfig(signer)

	err := reconciler.provisionMachine(context.Background(), machine, sshConfig, "")
	require.NoError(t, err)

	commands := srv.getExecutedCommands()

	// Find the config upload command.
	var configCmd *sshExecutedCommand

	for i := range commands {
		if strings.Contains(commands[i].command, "cat >") && strings.Contains(commands[i].command, remoteConfigPath) {
			configCmd = &commands[i]
			break
		}
	}

	require.NotNil(t, configCmd, "expected a config upload command")

	var agentConfig provision.AgentConfig
	require.NoError(t, json.Unmarshal(configCmd.stdin, &agentConfig))

	require.Equal(t, "test-machine", agentConfig.MachineName)
	require.Equal(t, "", agentConfig.Cluster.ClusterDNS)
	require.Equal(t, "", agentConfig.Cluster.Version)
	require.Equal(t, "", agentConfig.Kubelet.ApiServer)
	require.Equal(t, "", agentConfig.Kubelet.BootstrapToken)
}

func TestProvisionMachine_KubeVersionPrefixing(t *testing.T) {
	t.Parallel()

	srv := newSSHTestServer(t)

	_, signer := generateTestRSAKey(t)

	s := runtime.NewScheme()
	require.NoError(t, unboundedv1alpha3.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))

	machine := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "test-machine"},
		Spec: unboundedv1alpha3.MachineSpec{
			SSH: &unboundedv1alpha3.SSHSpec{
				Host:          fmt.Sprintf("%s:%d", srv.host, srv.port),
				Username:      "testuser",
				PrivateKeyRef: unboundedv1alpha3.SecretKeySelector{Name: "ssh-key-secret"},
			},
			Kubernetes: &unboundedv1alpha3.KubernetesSpec{
				Version:           "1.34.0",
				BootstrapTokenRef: unboundedv1alpha3.LocalObjectReference{Name: "bt"},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(machine).Build()

	reconciler := &MachineReconciler{
		Client:      fakeClient,
		Scheme:      s,
		ClusterInfo: &ClusterInfo{KubeVersion: "v1.33.0"},
	}

	sshConfig := sshTestClientConfig(signer)
	err := reconciler.provisionMachine(context.Background(), machine, sshConfig, "")
	require.NoError(t, err)

	commands := srv.getExecutedCommands()

	// Find the config upload command.
	var configCmd *sshExecutedCommand

	for i := range commands {
		if strings.Contains(commands[i].command, "cat >") && strings.Contains(commands[i].command, remoteConfigPath) {
			configCmd = &commands[i]
			break
		}
	}

	require.NotNil(t, configCmd, "expected a config upload command")

	var agentConfig provision.AgentConfig
	require.NoError(t, json.Unmarshal(configCmd.stdin, &agentConfig))

	require.Equal(t, "v1.34.0", agentConfig.Cluster.Version)
}

func TestProvisionMachine_LabelMerge(t *testing.T) {
	t.Parallel()

	srv := newSSHTestServer(t)

	_, signer := generateTestRSAKey(t)

	s := runtime.NewScheme()
	require.NoError(t, unboundedv1alpha3.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))

	machine := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "label-test-machine"},
		Spec: unboundedv1alpha3.MachineSpec{
			SSH: &unboundedv1alpha3.SSHSpec{
				Host:          fmt.Sprintf("%s:%d", srv.host, srv.port),
				Username:      "testuser",
				PrivateKeyRef: unboundedv1alpha3.SecretKeySelector{Name: "ssh-key-secret"},
			},
			Kubernetes: &unboundedv1alpha3.KubernetesSpec{
				BootstrapTokenRef: unboundedv1alpha3.LocalObjectReference{Name: "bt"},
				NodeLabels: map[string]string{
					"env":  "production",
					"team": "platform",
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(machine).Build()

	reconciler := &MachineReconciler{
		Client:      fakeClient,
		Scheme:      s,
		ClusterInfo: &ClusterInfo{},
	}

	sshConfig := sshTestClientConfig(signer)

	err := reconciler.provisionMachine(context.Background(), machine, sshConfig, "")
	require.NoError(t, err)

	commands := srv.getExecutedCommands()

	var configCmd *sshExecutedCommand

	for i := range commands {
		if strings.Contains(commands[i].command, "cat >") && strings.Contains(commands[i].command, remoteConfigPath) {
			configCmd = &commands[i]
			break
		}
	}

	require.NotNil(t, configCmd, "expected a config upload command")

	var agentConfig provision.AgentConfig
	require.NoError(t, json.Unmarshal(configCmd.stdin, &agentConfig))

	// User-defined labels should be present.
	require.Equal(t, "production", agentConfig.Kubelet.Labels["env"])
	require.Equal(t, "platform", agentConfig.Kubelet.Labels["team"])
}

func TestProvisionMachine_Taints(t *testing.T) {
	t.Parallel()

	srv := newSSHTestServer(t)

	_, signer := generateTestRSAKey(t)

	s := runtime.NewScheme()
	require.NoError(t, unboundedv1alpha3.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))

	machine := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "taint-test-machine"},
		Spec: unboundedv1alpha3.MachineSpec{
			SSH: &unboundedv1alpha3.SSHSpec{
				Host:          fmt.Sprintf("%s:%d", srv.host, srv.port),
				Username:      "testuser",
				PrivateKeyRef: unboundedv1alpha3.SecretKeySelector{Name: "ssh-key-secret"},
			},
			Kubernetes: &unboundedv1alpha3.KubernetesSpec{
				BootstrapTokenRef: unboundedv1alpha3.LocalObjectReference{Name: "bt"},
				RegisterWithTaints: []string{
					"dedicated=gpu:NoSchedule",
					"special=true:NoExecute",
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(machine).Build()

	reconciler := &MachineReconciler{
		Client:      fakeClient,
		Scheme:      s,
		ClusterInfo: &ClusterInfo{},
	}

	sshConfig := sshTestClientConfig(signer)

	err := reconciler.provisionMachine(context.Background(), machine, sshConfig, "")
	require.NoError(t, err)

	commands := srv.getExecutedCommands()

	var configCmd *sshExecutedCommand

	for i := range commands {
		if strings.Contains(commands[i].command, "cat >") && strings.Contains(commands[i].command, remoteConfigPath) {
			configCmd = &commands[i]
			break
		}
	}

	require.NotNil(t, configCmd, "expected a config upload command")

	var agentConfig provision.AgentConfig
	require.NoError(t, json.Unmarshal(configCmd.stdin, &agentConfig))

	require.Equal(t, []string{"dedicated=gpu:NoSchedule", "special=true:NoExecute"}, agentConfig.Kubelet.RegisterWithTaints)
}

func TestProvisionMachine_OCIImage(t *testing.T) {
	t.Parallel()

	srv := newSSHTestServer(t)

	_, signer := generateTestRSAKey(t)

	s := runtime.NewScheme()
	require.NoError(t, unboundedv1alpha3.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))

	machine := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "oci-image-machine"},
		Spec: unboundedv1alpha3.MachineSpec{
			SSH: &unboundedv1alpha3.SSHSpec{
				Host:          fmt.Sprintf("%s:%d", srv.host, srv.port),
				Username:      "testuser",
				PrivateKeyRef: unboundedv1alpha3.SecretKeySelector{Name: "ssh-key-secret"},
			},
			Kubernetes: &unboundedv1alpha3.KubernetesSpec{
				BootstrapTokenRef: unboundedv1alpha3.LocalObjectReference{Name: "bt"},
			},
			Agent: &unboundedv1alpha3.AgentSpec{
				Image: "ghcr.io/azure/rootfs:v1.0.0",
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(machine).Build()

	reconciler := &MachineReconciler{
		Client:      fakeClient,
		Scheme:      s,
		ClusterInfo: &ClusterInfo{},
	}

	sshConfig := sshTestClientConfig(signer)

	err := reconciler.provisionMachine(context.Background(), machine, sshConfig, "")
	require.NoError(t, err)

	commands := srv.getExecutedCommands()

	var configCmd *sshExecutedCommand

	for i := range commands {
		if strings.Contains(commands[i].command, "cat >") && strings.Contains(commands[i].command, remoteConfigPath) {
			configCmd = &commands[i]
			break
		}
	}

	require.NotNil(t, configCmd, "expected a config upload command")

	var agentConfig provision.AgentConfig
	require.NoError(t, json.Unmarshal(configCmd.stdin, &agentConfig))

	require.Equal(t, "ghcr.io/azure/rootfs:v1.0.0", agentConfig.OCIImage)
}

func TestProvisionMachine_ProviderLabelsOverride(t *testing.T) {
	t.Parallel()

	srv := newSSHTestServer(t)

	_, signer := generateTestRSAKey(t)

	s := runtime.NewScheme()
	require.NoError(t, unboundedv1alpha3.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))

	machine := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "provider-label-machine"},
		Spec: unboundedv1alpha3.MachineSpec{
			SSH: &unboundedv1alpha3.SSHSpec{
				Host:          fmt.Sprintf("%s:%d", srv.host, srv.port),
				Username:      "testuser",
				PrivateKeyRef: unboundedv1alpha3.SecretKeySelector{Name: "ssh-key-secret"},
			},
			Kubernetes: &unboundedv1alpha3.KubernetesSpec{
				BootstrapTokenRef: unboundedv1alpha3.LocalObjectReference{Name: "bt"},
				NodeLabels: map[string]string{
					"env": "production",
					// User tries to override provider label — provider should win.
					"kubernetes.azure.com/managed": "true",
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(machine).Build()

	reconciler := &MachineReconciler{
		Client: fakeClient,
		Scheme: s,
		ClusterInfo: &ClusterInfo{
			Provider: &cloudprovider.AKSProvider{ClusterName: "mc_rg_test_eastus"},
		},
	}

	sshConfig := sshTestClientConfig(signer)

	err := reconciler.provisionMachine(context.Background(), machine, sshConfig, "")
	require.NoError(t, err)

	commands := srv.getExecutedCommands()

	var configCmd *sshExecutedCommand

	for i := range commands {
		if strings.Contains(commands[i].command, "cat >") && strings.Contains(commands[i].command, remoteConfigPath) {
			configCmd = &commands[i]
			break
		}
	}

	require.NotNil(t, configCmd, "expected a config upload command")

	var agentConfig provision.AgentConfig
	require.NoError(t, json.Unmarshal(configCmd.stdin, &agentConfig))

	// User label is preserved.
	require.Equal(t, "production", agentConfig.Kubelet.Labels["env"])

	// Provider label overrides user-specified value.
	require.Equal(t, "false", agentConfig.Kubelet.Labels["kubernetes.azure.com/managed"])

	// Provider label for cluster name is injected.
	require.Equal(t, "mc_rg_test_eastus", agentConfig.Kubelet.Labels["kubernetes.azure.com/cluster"])
}

// ---------------------------------------------------------------------------
// dialViaBastion integration tests
// ---------------------------------------------------------------------------

func TestDialViaBastion_Integration(t *testing.T) {
	t.Parallel()

	targetSrv := newSSHTestServer(t)
	bastionSrv := newSSHTestServer(t)

	rsaKey, _ := generateTestRSAKey(t)
	pemBytes := marshalPrivateKeyPEM(t, rsaKey)

	s := runtime.NewScheme()
	require.NoError(t, unboundedv1alpha3.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))

	bastionKeySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bastion-key-secret", Namespace: "unbounded-kube"},
		Data:       map[string][]byte{"ssh-privatekey": pemBytes},
	}

	machine := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "test-machine"},
		Spec: unboundedv1alpha3.MachineSpec{
			SSH: &unboundedv1alpha3.SSHSpec{
				Host:     fmt.Sprintf("%s:%d", targetSrv.host, targetSrv.port),
				Username: "testuser",
				PrivateKeyRef: unboundedv1alpha3.SecretKeySelector{
					Name: "target-key-secret",
				},
				Bastion: &unboundedv1alpha3.BastionSSHSpec{
					Host:     fmt.Sprintf("%s:%d", bastionSrv.host, bastionSrv.port),
					Username: "bastionuser",
					PrivateKeyRef: &unboundedv1alpha3.SecretKeySelector{
						Name: "bastion-key-secret",
						Key:  "ssh-privatekey",
					},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(bastionKeySecret).
		Build()

	reconciler := &MachineReconciler{
		Client: fakeClient,
		Scheme: s,
	}

	targetConfig := noAuthClientConfig()

	sshClient, err := reconciler.dialViaBastion(
		context.Background(),
		machine,
		targetSrv.address(),
		targetConfig,
	)
	require.NoError(t, err)

	defer sshClient.Close()

	session, err := sshClient.NewSession()
	require.NoError(t, err)

	defer session.Close()

	err = session.Run("echo via-bastion")
	require.NoError(t, err)

	commands := targetSrv.getExecutedCommands()
	require.Len(t, commands, 1)
	require.Equal(t, "echo via-bastion", commands[0].command)
}

func TestDialViaBastion_FallsBackToMachineKey(t *testing.T) {
	t.Parallel()

	targetSrv := newSSHTestServer(t)
	bastionSrv := newSSHTestServer(t)

	rsaKey, _ := generateTestRSAKey(t)
	pemBytes := marshalPrivateKeyPEM(t, rsaKey)

	s := runtime.NewScheme()
	require.NoError(t, unboundedv1alpha3.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))

	machineKeySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-key-secret", Namespace: "unbounded-kube"},
		Data:       map[string][]byte{"ssh-privatekey": pemBytes},
	}

	// Bastion has no PrivateKeyRef — should fall back to machine's SSH key.
	machine := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "test-machine"},
		Spec: unboundedv1alpha3.MachineSpec{
			SSH: &unboundedv1alpha3.SSHSpec{
				Host:     fmt.Sprintf("%s:%d", targetSrv.host, targetSrv.port),
				Username: "testuser",
				PrivateKeyRef: unboundedv1alpha3.SecretKeySelector{
					Name: "machine-key-secret",
					Key:  "ssh-privatekey",
				},
				Bastion: &unboundedv1alpha3.BastionSSHSpec{
					Host:     fmt.Sprintf("%s:%d", bastionSrv.host, bastionSrv.port),
					Username: "bastionuser",
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machineKeySecret).
		Build()

	reconciler := &MachineReconciler{
		Client: fakeClient,
		Scheme: s,
	}

	targetConfig := noAuthClientConfig()

	sshClient, err := reconciler.dialViaBastion(
		context.Background(),
		machine,
		targetSrv.address(),
		targetConfig,
	)
	require.NoError(t, err)

	defer sshClient.Close()

	session, err := sshClient.NewSession()
	require.NoError(t, err)

	defer session.Close()

	err = session.Run("echo fallback-key")
	require.NoError(t, err)
}

func TestDialViaBastion_DefaultPort(t *testing.T) {
	t.Parallel()

	rsaKey, _ := generateTestRSAKey(t)
	pemBytes := marshalPrivateKeyPEM(t, rsaKey)

	s := runtime.NewScheme()
	require.NoError(t, unboundedv1alpha3.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))

	bastionKeySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bastion-key-secret", Namespace: "unbounded-kube"},
		Data:       map[string][]byte{"ssh-privatekey": pemBytes},
	}

	// Bastion host has no port — hostPort() should default to :22.
	machine := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "test-machine"},
		Spec: unboundedv1alpha3.MachineSpec{
			SSH: &unboundedv1alpha3.SSHSpec{
				Host:          "127.0.0.1:9999",
				Username:      "testuser",
				PrivateKeyRef: unboundedv1alpha3.SecretKeySelector{Name: "key"},
				Bastion: &unboundedv1alpha3.BastionSSHSpec{
					Host: "127.0.0.1",
					PrivateKeyRef: &unboundedv1alpha3.SecretKeySelector{
						Name: "bastion-key-secret",
						Key:  "ssh-privatekey",
					},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(bastionKeySecret).
		Build()

	reconciler := &MachineReconciler{
		Client: fakeClient,
		Scheme: s,
	}

	targetConfig := &ssh.ClientConfig{
		User:            "testuser",
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec
		Timeout:         1 * time.Second,
	}

	_, err := reconciler.dialViaBastion(
		context.Background(),
		machine,
		"127.0.0.1:9999",
		targetConfig,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "dial bastion")
}

// ---------------------------------------------------------------------------
// Bastion reachability checker integration tests
// ---------------------------------------------------------------------------

func TestDefaultReachabilityChecker_BastionReachable_TargetReachable(t *testing.T) {
	t.Parallel()

	targetSrv := newSSHTestServer(t)
	bastionSrv := newSSHTestServer(t)

	rsaKey, _ := generateTestRSAKey(t)
	pemBytes := marshalPrivateKeyPEM(t, rsaKey)

	s := runtime.NewScheme()
	require.NoError(t, unboundedv1alpha3.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))

	bastionKeySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bastion-key-secret", Namespace: SecretNamespaceUnboundedKube},
		Data:       map[string][]byte{"ssh-privatekey": pemBytes},
	}

	machine := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "bastion-reach-test"},
		Spec: unboundedv1alpha3.MachineSpec{
			SSH: &unboundedv1alpha3.SSHSpec{
				Host:     targetSrv.address(),
				Username: "testuser",
				PrivateKeyRef: unboundedv1alpha3.SecretKeySelector{
					Name: "machine-key-secret",
				},
				Bastion: &unboundedv1alpha3.BastionSSHSpec{
					Host:     bastionSrv.address(),
					Username: "bastionuser",
					PrivateKeyRef: &unboundedv1alpha3.SecretKeySelector{
						Name: "bastion-key-secret",
					},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(bastionKeySecret).
		Build()

	checker := &DefaultReachabilityChecker{
		Client:  fakeClient,
		Timeout: 5 * time.Second,
	}

	err := checker.CheckReachable(context.Background(), machine)
	require.NoError(t, err)
}

func TestDefaultReachabilityChecker_BastionUnreachable(t *testing.T) {
	t.Parallel()

	rsaKey, _ := generateTestRSAKey(t)
	pemBytes := marshalPrivateKeyPEM(t, rsaKey)

	s := runtime.NewScheme()
	require.NoError(t, unboundedv1alpha3.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))

	bastionKeySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bastion-key-secret", Namespace: SecretNamespaceUnboundedKube},
		Data:       map[string][]byte{"ssh-privatekey": pemBytes},
	}

	// Bastion points to a port with nothing listening.
	machine := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "bastion-unreach-test"},
		Spec: unboundedv1alpha3.MachineSpec{
			SSH: &unboundedv1alpha3.SSHSpec{
				Host:     "127.0.0.1:59998",
				Username: "testuser",
				PrivateKeyRef: unboundedv1alpha3.SecretKeySelector{
					Name: "machine-key-secret",
				},
				Bastion: &unboundedv1alpha3.BastionSSHSpec{
					Host:     "127.0.0.1:59997",
					Username: "bastionuser",
					PrivateKeyRef: &unboundedv1alpha3.SecretKeySelector{
						Name: "bastion-key-secret",
					},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(bastionKeySecret).
		Build()

	checker := &DefaultReachabilityChecker{
		Client:  fakeClient,
		Timeout: 100 * time.Millisecond,
	}

	err := checker.CheckReachable(context.Background(), machine)
	require.Error(t, err)
	require.Contains(t, err.Error(), "SSH dial bastion")
}

func TestDefaultReachabilityChecker_BastionReachable_TargetUnreachable(t *testing.T) {
	t.Parallel()

	bastionSrv := newSSHTestServer(t)

	rsaKey, _ := generateTestRSAKey(t)
	pemBytes := marshalPrivateKeyPEM(t, rsaKey)

	s := runtime.NewScheme()
	require.NoError(t, unboundedv1alpha3.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))

	bastionKeySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bastion-key-secret", Namespace: SecretNamespaceUnboundedKube},
		Data:       map[string][]byte{"ssh-privatekey": pemBytes},
	}

	// Target points to a port with nothing listening; bastion is up.
	machine := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "target-unreach-test"},
		Spec: unboundedv1alpha3.MachineSpec{
			SSH: &unboundedv1alpha3.SSHSpec{
				Host:     "127.0.0.1:59996",
				Username: "testuser",
				PrivateKeyRef: unboundedv1alpha3.SecretKeySelector{
					Name: "machine-key-secret",
				},
				Bastion: &unboundedv1alpha3.BastionSSHSpec{
					Host:     bastionSrv.address(),
					Username: "bastionuser",
					PrivateKeyRef: &unboundedv1alpha3.SecretKeySelector{
						Name: "bastion-key-secret",
					},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(bastionKeySecret).
		Build()

	checker := &DefaultReachabilityChecker{
		Client:  fakeClient,
		Timeout: 5 * time.Second,
	}

	err := checker.CheckReachable(context.Background(), machine)
	require.Error(t, err)
	require.Contains(t, err.Error(), "through bastion")
}

func TestDefaultReachabilityChecker_BastionFallsBackToMachineKey(t *testing.T) {
	t.Parallel()

	targetSrv := newSSHTestServer(t)
	bastionSrv := newSSHTestServer(t)

	rsaKey, _ := generateTestRSAKey(t)
	pemBytes := marshalPrivateKeyPEM(t, rsaKey)

	s := runtime.NewScheme()
	require.NoError(t, unboundedv1alpha3.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))

	machineKeySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-key-secret", Namespace: SecretNamespaceUnboundedKube},
		Data:       map[string][]byte{"ssh-privatekey": pemBytes},
	}

	// Bastion has no PrivateKeyRef — should fall back to machine's SSH key.
	machine := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "bastion-fallback-test"},
		Spec: unboundedv1alpha3.MachineSpec{
			SSH: &unboundedv1alpha3.SSHSpec{
				Host:     targetSrv.address(),
				Username: "testuser",
				PrivateKeyRef: unboundedv1alpha3.SecretKeySelector{
					Name: "machine-key-secret",
				},
				Bastion: &unboundedv1alpha3.BastionSSHSpec{
					Host:     bastionSrv.address(),
					Username: "bastionuser",
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machineKeySecret).
		Build()

	checker := &DefaultReachabilityChecker{
		Client:  fakeClient,
		Timeout: 5 * time.Second,
	}

	err := checker.CheckReachable(context.Background(), machine)
	require.NoError(t, err)
}

func TestDefaultReachabilityChecker_BastionKeySecretMissing(t *testing.T) {
	t.Parallel()

	bastionSrv := newSSHTestServer(t)

	s := runtime.NewScheme()
	require.NoError(t, unboundedv1alpha3.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))

	// No secret created — should fail when trying to look it up.
	machine := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "bastion-no-key-test"},
		Spec: unboundedv1alpha3.MachineSpec{
			SSH: &unboundedv1alpha3.SSHSpec{
				Host:     "127.0.0.1:59995",
				Username: "testuser",
				PrivateKeyRef: unboundedv1alpha3.SecretKeySelector{
					Name: "machine-key-secret",
				},
				Bastion: &unboundedv1alpha3.BastionSSHSpec{
					Host:     bastionSrv.address(),
					Username: "bastionuser",
					PrivateKeyRef: &unboundedv1alpha3.SecretKeySelector{
						Name: "missing-bastion-key",
					},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		Build()

	checker := &DefaultReachabilityChecker{
		Client:  fakeClient,
		Timeout: 5 * time.Second,
	}

	err := checker.CheckReachable(context.Background(), machine)
	require.Error(t, err)
	require.Contains(t, err.Error(), "bastion SSH private key")
}
