// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package ipam

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/plugins/plugins/ipam/host-local/backend/disk"
	"google.golang.org/protobuf/encoding/protodelim"
	"google.golang.org/protobuf/proto"

	ipamproto "github.com/Azure/unbounded/internal/net/ipam/proto"
)

func startTestServer(t *testing.T, provider ConfigProvider) string {
	t.Helper()

	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(directory, "ipam.sock")

	server, err := Listen(t.Context(), path, provider)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Error(err)
		}
	})

	return path
}

func testConfig(t *testing.T, cidrs ...string) *ipamproto.Config {
	t.Helper()

	conf, err := ConfigFromCIDRs(cidrs)
	if err != nil {
		t.Fatal(err)
	}

	return conf
}

func testArgs(t *testing.T, path, name, id string) *skel.CmdArgs {
	t.Helper()

	data, err := json.Marshal(map[string]any{
		"name": name, "cniVersion": "0.4.0", "type": "bridge",
		"ipam": map[string]string{"type": "unbounded-ipam", "socketPath": path},
	})
	if err != nil {
		t.Fatal(err)
	}

	return &skel.CmdArgs{ContainerID: id, IfName: "eth0", StdinData: data}
}

func TestAllocationAndOfflineCleanup(t *testing.T) {
	for _, cidrs := range [][]string{
		{"10.244.1.0/29"}, {"fd00:10::/125"}, {"10.244.1.0/29", "fd00:10::/125"},
	} {
		t.Run(cidrs[0], func(t *testing.T) {
			conf := testConfig(t, cidrs...)
			path := startTestServer(t, func() (*ipamproto.Config, error) { return conf, nil })

			var result *current.Result

			plugin := Plugin{
				DataDir: t.TempDir(),
				printResult: func(r types.Result, version string) error {
					if version != "0.4.0" {
						t.Fatalf("result version %s", version)
					}

					var err error

					result, err = current.NewResultFromResult(r)

					return err
				},
			}

			args := testArgs(t, path, "third-party", "pod-a")
			if err := plugin.Add(args); err != nil {
				t.Fatal(err)
			}

			if len(result.IPs) != len(cidrs) {
				t.Fatalf("result IPs: %v", result.IPs)
			}

			if err := plugin.Add(args); err == nil {
				t.Fatal("duplicate ADD succeeded")
			}

			args = testArgs(t, "/missing/socket", "third-party", "pod-a")
			if err := plugin.Check(args); err != nil {
				t.Fatal(err)
			}

			for range 2 {
				if err := plugin.Del(args); err != nil {
					t.Fatal(err)
				}
			}

			if err := plugin.Check(args); err == nil {
				t.Fatal("CHECK without reservation succeeded")
			}
		})
	}
}

