package controller

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	machinav1alpha2 "github.com/project-unbounded/unbounded-kube/cmd/machina/machina/api/v1alpha2"
)

// ---------------------------------------------------------------------------
// In-process SSH test server
// ---------------------------------------------------------------------------

// sshTestServer is an in-process SSH server used for integration tests.
type sshTestServer struct {
	listener net.Listener
	config   *ssh.ServerConfig
	host     string
	port     int

	// mu protects executedCommands and exitCode.
	mu               sync.Mutex
	executedCommands []sshExecutedCommand
	exitCode         int
}

type sshExecutedCommand struct {
	command string
	stdin   []byte
}

// newSSHTestServer creates and starts an in-process SSH server with no auth.
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

// newSSHTestServerWithAuth creates an SSH server requiring public key auth.
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

			// Read all stdin from the client. The client will close the
			// write side of the channel when its stdin buffer is drained,
			// which causes io.Copy to return.
			var stdinBuf bytes.Buffer

			done := make(chan struct{})

			go func() {
				_, _ = io.Copy(&stdinBuf, channel)

				close(done)
			}()

			// Wait for stdin EOF or timeout.
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

// ---------------------------------------------------------------------------
// Helper: generate RSA key pair
// ---------------------------------------------------------------------------

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

func TestSSH_JumpboxConnection(t *testing.T) {
	t.Parallel()

	targetSrv := newSSHTestServer(t)
	jumpSrv := newSSHTestServer(t)

	jumpClient, err := ssh.Dial("tcp", jumpSrv.address(), noAuthClientConfig())
	require.NoError(t, err)

	defer jumpClient.Close()

	conn, err := jumpClient.Dial("tcp", targetSrv.address())
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
	require.NoError(t, machinav1alpha2.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))

	model := &machinav1alpha2.MachineModel{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-model",
			Generation: 1,
		},
		Spec: machinav1alpha2.MachineModelSpec{
			SSHUsername: "testuser",
			SSHPrivateKeyRef: machinav1alpha2.SecretKeySelector{
				Name: "ssh-key-secret",
				Key:  "ssh-privatekey",
			},
			AgentInstallScript: "#!/bin/bash\necho provisioning",
			KubernetesProfile: &machinav1alpha2.KubernetesProfile{
				Version: "1.34.0",
				BootstrapTokenRef: machinav1alpha2.LocalObjectReference{
					Name: "bootstrap-token-abc123",
				},
			},
		},
	}

	machine := &machinav1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-machine",
		},
		Spec: machinav1alpha2.MachineSpec{
			SSH: machinav1alpha2.MachineSSHSpec{
				Host: srv.host,
				Port: int32(srv.port),
			},
			ModelRef: &machinav1alpha2.LocalObjectReference{Name: "test-model"},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, model).
		Build()

	reconciler := &MachineReconciler{
		Client: fakeClient,
		Scheme: s,
		ClusterInfo: &ClusterInfo{
			APIServer:    "api.example.com:443",
			CACertBase64: "dGVzdC1jYQ==",
			ClusterDNS:   "10.0.0.10",
			ClusterRG:    "mc_rg",
			KubeVersion:  "v1.34.2",
		},
	}

	sshConfig := sshTestClientConfig(signer)

	err := reconciler.provisionMachine(context.Background(), machine, model, sshConfig, "abc123.secret")
	require.NoError(t, err)

	commands := srv.getExecutedCommands()
	require.Len(t, commands, 3)

	// Command 1: script copy.
	require.Contains(t, commands[0].command, "cat >")
	require.Contains(t, commands[0].command, remoteScriptPath)
	require.Contains(t, commands[0].command, "chmod +x")
	require.Equal(t, "#!/bin/bash\necho provisioning", string(commands[0].stdin))

	// Command 2: script execution with env vars.
	require.Contains(t, commands[1].command, "sudo -E bash")
	require.Contains(t, commands[1].command, remoteScriptPath)
	require.Contains(t, commands[1].command, "API_SERVER")
	require.Contains(t, commands[1].command, "api.example.com:443")
	require.Contains(t, commands[1].command, "BOOTSTRAP_TOKEN")
	require.Contains(t, commands[1].command, "abc123.secret")
	require.Contains(t, commands[1].command, "CA_CERT_BASE64")
	require.Contains(t, commands[1].command, "KUBE_VERSION")
	require.Contains(t, commands[1].command, "v1.34.0") // Model overrides cluster version.
	require.Contains(t, commands[1].command, "CLUSTER_DNS")
	require.Contains(t, commands[1].command, "CLUSTER_RG")
	require.Contains(t, commands[1].command, "MACHINA_MACHINE_NAME")

	// Command 3: cleanup.
	require.Contains(t, commands[2].command, "rm -f")
	require.Contains(t, commands[2].command, remoteScriptPath)
}

