//go:build linux

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// racer-guard-launch checks the local root-owned guard before every DP exec.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"time"
)

const (
	socketPath = "/run/racer-guard/start.sock"
	binaryPath = "/usr/local/bin/racer-dataplane"
)

type response struct {
	Nonce    string `json:"nonce"`
	Node     string `json:"node"`
	NodeUID  string `json:"nodeUID"`
	BootID   string `json:"bootID"`
	Digest   string `json:"contentDigest"`
	Sequence uint64 `json:"sequence"`
	Until    int64  `json:"valid_until"`
}

func validate(r response, nonce, node, boot string, now int64) error {
	digest, err := hex.DecodeString(r.Digest)
	if err != nil || len(digest) != 32 || r.Nonce != nonce || r.Node != node || r.NodeUID == "" ||
		r.BootID != boot || r.Sequence == 0 || r.Until <= now+2 || r.Until > now+60 {
		return errors.New("guard proof invalid or expired")
	}

	return nil
}

func securePath(path string, socket bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}

	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != 0 || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("untrusted guard path")
	}

	if socket {
		if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
			return errors.New("unsafe guard socket")
		}
	} else if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return errors.New("unsafe guard directory")
	}

	return nil
}

func check() error {
	if os.Geteuid() != 0 {
		return errors.New("launcher requires approved root DP identity")
	}

	if err := securePath("/run/racer-guard", false); err != nil {
		return err
	}

	if err := securePath(socketPath, true); err != nil {
		return err
	}

	node := os.Getenv("NODE_NAME")
	if node == "" {
		return errors.New("missing node name")
	}

	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return err
	}

	nonceBytes := make([]byte, 32)
	if _, err := rand.Read(nonceBytes); err != nil {
		return err
	}

	nonce := hex.EncodeToString(nonceBytes)

	connection, err := net.DialTimeout("unix", socketPath, 2*time.Second)
	if err != nil {
		return err
	}

	defer func() {
		if closeErr := connection.Close(); closeErr != nil {
			fmt.Fprintln(os.Stderr, "guard connection close:", closeErr)
		}
	}()

	if err := connection.SetDeadline(time.Now().Add(25 * time.Second)); err != nil {
		return err
	}

	unix, ok := connection.(*net.UnixConn)
	if !ok {
		return errors.New("guard connection is not Unix")
	}

	raw, err := unix.SyscallConn()
	if err != nil {
		return err
	}

	var credentialErr error

	if err := raw.Control(func(fd uintptr) {
		credentials, e := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)

		credentialErr = e
		if e == nil && credentials.Uid != 0 {
			credentialErr = errors.New("guard server is not root")
		}
	}); err != nil {
		return err
	}

	if credentialErr != nil {
		return credentialErr
	}

	if err := json.NewEncoder(connection).Encode(map[string]string{"nonce": nonce, "node": node}); err != nil {
		return err
	}

	var proof response

	decoder := json.NewDecoder(io.LimitReader(connection, 4096))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&proof); err != nil {
		return err
	}

	return validate(proof, nonce, node, strings.TrimSpace(string(boot)), time.Now().Unix())
}

func main() {
	if err := check(); err != nil {
		fmt.Fprintln(os.Stderr, "racer start denied:", err)
		os.Exit(1)
	}
	// No shell, configurable executable, child DP, or surviving Go runtime threads.
	if err := syscall.Exec(binaryPath, append([]string{binaryPath}, os.Args[1:]...), os.Environ()); err != nil {
		fmt.Fprintln(os.Stderr, "racer exec failed:", err)
		os.Exit(1)
	}
}