func TestRebootAssignmentGate(t *testing.T) {
	var (
		mu       sync.Mutex
		verified *ipamproto.Config
	)

	path := startTestServer(t, func() (*ipamproto.Config, error) {
		mu.Lock()
		defer mu.Unlock()

		if verified == nil {
			return nil, errors.New("Node assignment not verified")
		}

		return verified, nil
	})

	dataDir := t.TempDir()

	store, err := disk.New("unbounded-net", dataDir)
	if err != nil {
		t.Fatal(err)
	}

	if err := store.Lock(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Reserve("old-pod", "eth0", net.ParseIP("10.244.1.2"), "0"); err != nil {
		t.Fatal(err)
	}

	if err := store.Unlock(); err != nil {
		t.Fatal(err)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var address string

	plugin := Plugin{DataDir: dataDir, printResult: func(r types.Result, _ string) error {
		result, err := current.NewResultFromResult(r)
		if err == nil {
			address = result.IPs[0].Address.String()
		}

		return err
	}}

	args := testArgs(t, path, "unbounded-net", "new-pod")
	if err := plugin.Add(args); err == nil {
		t.Fatal("unverified agent allocated an address")
	}

	mu.Lock()
	verified = testConfig(t, "10.244.2.0/24")
	mu.Unlock()

	if err := plugin.Add(args); err != nil {
		t.Fatal(err)
	}

	if address != "10.244.2.2/24" {
		t.Fatalf("allocated stale address %s", address)
	}

	mu.Lock()
	verified = nil
	mu.Unlock()

	if err := plugin.Add(testArgs(t, path, "unbounded-net", "after-restart")); err == nil {
		t.Fatal("restarted agent reused verification")
	}

	if err := plugin.Del(testArgs(t, "/missing", "unbounded-net", "old-pod")); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(dataDir, "unbounded-net", "10.244.1.2")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy reservation not released: %v", err)
	}
}

func TestInvalidCallerConfiguration(t *testing.T) {
	plugin := Plugin{DataDir: t.TempDir()}
	for _, data := range []string{
		`null`, `{`, `{"name":"../escape","ipam":{"type":"unbounded-ipam"}}`,
		`{"name":"net","ipam":{"type":"unbounded-ipam","ranges":[]}}`,
		`{"name":"net","ipam":{"type":"unbounded-ipam"},"runtimeConfig":{"ipRanges":[]}}`,
		`{"name":"net","ipam":{"type":"host-local"}}`,
	} {
		if err := plugin.Add(&skel.CmdArgs{ContainerID: "pod", IfName: "eth0", StdinData: []byte(data)}); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}

	entries, err := os.ReadDir(plugin.DataDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("invalid input created reservations: %v, %v", entries, err)
	}
}

func TestCallerRangeCaseVariantsCannotAllocate(t *testing.T) {
	path := startTestServer(t, func() (*ipamproto.Config, error) {
		return testConfig(t, "10.244.0.0/24"), nil
	})

	for _, field := range []string{"runtimeConfig", "RuntimeConfig", "RUNTIMECONFIG"} {
		for _, option := range []string{"ipRanges", "IPRanges", "IPRANGES"} {
			t.Run(field+"/"+option, func(t *testing.T) {
				plugin := Plugin{DataDir: t.TempDir()}
				args := testArgs(t, path, "net", "pod")

				var input map[string]json.RawMessage
				if err := json.Unmarshal(args.StdinData, &input); err != nil {
					t.Fatal(err)
				}

				input[field] = json.RawMessage(fmt.Sprintf(`{%q:[[{"subnet":"10.99.0.0/24"}]]}`, option))

				data, err := json.Marshal(input)
				if err != nil {
					t.Fatal(err)
				}

				args.StdinData = data
				if err := plugin.Add(args); err == nil || !strings.Contains(err.Error(), "cannot override") {
					t.Fatalf("caller allocation range was not rejected: %v", err)
				}

				entries, err := os.ReadDir(plugin.DataDir)
				if err != nil || len(entries) != 0 {
					t.Fatalf("rejected input created reservations: %v, %v", entries, err)
				}
			})
		}
	}
}

func TestCallerIPAMCaseAliasesCannotAllocate(t *testing.T) {
	for _, field := range []string{"IPAM", "Ipam", "iPaM"} {
		t.Run(field, func(t *testing.T) {
			called := make(chan struct{}, 1)
			path := startTestServer(t, func() (*ipamproto.Config, error) {
				called <- struct{}{}
				return testConfig(t, "10.244.0.0/24"), nil
			})
			callerDataDir := t.TempDir()
			printed := false
			plugin := Plugin{
				DataDir: t.TempDir(),
				printResult: func(types.Result, string) error {
					printed = true
					return nil
				},
			}
			args := testArgs(t, path, "net", "pod")

			var input map[string]json.RawMessage
			if err := json.Unmarshal(args.StdinData, &input); err != nil {
				t.Fatal(err)
			}

			input[field] = json.RawMessage(fmt.Sprintf(`{"subnet":"10.99.0.0/24","dataDir":%q}`, callerDataDir))

			data, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}

			args.StdinData = data
			if err := plugin.Add(args); err == nil || !strings.Contains(err.Error(), "unsupported top-level IPAM alias") {
				t.Fatalf("caller IPAM alias was not rejected: %v", err)
			}

			select {
			case <-called:
				t.Fatal("rejected input reached agent")
			default:
			}

			if printed {
				t.Fatal("rejected input produced a result")
			}

			for _, directory := range []string{plugin.DataDir, callerDataDir} {
				entries, err := os.ReadDir(directory)
				if err != nil || len(entries) != 0 {
					t.Fatalf("rejected input created reservations in %s: %v, %v", directory, entries, err)
				}
			}
		})
	}
}

func TestResultFailureRollsBack(t *testing.T) {
	conf := testConfig(t, "10.244.0.0/24")
	path := startTestServer(t, func() (*ipamproto.Config, error) { return conf, nil })
	expected := errors.New("result output failed")
	plugin := Plugin{DataDir: t.TempDir(), printResult: func(types.Result, string) error { return expected }}

	args := testArgs(t, path, "net", "pod")
	if err := plugin.Add(args); !errors.Is(err, expected) {
		t.Fatalf("result failure lost: %v", err)
	}

	if err := plugin.Check(args); err == nil {
		t.Fatal("output failure retained reservation")
	}
}