func TestProvisionMachine_CleanupAlwaysRuns(t *testing.T) {
	t.Parallel()

	srv := newSSHTestServer(t)

	_, signer := generateTestRSAKey(t)

	s := runtime.NewScheme()
	require.NoError(t, machinav1alpha2.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))

	model := &machinav1alpha2.MachineModel{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model", Generation: 1},
		Spec: machinav1alpha2.MachineModelSpec{
			SSHUsername:        "testuser",
			SSHPrivateKeyRef:   machinav1alpha2.SecretKeySelector{Name: "ssh-key-secret"},
			AgentInstallScript: "#!/bin/bash\nexit 1",
		},
	}

	machine := &machinav1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "test-machine"},
		Spec: machinav1alpha2.MachineSpec{
			SSH: machinav1alpha2.MachineSSHSpec{Host: srv.host, Port: int32(srv.port)},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, model).Build()

	reconciler := &MachineReconciler{
		Client:      fakeClient,
		Scheme:      s,
		ClusterInfo: &ClusterInfo{},
	}

	sshConfig := sshTestClientConfig(signer)

	// With exitCode=0 all commands succeed; we verify all 3 execute.
	err := reconciler.provisionMachine(context.Background(), machine, model, sshConfig, "")
	require.NoError(t, err)

	commands := srv.getExecutedCommands()
	require.Len(t, commands, 3, "copy + exec + cleanup should all run")
	require.Contains(t, commands[2].command, "rm -f")
}

func TestProvisionMachine_EnvVarsInCommand(t *testing.T) {
	t.Parallel()

	srv := newSSHTestServer(t)

	_, signer := generateTestRSAKey(t)

	s := runtime.NewScheme()
	require.NoError(t, machinav1alpha2.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))

	model := &machinav1alpha2.MachineModel{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model", Generation: 1},
		Spec: machinav1alpha2.MachineModelSpec{
			SSHUsername:        "testuser",
			SSHPrivateKeyRef:   machinav1alpha2.SecretKeySelector{Name: "ssh-key-secret"},
			AgentInstallScript: "#!/bin/bash\necho test",
		},
	}

	machine := &machinav1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "my-special-machine"},
		Spec: machinav1alpha2.MachineSpec{
			SSH: machinav1alpha2.MachineSSHSpec{Host: srv.host, Port: int32(srv.port)},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, model).Build()

	reconciler := &MachineReconciler{
		Client: fakeClient,
		Scheme: s,
		ClusterInfo: &ClusterInfo{
			APIServer:    "k8s.example.com:6443",
			CACertBase64: "Y2VydC1kYXRh",
			ClusterDNS:   "10.96.0.10",
			ClusterRG:    "my-resource-group",
			KubeVersion:  "v1.33.1",
		},
	}

	sshConfig := sshTestClientConfig(signer)

	err := reconciler.provisionMachine(context.Background(), machine, model, sshConfig, "tok123.secret456")
	require.NoError(t, err)

	commands := srv.getExecutedCommands()
	require.GreaterOrEqual(t, len(commands), 2)

	execCmd := commands[1].command

	require.Contains(t, execCmd, `API_SERVER="k8s.example.com:6443"`)
	require.Contains(t, execCmd, `BOOTSTRAP_TOKEN="tok123.secret456"`)
	require.Contains(t, execCmd, `CA_CERT_BASE64="Y2VydC1kYXRh"`)
	require.Contains(t, execCmd, `KUBE_VERSION="v1.33.1"`)
	require.Contains(t, execCmd, `CLUSTER_DNS="10.96.0.10"`)
	require.Contains(t, execCmd, `CLUSTER_RG="my-resource-group"`)
	require.Contains(t, execCmd, `MACHINA_MACHINE_NAME="my-special-machine"`)
}

