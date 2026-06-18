// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type sshRunner struct {
	keyPath string
	options []string
}

type sshProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
}

type startedForward struct {
	url  string
	proc *sshProcess
}

func (r sshRunner) writeFile(ctx context.Context, target, remotePath, body string, sudo bool) error {
	dir := path.Dir(remotePath)
	remote := fmt.Sprintf("mkdir -p %s && cat > %s", shellQuote(dir), shellQuote(remotePath))
	if sudo {
		remote = fmt.Sprintf("sudo -n sh -c %s sh %s %s", shellQuote("mkdir -p \"$1\" && cat > \"$2\""), shellQuote(dir), shellQuote(remotePath))
	}

	cmd := exec.CommandContext(ctx, "ssh", append(r.baseArgs(target), remote)...)
	cmd.Stdin = strings.NewReader(body)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

func (r sshRunner) startDaemon(ctx context.Context, target, binary, configPath string, sudo bool) (*sshProcess, error) {
	remote := remoteDaemonCommand(binary, configPath, sudo)
	cmd := exec.CommandContext(ctx, "ssh", append(r.baseArgs(target), remote)...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	proc := newSSHProcess(cmd)

	return proc, nil
}

func remoteDaemonCommand(binary, configPath string, sudo bool) string {
	args := []string{binary, "--config", configPath}
	if sudo {
		args = append([]string{"sudo", "-n"}, args...)
	}

	return fmt.Sprintf("sh -c %s sh %s", shellQuote(daemonWrapperScript), shellJoin(args))
}

const daemonWrapperScript = `
pid=
cleanup() {
    trap - INT TERM HUP EXIT
    if [ -n "$pid" ]; then
        kill -TERM -"$pid" 2>/dev/null || kill -TERM "$pid" 2>/dev/null || true
        i=0
        while kill -0 "$pid" 2>/dev/null && [ "$i" -lt 5 ]; do
            sleep 1
            i=$((i + 1))
        done
        if kill -0 "$pid" 2>/dev/null; then
            kill -KILL -"$pid" 2>/dev/null || kill -KILL "$pid" 2>/dev/null || true
        fi
        wait "$pid" 2>/dev/null || true
    fi
}
trap cleanup INT TERM HUP EXIT
setsid "$@" &
pid=$!
wait "$pid"
status=$?
trap - INT TERM HUP EXIT
exit "$status"
`

func (r sshRunner) startForward(ctx context.Context, target, metricsAddr string) (*startedForward, error) {
	localPort, err := freeLocalPort()
	if err != nil {
		return nil, err
	}

	args := r.optionArgs()
	args = append(args,
		"-N",
		"-o", "ExitOnForwardFailure=yes",
		"-L", fmt.Sprintf("127.0.0.1:%d:%s", localPort, metricsAddr),
		target,
	)

	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	proc := newSSHProcess(cmd)

	time.Sleep(200 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		proc.stop()

		return nil, err
	}

	return &startedForward{
		url:  fmt.Sprintf("http://127.0.0.1:%d/metrics", localPort),
		proc: proc,
	}, nil
}

func (r sshRunner) baseArgs(target string) []string {
	args := r.optionArgs()
	args = append(args, target)

	return args
}

func (r sshRunner) optionArgs() []string {
	args := []string{}
	if r.keyPath != "" {
		args = append(args, "-i", r.keyPath)
	}

	for _, opt := range r.options {
		args = append(args, "-o", opt)
	}

	return args
}

func newSSHProcess(cmd *exec.Cmd) *sshProcess {
	proc := &sshProcess{cmd: cmd, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(proc.done)
	}()

	return proc
}

func (p *sshProcess) stop() {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}

	pid := p.cmd.Process.Pid
	if pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGTERM)
	}

	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		if pid > 0 {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
		}
	}
}

func freeLocalPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}

	defer func() { _ = listener.Close() }()

	_, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		return 0, err
	}

	port, err := strconv.Atoi(portText)
	if err != nil {
		return 0, err
	}

	return port, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func shellJoin(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, shellQuote(arg))
	}

	return strings.Join(quoted, " ")
}