func TestDefaultSocketAndNetworkIsolation(t *testing.T) {
	args := &skel.CmdArgs{
		ContainerID: "pod", IfName: "eth0",
		StdinData: []byte(`{"name":"net","ipam":{"type":"unbounded-ipam"}}`),
	}

	_, _, path, err := parseInput(args)
	if err != nil || path != DefaultSocketPath {
		t.Fatalf("default socket %s: %v", path, err)
	}

	conf := testConfig(t, "10.244.0.0/24")
	path = startTestServer(t, func() (*ipamproto.Config, error) { return conf, nil })

	plugin := Plugin{DataDir: t.TempDir(), printResult: func(types.Result, string) error { return nil }}
	for _, name := range []string{"net-a", "net-b"} {
		if err := plugin.Add(testArgs(t, path, name, "pod")); err != nil {
			t.Fatal(err)
		}
	}

	if err := plugin.Del(testArgs(t, "/absent", "net-a", "pod")); err != nil {
		t.Fatal(err)
	}

	if err := plugin.Check(testArgs(t, "/absent", "net-b", "pod")); err != nil {
		t.Fatalf("DEL removed another network: %v", err)
	}
}

func staleTestSocket(t *testing.T) string {
	t.Helper()

	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(directory, "socket")

	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}

	stale.SetUnlinkOnClose(false)

	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}

	return path
}

func TestSocketRecoveryPermissionsAndReplacement(t *testing.T) {
	path := staleTestSocket(t)

	server, err := Listen(t.Context(), path, func() (*ipamproto.Config, error) {
		return testConfig(t, "10.244.0.0/24"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode: %v %v", info, err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	replacement, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()

	if err := server.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cleanup removed replacement: %v", err)
	}
}

func TestSocketRecoveryRespectsOwnershipLock(t *testing.T) {
	path := staleTestSocket(t)

	identity, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}

	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}

	conf := testConfig(t, "10.244.0.0/24")

	provider := func() (*ipamproto.Config, error) { return conf, nil }
	if server, err := Listen(t.Context(), path, provider); err == nil {
		server.Close()
		t.Fatal("recovered a socket while another owner held its lock")
	}

	info, err := os.Lstat(path)
	if err != nil || !os.SameFile(identity, info) {
		t.Fatalf("contended recovery changed the stale socket: %v", err)
	}

	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}

	server, err := Listen(t.Context(), path, provider)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	if _, err := GetConfig(t.Context(), path); err != nil {
		t.Fatalf("recovered listener is unreachable: %v", err)
	}

	if err := server.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := Listen(t.Context(), path, provider)
	if err != nil {
		t.Fatalf("shutdown retained ownership: %v", err)
	}

	if err := restarted.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentStaleSocketRecovery(t *testing.T) {
	path := staleTestSocket(t)
	conf := testConfig(t, "10.244.0.0/24")
	start := make(chan struct{})
	servers := make(chan *Server, 16)

	var wg sync.WaitGroup
	for range cap(servers) {
		wg.Go(func() {
			<-start

			server, err := Listen(t.Context(), path, func() (*ipamproto.Config, error) { return conf, nil })
			if err == nil {
				servers <- server
			}
		})
	}

	close(start)
	wg.Wait()
	close(servers)

	var count int
	for server := range servers {
		count++

		t.Cleanup(func() {
			if err := server.Close(); err != nil {
				t.Error(err)
			}
		})
	}

	if count != 1 {
		t.Fatalf("concurrent recovery returned %d successful listeners, want 1", count)
	}

	if _, err := GetConfig(t.Context(), path); err != nil {
		t.Fatalf("winning listener is unreachable: %v", err)
	}
}

func TestSocketOwnershipLockSafety(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(directory, "socket")
	if err := os.Symlink(target, path+".lock"); err != nil {
		t.Fatal(err)
	}

	conf := testConfig(t, "10.244.0.0/24")

	provider := func() (*ipamproto.Config, error) { return conf, nil }
	if server, err := Listen(t.Context(), path, provider); err == nil {
		server.Close()
		t.Fatal("accepted a symlink as the ownership lock")
	}

	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsafe ownership lock acquired a socket: %v", err)
	}

	data, err := os.ReadFile(target)
	if err != nil || string(data) != "preserve" {
		t.Fatalf("ownership lock check modified target: %q, %v", data, err)
	}
}