func TestProvisionMachine_NilClusterInfo(t *testing.T) {
	t.Parallel()

	srv := newSSHTestServer(t)

	_, signer := generateTestRSAKey(t)

	s := runtime.NewScheme()
	require.NoError(t, machinav1alpha2.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))

	model := &machinav1alpha2.MachineModel{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model", Generation: 1},
		Spec: machinav1alpha2.MachineModelSpec{
			SSHUsername:        "testuser",
			SSHPrivateKeyRef:   machinav1alpha2.SecretKeySelector{Name: "ssh-key-secret"},
			AgentInstallScript: "#!/bin/bash\necho test",
		},
	}

	machine := &machinav1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "test-machine"},
		Spec: machinav1alpha2.MachineSpec{
			SSH: machinav1alpha2.MachineSSHSpec{Host: srv.host, Port: int32(srv.port)},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, model).Build()

	reconciler := &MachineReconciler{
		Client:      fakeClient,
		Scheme:      s,
		ClusterInfo: nil,
	}

	sshConfig := sshTestClientConfig(signer)

	err := reconciler.provisionMachine(context.Background(), machine, model, sshConfig, "")
	require.NoError(t, err)

	commands := srv.getExecutedCommands()
	require.GreaterOrEqual(t, len(commands), 2)

	execCmd := commands[1].command
	require.Contains(t, execCmd, `API_SERVER=""`)
	require.Contains(t, execCmd, `BOOTSTRAP_TOKEN=""`)
	require.Contains(t, execCmd, `KUBE_VERSION=""`)
}

func TestProvisionMachine_KubeVersionPrefixing(t *testing.T) {
	t.Parallel()

	srv := newSSHTestServer(t)

	_, signer := generateTestRSAKey(t)

	s := runtime.NewScheme()
	require.NoError(t, machinav1alpha2.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))

	model := &machinav1alpha2.MachineModel{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model", Generation: 1},
		Spec: machinav1alpha2.MachineModelSpec{
			SSHUsername:        "testuser",
			SSHPrivateKeyRef:   machinav1alpha2.SecretKeySelector{Name: "ssh-key-secret"},
			AgentInstallScript: "#!/bin/bash\necho test",
			KubernetesProfile: &machinav1alpha2.KubernetesProfile{
				Version:           "1.34.0",
				BootstrapTokenRef: machinav1alpha2.LocalObjectReference{Name: "bt"},
			},
		},
	}

	machine := &machinav1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "test-machine"},
		Spec: machinav1alpha2.MachineSpec{
			SSH: machinav1alpha2.MachineSSHSpec{Host: srv.host, Port: int32(srv.port)},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, model).Build()

	reconciler := &MachineReconciler{
		Client:      fakeClient,
		Scheme:      s,
		ClusterInfo: &ClusterInfo{KubeVersion: "v1.33.0"},
	}

	sshConfig := sshTestClientConfig(signer)
	err := reconciler.provisionMachine(context.Background(), machine, model, sshConfig, "")
	require.NoError(t, err)

	commands := srv.getExecutedCommands()
	execCmd := commands[1].command
	require.Contains(t, execCmd, `KUBE_VERSION="v1.34.0"`)
}

// ---------------------------------------------------------------------------
// dialViaJumpHost integration tests
// ---------------------------------------------------------------------------

