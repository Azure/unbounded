// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodestart

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/Azure/unbounded/pkg/agent/preflight"
)

const (
	checkKubeletBindAddressName = "kubelet-bind-address"
	kubeletBindAddress          = "0.0.0.0:10250"
	kubeletBindPort             = uint16(10250)
)

type kubeletBindAddressChecker struct {
	log           *slog.Logger
	listen        func(network, address string) (io.Closer, error)
	findPortOwner func(port uint16) string
}

// CheckKubeletBindAddress returns a non-mutating checker that verifies the
// kubelet bind address is available in the host network namespace.
func CheckKubeletBindAddress(log *slog.Logger) preflight.Checker {
	return kubeletBindAddressChecker{
		log: log,
		listen: func(network, address string) (io.Closer, error) {
			return net.Listen(network, address)
		},
		findPortOwner: func(port uint16) string {
			return findTCPListenerOwner("/proc", port)
		},
	}
}

func (c kubeletBindAddressChecker) Name() string { return checkKubeletBindAddressName }

func (c kubeletBindAddressChecker) Check(context.Context) []preflight.Result {
	listener, err := c.listen("tcp", kubeletBindAddress)
	if err != nil {
		c.log.Debug("kubelet bind address probe failed", "address", kubeletBindAddress, "error", err)

		if errors.Is(err, syscall.EADDRINUSE) {
			if c.findPortOwner != nil {
				if owner := c.findPortOwner(kubeletBindPort); owner != "" {
					return preflight.ResultsError(
						checkKubeletBindAddressName,
						kubeletBindAddress,
						"kubelet bind address is already in use by process %s",
						owner,
					)
				}
			}

			return preflight.ResultsError(
				checkKubeletBindAddressName,
				kubeletBindAddress,
				"kubelet bind address is already in use",
			)
		}

		return preflight.ResultsError(
			checkKubeletBindAddressName,
			kubeletBindAddress,
			"kubelet bind address availability could not be determined",
		)
	}
	defer listener.Close() //nolint:errcheck // best effort close

	return preflight.ResultsOK(
		checkKubeletBindAddressName,
		kubeletBindAddress,
		"kubelet bind address is available",
	)
}

func findTCPListenerOwner(procRoot string, port uint16) string {
	socketTable, err := os.ReadFile(filepath.Join(procRoot, "net", "tcp"))
	if err != nil {
		return ""
	}

	inodes := listenerSocketInodes(socketTable, port)
	if len(inodes) == 0 {
		return ""
	}

	processes, err := os.ReadDir(procRoot)
	if err != nil {
		return ""
	}

	for _, process := range processes {
		pid, err := strconv.Atoi(process.Name())
		if err != nil || !process.IsDir() {
			continue
		}

		fds, err := os.ReadDir(filepath.Join(procRoot, process.Name(), "fd"))
		if err != nil {
			continue
		}

		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(procRoot, process.Name(), "fd", fd.Name()))
			if err != nil {
				continue
			}

			inode := strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")
			if _, ok := inodes[inode]; !ok || target != "socket:["+inode+"]" {
				continue
			}

			name, err := os.ReadFile(filepath.Join(procRoot, process.Name(), "comm"))
			if err == nil {
				if name := strings.TrimSpace(string(name)); name != "" {
					return strconv.Quote(name) + " (PID " + strconv.Itoa(pid) + ")"
				}
			}

			return "PID " + strconv.Itoa(pid)
		}
	}

	return ""
}

func listenerSocketInodes(socketTable []byte, port uint16) map[string]struct{} {
	inodes := map[string]struct{}{}
	scanner := bufio.NewScanner(bytes.NewReader(socketTable))

	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 10 || fields[3] != "0A" {
			continue
		}

		_, portHex, ok := strings.Cut(fields[1], ":")
		if !ok {
			continue
		}

		socketPort, err := strconv.ParseUint(portHex, 16, 16)
		if err == nil && uint16(socketPort) == port {
			inodes[fields[9]] = struct{}{}
		}
	}

	return inodes
}