func TestSocketDirectoryAndPathSafety(t *testing.T) {
	provider := func() (*ipamproto.Config, error) { return testConfig(t, "10.244.0.0/24"), nil }

	for _, mode := range []os.FileMode{0o700, 0o755, 0o770, 0o777} {
		t.Run(mode.String(), func(t *testing.T) {
			directory := t.TempDir()
			if err := os.Chmod(directory, mode); err != nil {
				t.Fatal(err)
			}

			path := filepath.Join(directory, "socket")

			server, err := Listen(t.Context(), path, provider)
			if mode&0o022 != 0 {
				if err == nil {
					server.Close()
					t.Fatal("accepted writable socket directory")
				}

				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("rejected directory acquired a socket: %v", err)
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			if err := server.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}

	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(directory, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	directoryLink := filepath.Join(directory, "directory-link")
	if err := os.Symlink(directory, directoryLink); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{target, link, filepath.Join(directoryLink, "socket")} {
		if server, err := Listen(t.Context(), path, provider); err == nil {
			server.Close()
			t.Fatalf("accepted unsafe path %s", path)
		}
	}

	data, err := os.ReadFile(target)
	if err != nil || string(data) != "preserve" {
		t.Fatalf("unsafe path check modified target: %q, %v", data, err)
	}
}

func TestTypedProtocolErrors(t *testing.T) {
	for _, test := range []struct {
		name     string
		version  uint32
		provider ConfigProvider
		code     ipamproto.ErrorCode
	}{
		{"missing-version", 0, func() (*ipamproto.Config, error) { return nil, nil }, ipamproto.ErrorCode_INVALID_REQUEST},
		{"empty-provider", 1, func() (*ipamproto.Config, error) { return nil, nil }, ipamproto.ErrorCode_INTERNAL},
		{"not-ready", 1, func() (*ipamproto.Config, error) { return nil, errors.New("pending") }, ipamproto.ErrorCode_NOT_READY},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := startTestServer(t, test.provider)

			conn, err := net.Dial("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()

			if err := conn.SetDeadline(time.Now().Add(RequestTimeout)); err != nil {
				t.Fatal(err)
			}

			if err := writeMessage(conn, &ipamproto.GetIPAMConfigRequest{Version: test.version}); err != nil {
				t.Fatal(err)
			}

			response := &ipamproto.GetIPAMConfigResponse{}
			if err := protodelim.UnmarshalFrom(bufio.NewReader(conn), response); err != nil {
				t.Fatal(err)
			}

			if response.Version != ProtocolVersion || response.GetError().GetCode() != test.code || response.GetConfig() != nil {
				t.Fatalf("unexpected protocol response %v", response)
			}
		})
	}
}

func TestServerShutdownClosesIdleConnections(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(directory, "socket")

	server, err := Listen(t.Context(), path, func() (*ipamproto.Config, error) {
		return testConfig(t, "10.244.0.0/24"), nil
	})
	if err != nil {
		t.Fatal(err)
	}

	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- server.Close() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown waited for idle request timeout")
	}

	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("shutdown retained socket: %v", err)
	}
}

func TestFramingBoundaries(t *testing.T) {
	for _, size := range []int{MaxMessageSize, MaxMessageSize + 1} {
		message := &ipamproto.GetIPAMConfigResponse{Version: 1}
		payloadSize := size - 6
		message.ProtoReflect().SetUnknown(append([]byte{0x7a}, append(binary.AppendUvarint(nil, uint64(payloadSize)), make([]byte, payloadSize)...)...))

		if proto.Size(message) != size {
			t.Fatalf("message size %d, want %d", proto.Size(message), size)
		}

		left, right := net.Pipe()

		errs := make(chan error, 1)
		go func() { errs <- writeMessage(left, message) }()

		if size == MaxMessageSize {
			if err := (protodelim.UnmarshalOptions{MaxSize: MaxMessageSize}).UnmarshalFrom(bufio.NewReader(right), &ipamproto.GetIPAMConfigResponse{}); err != nil {
				t.Fatal(err)
			}

			if err := <-errs; err != nil {
				t.Fatal(err)
			}
		} else if err := <-errs; err == nil {
			t.Fatal("oversized write accepted")
		}

		left.Close()
		right.Close()
	}
}

func TestMalformedFramesFailClosed(t *testing.T) {
	for _, frame := range [][]byte{
		{2, 0xff},
		binary.AppendUvarint(nil, MaxMessageSize+1),
		{1, 0xff},
	} {
		path := filepath.Join(t.TempDir(), "socket")

		listener, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() { listener.Close() })

		go func() {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			defer conn.Close()

			protodelim.UnmarshalFrom(bufio.NewReader(conn), &ipamproto.GetIPAMConfigRequest{})
			conn.Write(frame)
		}()

		if _, err := GetConfig(t.Context(), path); err == nil {
			t.Fatalf("accepted malformed frame %x", frame)
		}
	}
}