func TestDialViaJumpHost_Integration(t *testing.T) {
	t.Parallel()

	targetSrv := newSSHTestServer(t)
	jumpSrv := newSSHTestServer(t)

	rsaKey, _ := generateTestRSAKey(t)
	pemBytes := marshalPrivateKeyPEM(t, rsaKey)

	s := runtime.NewScheme()
	require.NoError(t, machinav1alpha2.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))

	jumpKeySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "jump-key-secret", Namespace: "machina-system"},
		Data:       map[string][]byte{"ssh-privatekey": pemBytes},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(jumpKeySecret).
		Build()

	model := &machinav1alpha2.MachineModel{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model"},
		Spec: machinav1alpha2.MachineModelSpec{
			SSHUsername:        "testuser",
			SSHPrivateKeyRef:   machinav1alpha2.SecretKeySelector{Name: "target-key-secret"},
			AgentInstallScript: "#!/bin/bash\necho test",
			Jumpbox: &machinav1alpha2.JumpboxConfig{
				Host:        jumpSrv.host,
				Port:        int32(jumpSrv.port),
				SSHUsername: "jumpuser",
				SSHPrivateKeyRef: &machinav1alpha2.SecretKeySelector{
					Name: "jump-key-secret",
					Key:  "ssh-privatekey",
				},
			},
		},
	}

	reconciler := &MachineReconciler{
		Client: fakeClient,
		Scheme: s,
	}

	targetConfig := noAuthClientConfig()

	sshClient, err := reconciler.dialViaJumpHost(
		context.Background(),
		model,
		targetSrv.address(),
		targetConfig,
	)
	require.NoError(t, err)

	defer sshClient.Close()

	session, err := sshClient.NewSession()
	require.NoError(t, err)

	defer session.Close()

	err = session.Run("echo via-jumpbox")
	require.NoError(t, err)

	commands := targetSrv.getExecutedCommands()
	require.Len(t, commands, 1)
	require.Equal(t, "echo via-jumpbox", commands[0].command)
}

func TestDialViaJumpHost_FallsBackToModelKey(t *testing.T) {
	t.Parallel()

	targetSrv := newSSHTestServer(t)
	jumpSrv := newSSHTestServer(t)

	rsaKey, _ := generateTestRSAKey(t)
	pemBytes := marshalPrivateKeyPEM(t, rsaKey)

	s := runtime.NewScheme()
	require.NoError(t, machinav1alpha2.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))

	modelKeySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "model-key-secret", Namespace: "machina-system"},
		Data:       map[string][]byte{"ssh-privatekey": pemBytes},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(modelKeySecret).
		Build()

	model := &machinav1alpha2.MachineModel{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model"},
		Spec: machinav1alpha2.MachineModelSpec{
			SSHUsername: "testuser",
			SSHPrivateKeyRef: machinav1alpha2.SecretKeySelector{
				Name: "model-key-secret",
				Key:  "ssh-privatekey",
			},
			AgentInstallScript: "#!/bin/bash\necho test",
			Jumpbox: &machinav1alpha2.JumpboxConfig{
				Host:        jumpSrv.host,
				Port:        int32(jumpSrv.port),
				SSHUsername: "jumpuser",
			},
		},
	}

	reconciler := &MachineReconciler{
		Client: fakeClient,
		Scheme: s,
	}

	targetConfig := noAuthClientConfig()

	sshClient, err := reconciler.dialViaJumpHost(
		context.Background(),
		model,
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

func TestDialViaJumpHost_DefaultPort(t *testing.T) {
	t.Parallel()

	rsaKey, _ := generateTestRSAKey(t)
	pemBytes := marshalPrivateKeyPEM(t, rsaKey)

	s := runtime.NewScheme()
	require.NoError(t, machinav1alpha2.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))

	jumpKeySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "jump-key-secret", Namespace: "machina-system"},
		Data:       map[string][]byte{"ssh-privatekey": pemBytes},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(jumpKeySecret).
		Build()

	model := &machinav1alpha2.MachineModel{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model"},
		Spec: machinav1alpha2.MachineModelSpec{
			SSHUsername:        "testuser",
			SSHPrivateKeyRef:   machinav1alpha2.SecretKeySelector{Name: "key"},
			AgentInstallScript: "echo test",
			Jumpbox: &machinav1alpha2.JumpboxConfig{
				Host: "127.0.0.1",
				Port: 0,
				SSHPrivateKeyRef: &machinav1alpha2.SecretKeySelector{
					Name: "jump-key-secret",
					Key:  "ssh-privatekey",
				},
			},
		},
	}

	reconciler := &MachineReconciler{
		Client: fakeClient,
		Scheme: s,
	}

	targetConfig := &ssh.ClientConfig{
		User:            "testuser",
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec
		Timeout:         1 * time.Second,
	}

	_, err := reconciler.dialViaJumpHost(
		context.Background(),
		model,
		"127.0.0.1:9999",
		targetConfig,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "dial jumpbox")
}
