// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nodestart

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Azure/unbounded/pkg/agent/preflight"
)

const (
	checkKubeletBindAddressName           = "kubelet-bind-address"
	checkContainerdMetricsBindAddressName = "containerd-metrics-bind-address"
	kubeletBindAddress                    = "0.0.0.0:10250"

	// Executable paths inside the nspawn machine rootfs, used to prove that a
	// listener belongs to this installation.
	kubeletExecutablePath    = "usr/local/bin/kubelet"
	containerdExecutablePath = "usr/local/bin/containerd"
)

type bindAddressChecker struct {
	name        string
	address     string
	description string
	log         *slog.Logger
	inspect     func(address string) (string, bool, error)
	owned       func() bool
}

// checkOwnedBindAddress accepts only listeners with both the expected process
// root and executable inode. An uninspectable owner is not proof of ownership.
func checkOwnedBindAddress(log *slog.Logger, name, address, description, root, executable string) preflight.Checker {
	c := bindAddressChecker{
		name: name, address: address, description: description, log: log,
		inspect: func(address string) (string, bool, error) { return inspectTCPListener("/proc", address) },
	}
	c.owned = func() bool { return listenerOwnedByRoot("/proc", address, root, executable) }

	return c
}

func listenerOwnedByRoot(procRoot, address, root, executable string) bool {
	_, portText, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}

	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return false
	}

	wanted := map[string]struct{}{}

	for _, table := range []string{"tcp", "tcp6"} {
		data, err := os.ReadFile(filepath.Join(procRoot, "net", table))
		if errors.Is(err, fs.ErrNotExist) && table == "tcp6" {
			continue
		}

		if err != nil {
			return false
		}

		for inode := range listenerSocketInodes(data, uint16(port)) {
			wanted[inode] = struct{}{}
		}
	}

	rootInfo, err := os.Stat(root)
	if err != nil {
		return false
	}

	exeInfo, err := os.Stat(filepath.Join(root, executable))
	if err != nil {
		return false
	}

	matched := map[string]bool{}

	for _, owner := range socketOwners(procRoot, wanted) {
		base := filepath.Join(procRoot, strconv.Itoa(owner.pid))
		processRoot, rootErr := os.Stat(filepath.Join(base, "root"))

		processExe, exeErr := os.Stat(filepath.Join(base, "exe"))
		if rootErr != nil || exeErr != nil || !os.SameFile(rootInfo, processRoot) || !os.SameFile(exeInfo, processExe) {
			return false
		}

		matched[owner.inode] = true
	}

	return len(wanted) > 0 && len(matched) == len(wanted)
}

func (c bindAddressChecker) Name() string { return c.name }

func (c bindAddressChecker) Check(context.Context) []preflight.Result {
	owner, occupied, err := c.inspect(c.address)
	if err != nil {
		c.log.Debug("TCP listener inspection failed", "address", c.address, "error", err)

		return preflight.ResultsError(c.name, c.address, "%s availability could not be determined", c.description)
	}

	if occupied {
		if c.owned != nil && c.owned() {
			return preflight.ResultsOK(c.name, c.address, c.description+" belongs to this installation")
		}

		if owner != "" {
			return preflight.ResultsError(c.name, c.address, "%s is already in use by process %s", c.description, owner)
		}

		return preflight.ResultsError(c.name, c.address, "%s is already in use", c.description)
	}

	return preflight.ResultsOK(c.name, c.address, c.description+" is available")
}

func inspectTCPListener(procRoot, address string) (string, bool, error) {
	_, portText, err := net.SplitHostPort(address)
	if err != nil {
		return "", false, fmt.Errorf("parse address: %w", err)
	}

	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return "", false, fmt.Errorf("parse port: %w", err)
	}

	inodes := map[string]struct{}{}

	for _, table := range []string{"tcp", "tcp6"} {
		data, err := os.ReadFile(filepath.Join(procRoot, "net", table))
		if errors.Is(err, fs.ErrNotExist) && table == "tcp6" {
			continue
		}

		if err != nil {
			return "", false, fmt.Errorf("read %s socket table: %w", table, err)
		}

		for inode := range listenerSocketInodes(data, uint16(port)) {
			inodes[inode] = struct{}{}
		}
	}

	if len(inodes) == 0 {
		return "", false, nil
	}

	return findSocketOwner(procRoot, inodes), true, nil
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

func findSocketOwner(procRoot string, inodes map[string]struct{}) string {
	owners := socketOwners(procRoot, inodes)
	if len(owners) > 0 {
		owner := owners[0]

		name, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(owner.pid), "comm"))
		if err == nil && strings.TrimSpace(string(name)) != "" {
			return strconv.Quote(strings.TrimSpace(string(name))) + " (PID " + strconv.Itoa(owner.pid) + ")"
		}

		return "PID " + strconv.Itoa(owner.pid)
	}

	return ""
}

type socketOwner struct {
	pid   int
	inode string
}

func socketOwners(procRoot string, inodes map[string]struct{}) []socketOwner {
	processes, err := os.ReadDir(procRoot)
	if err != nil {
		return nil
	}

	var owners []socketOwner

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

			owners = append(owners, socketOwner{pid: pid, inode: inode})
		}
	}

	return owners
}