func TestPartialAllocationRollbackAndRequestedIPs(t *testing.T) {
	path := startTestServer(t, func() (*ipamproto.Config, error) {
		return testConfig(t, "10.244.0.0/30", "fd00::/126"), nil
	})
	plugin := Plugin{DataDir: t.TempDir(), printResult: func(types.Result, string) error { return nil }}
	args := testArgs(t, path, "net", "pod")

	args.Args = "IP=10.245.0.2"
	if err := plugin.Add(args); err == nil {
		t.Fatal("out-of-range request succeeded")
	}

	if err := plugin.Check(args); err == nil {
		t.Fatal("partial reservations were not rolled back")
	}

	args.Args = "IP=10.244.0.2"
	if err := plugin.Add(args); err != nil {
		t.Fatal(err)
	}

	if err := plugin.Add(testArgs(t, path, "net", "pod-2")); err == nil {
		t.Fatal("exhausted range allocated")
	}
}

func TestConcurrentAllocation(t *testing.T) {
	conf := testConfig(t, "10.244.0.0/24")
	path := startTestServer(t, func() (*ipamproto.Config, error) { return conf, nil })
	dir := t.TempDir()
	addresses := make(chan string, 16)
	errs := make(chan error, 16)

	var wg sync.WaitGroup

	for index := range 16 {
		args := testArgs(t, path, "net", string(rune('a'+index)))

		wg.Go(func() {
			plugin := Plugin{DataDir: dir, printResult: func(r types.Result, _ string) error {
				result, err := current.NewResultFromResult(r)
				if err == nil {
					addresses <- result.IPs[0].Address.String()
				}

				return err
			}}
			errs <- plugin.Add(args)
		})
	}

	wg.Wait()
	close(addresses)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	seen := make(map[string]bool)
	for address := range addresses {
		if seen[address] {
			t.Fatalf("duplicate concurrent reservation %s", address)
		}

		seen[address] = true
	}
}

func TestProtocolAndSocketLifecycle(t *testing.T) {
	conf := testConfig(t, "10.244.1.0/24")

	path := startTestServer(t, func() (*ipamproto.Config, error) { return conf, nil })
	if _, err := Listen(t.Context(), path, func() (*ipamproto.Config, error) { return conf, nil }); err == nil {
		t.Fatal("replaced live listener")
	}

	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if err := writeMessage(conn, &ipamproto.GetIPAMConfigRequest{Version: 99}); err != nil {
		t.Fatal(err)
	}

	response := &ipamproto.GetIPAMConfigResponse{}
	if err := protodelim.UnmarshalFrom(bufio.NewReader(conn), response); err != nil {
		t.Fatal(err)
	}

	if response.GetError().GetCode() != ipamproto.ErrorCode_UNSUPPORTED_VERSION {
		t.Fatalf("unexpected response %v", response)
	}

	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Listen(t.Context(), file, nil); err == nil {
		t.Fatal("replaced regular file")
	}
}

func TestMalformedResponses(t *testing.T) {
	for _, response := range []*ipamproto.GetIPAMConfigResponse{
		{Version: 99},
		{Version: 1},
		{Version: 1, Payload: &ipamproto.GetIPAMConfigResponse_Config{Config: &ipamproto.Config{}}},
		{Version: 1, Payload: &ipamproto.GetIPAMConfigResponse_Error{Error: &ipamproto.Error{Code: 99}}},
	} {
		path := filepath.Join(t.TempDir(), "socket")

		listener, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() { _ = listener.Close() })

		go func() {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			defer conn.Close()

			_ = protodelim.UnmarshalFrom(bufio.NewReader(conn), &ipamproto.GetIPAMConfigRequest{})
			_ = writeMessage(conn, response)
		}()

		if _, err := GetConfig(t.Context(), path); err == nil {
			t.Fatalf("accepted response %v", response)
		}
	}
}

func TestSizeLimitAndClientDeadline(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()

	message := &ipamproto.GetIPAMConfigResponse{Version: 1}
	message.ProtoReflect().SetUnknown(append([]byte{0x7a}, append(binary.AppendUvarint(nil, MaxMessageSize), make([]byte, MaxMessageSize)...)...))

	if proto.Size(message) <= MaxMessageSize {
		t.Fatal("test message is not oversized")
	}

	if err := writeMessage(left, message); err == nil {
		t.Fatal("oversized output accepted")
	}

	path := filepath.Join(t.TempDir(), "socket")

	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	if _, err := GetConfig(ctx, path); err == nil {
		t.Fatal("idle server did not hit client deadline")
	}
}
