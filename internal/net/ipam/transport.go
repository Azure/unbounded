// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package ipam

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"google.golang.org/protobuf/encoding/protodelim"
	"google.golang.org/protobuf/proto"

	ipamproto "github.com/Azure/unbounded/internal/net/ipam/proto"
)

const (
	DefaultSocketPath = "/run/unbounded-net/ipam.sock"
	ProtocolVersion   = 1
	MaxMessageSize    = 64 << 10
	RequestTimeout    = 5 * time.Second
)

func ValidateSocketPath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || len(path) > 107 {
		return fmt.Errorf("IPAM socket path must be a clean absolute path of at most 107 bytes: %q", path)
	}

	return nil
}

func ConfigFromCIDRs(cidrs []string) (*ipamproto.Config, error) {
	if len(cidrs) == 0 {
		return nil, errors.New("node has no verified PodCIDRs")
	}

	config := &ipamproto.Config{}

	for _, cidr := range cidrs {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil || prefix != prefix.Masked() || prefix.Addr().Is4In6() {
			return nil, fmt.Errorf("invalid PodCIDR %q", cidr)
		}

		config.Ranges = append(config.Ranges, &ipamproto.RangeSet{
			Ranges: []*ipamproto.Range{{Subnet: cidr}},
		})
	}

	return config, nil
}

func writeMessage(conn net.Conn, message proto.Message) error {
	if proto.Size(message) > MaxMessageSize {
		return errors.New("IPAM message exceeds maximum size")
	}

	_, err := protodelim.MarshalTo(conn, message)

	return err
}

func GetConfig(ctx context.Context, path string) (*ipamproto.Config, error) {
	if err := ValidateSocketPath(path); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()

	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("connect to IPAM agent: %w", err)
	}
	defer closeConnection(conn)

	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}

	stop := context.AfterFunc(ctx, func() { closeConnection(conn) })
	defer stop()

	if err := writeMessage(conn, &ipamproto.GetIPAMConfigRequest{Version: ProtocolVersion}); err != nil {
		return nil, fmt.Errorf("write IPAM request: %w", err)
	}

	response := &ipamproto.GetIPAMConfigResponse{}
	if err := (protodelim.UnmarshalOptions{MaxSize: MaxMessageSize}).UnmarshalFrom(bufio.NewReader(conn), response); err != nil {
		return nil, fmt.Errorf("read IPAM response: %w", err)
	}

	if response.Version != ProtocolVersion {
		return nil, fmt.Errorf("unsupported IPAM response version %d", response.Version)
	}

	switch payload := response.Payload.(type) {
	case *ipamproto.GetIPAMConfigResponse_Config:
		if payload.Config == nil || len(payload.Config.Ranges) == 0 {
			return nil, errors.New("IPAM agent returned empty configuration")
		}

		return payload.Config, nil
	case *ipamproto.GetIPAMConfigResponse_Error:
		if payload.Error == nil {
			return nil, errors.New("IPAM response has no error details")
		}

		return nil, fmt.Errorf("IPAM agent error: %s: %s", payload.Error.GetCode(), payload.Error.GetMessage())
	default:
		return nil, errors.New("IPAM response has no payload")
	}
}

type ConfigProvider func() (*ipamproto.Config, error)

type Server struct {
	listener  *net.UnixListener
	path      string
	identity  os.FileInfo
	ownership *os.File
	closeOnce sync.Once
	closeErr  error
	mu        sync.Mutex
	conns     map[net.Conn]struct{}
	wg        sync.WaitGroup
	done      chan struct{}
	err       error
}

func Listen(ctx context.Context, path string, provider ConfigProvider) (server *Server, returnErr error) {
	if err := ValidateSocketPath(path); err != nil {
		return nil, err
	}

	if provider == nil {
		return nil, errors.New("IPAM configuration provider is required")
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create IPAM socket directory: %w", err)
	}

	directoryInfo, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return nil, err
	}

	stat, ok := directoryInfo.Sys().(*syscall.Stat_t)
	if !directoryInfo.IsDir() || directoryInfo.Mode().Perm()&0o022 != 0 || !ok || stat.Uid != uint32(os.Geteuid()) {
		return nil, fmt.Errorf("IPAM socket directory %s must be owned by the agent user and not writable by other users", filepath.Dir(path))
	}

	// Keep this file in place so all owners lock the same inode, including
	// while recovering stale sockets and cleaning up after shutdown.
	ownership, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open IPAM socket ownership lock: %w", err)
	}
	defer func() {
		if server == nil {
			returnErr = errors.Join(returnErr, ownership.Close())
		}
	}()

	lockInfo, err := ownership.Stat()
	if err != nil {
		return nil, err
	}

	lockStat, ok := lockInfo.Sys().(*syscall.Stat_t)
	if !lockInfo.Mode().IsRegular() || lockInfo.Mode().Perm()&0o022 != 0 || !ok || lockStat.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("IPAM socket ownership lock must be a regular file owned by the agent user and not writable by other users")
	}

	if err := syscall.Flock(int(ownership.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, fmt.Errorf("acquire IPAM socket ownership lock: %w", err)
	}

	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("refusing to replace non-socket IPAM path %s", path)
		}

		conn, dialErr := net.DialTimeout("unix", path, time.Second)
		if dialErr == nil {
			closeConnection(conn)
			return nil, fmt.Errorf("IPAM socket %s is already serving", path)
		}

		if !errors.Is(dialErr, syscall.ECONNREFUSED) {
			return nil, fmt.Errorf("inspect existing IPAM socket: %w", dialErr)
		}

		current, err := os.Lstat(path)
		if err != nil || !os.SameFile(info, current) {
			return nil, errors.New("IPAM socket changed during inspection")
		}

		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}

	listener.SetUnlinkOnClose(false)

	info, err := os.Lstat(path)
	if err != nil {
		return nil, errors.Join(err, listener.Close())
	}

	if err := os.Chmod(path, 0o600); err != nil {
		return nil, errors.Join(err, listener.Close(), os.Remove(path))
	}

	server = &Server{
		listener: listener, path: path, identity: info,
		ownership: ownership,
		conns:     make(map[net.Conn]struct{}), done: make(chan struct{}),
	}
	server.wg.Go(func() { server.serve(provider) })

	go func() {
		select {
		case <-ctx.Done():
			if err := server.Close(); err != nil {
				slog.Error("close IPAM server", "error", err)
			}
		case <-server.done:
		}
	}()

	return server, nil
}

func (s *Server) serve(provider ConfigProvider) {
	defer close(s.done)

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			s.mu.Lock()
			if !errors.Is(err, net.ErrClosed) {
				s.err = fmt.Errorf("accept IPAM connection: %w", err)
				slog.Error("IPAM listener stopped", "error", s.err)
			}
			s.mu.Unlock()

			return
		}

		s.mu.Lock()
		if len(s.conns) >= 64 {
			s.mu.Unlock()

			closeConnection(conn)

			slog.Debug("IPAM connection limit reached")

			continue
		}

		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		s.wg.Go(func() {
			defer func() {
				closeConnection(conn)

				s.mu.Lock()
				delete(s.conns, conn)
				s.mu.Unlock()
			}()

			if err := conn.SetDeadline(time.Now().Add(RequestTimeout)); err != nil {
				slog.Debug("set IPAM server deadline", "error", err)
				return
			}

			request := &ipamproto.GetIPAMConfigRequest{}
			if err := (protodelim.UnmarshalOptions{MaxSize: MaxMessageSize}).UnmarshalFrom(bufio.NewReader(conn), request); err != nil {
				slog.Debug("reject IPAM request", "error", err)
				return
			}

			response := &ipamproto.GetIPAMConfigResponse{Version: ProtocolVersion}
			if request.Version == 0 {
				response.Payload = &ipamproto.GetIPAMConfigResponse_Error{
					Error: &ipamproto.Error{Code: ipamproto.ErrorCode_INVALID_REQUEST, Message: "IPAM request version is required"},
				}
			} else if request.Version != ProtocolVersion {
				response.Payload = &ipamproto.GetIPAMConfigResponse_Error{
					Error: &ipamproto.Error{Code: ipamproto.ErrorCode_UNSUPPORTED_VERSION, Message: "unsupported IPAM request version"},
				}
			} else if config, err := provider(); err != nil {
				response.Payload = &ipamproto.GetIPAMConfigResponse_Error{
					Error: &ipamproto.Error{Code: ipamproto.ErrorCode_NOT_READY, Message: err.Error()},
				}
			} else if config == nil || len(config.Ranges) == 0 {
				slog.Error("IPAM provider returned empty verified configuration")
				response.Payload = &ipamproto.GetIPAMConfigResponse_Error{
					Error: &ipamproto.Error{Code: ipamproto.ErrorCode_INTERNAL, Message: "empty verified configuration"},
				}
			} else {
				response.Payload = &ipamproto.GetIPAMConfigResponse_Config{Config: config}
			}

			if err := writeMessage(conn, response); err != nil {
				slog.Debug("write IPAM response", "error", err)
			}
		})
	}
}

func (s *Server) Done() <-chan struct{} { return s.done }

func (s *Server) Close() error {
	s.closeOnce.Do(func() { s.closeErr = s.close() })

	return s.closeErr
}

func (s *Server) close() (returnErr error) {
	defer func() { returnErr = errors.Join(returnErr, s.ownership.Close()) }()

	if err := s.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		s.mu.Lock()
		s.err = errors.Join(s.err, err)
		s.mu.Unlock()
	}

	<-s.done

	s.mu.Lock()
	for conn := range s.conns {
		closeConnection(conn)
	}
	s.mu.Unlock()
	s.wg.Wait()

	s.mu.Lock()
	defer s.mu.Unlock()

	info, err := os.Lstat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s.err
	}

	if err != nil {
		return errors.Join(s.err, err)
	}

	if os.SameFile(s.identity, info) {
		return errors.Join(s.err, os.Remove(s.path))
	}

	return s.err
}

func closeConnection(conn net.Conn) {
	if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		slog.Debug("close IPAM connection", "error", err)
	}
}
