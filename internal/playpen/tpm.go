// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package playpen

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const (
	swtpmStartupTimeout = 5 * time.Second
	swtpmShutdownGrace  = 5 * time.Second
	swtpmPollInterval   = 10 * time.Millisecond
)

type commandBuilder func(name string, args ...string) *exec.Cmd

// SWTPM is a running software TPM process.
type SWTPM struct {
	cmd           *exec.Cmd
	done          <-chan error
	socketPath    string
	shutdownGrace time.Duration
	closeOnce     sync.Once
	closeErr      error
}

func BuildSWTPMArgs(cfg Config) []string {
	return []string{
		"socket",
		"--tpm2",
		"--runas", "0",
		"--tpmstate", "dir=" + cfg.TPMStateDir,
		"--ctrl", "type=unixio,path=" + cfg.TPMSocket,
		"--terminate",
	}
}

// StartSWTPM starts swtpm and waits for its control socket to accept QEMU.
func StartSWTPM(ctx context.Context, cfg Config) (*SWTPM, error) {
	return startSWTPM(ctx, cfg, exec.Command, swtpmStartupTimeout, swtpmShutdownGrace)
}

func startSWTPM(
	ctx context.Context,
	cfg Config,
	buildCommand commandBuilder,
	startupTimeout time.Duration,
	shutdownGrace time.Duration,
) (*SWTPM, error) {
	if err := os.MkdirAll(cfg.TPMStateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create TPM state directory %s: %w", cfg.TPMStateDir, err)
	}

	if err := os.MkdirAll(filepath.Dir(cfg.TPMSocket), 0o755); err != nil {
		return nil, fmt.Errorf("create TPM socket directory for %s: %w", cfg.TPMSocket, err)
	}

	if err := removeTPMSocket(cfg.TPMSocket); err != nil {
		return nil, err
	}

	cmd := buildCommand(cfg.SWTPMBinary, BuildSWTPMArgs(cfg)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", cfg.SWTPMBinary, err)
	}

	done := make(chan error, 1)

	go func() {
		done <- cmd.Wait()
	}()

	tpm := &SWTPM{
		cmd:           cmd,
		done:          done,
		socketPath:    cfg.TPMSocket,
		shutdownGrace: shutdownGrace,
	}

	ticker := time.NewTicker(swtpmPollInterval)
	defer ticker.Stop()

	timeout := time.NewTimer(startupTimeout)
	defer timeout.Stop()

	for {
		select {
		case err := <-done:
			if err == nil {
				err = errors.New("process exited successfully")
			}

			return nil, errors.Join(
				fmt.Errorf("swtpm exited before its socket was ready: %w", err),
				removeTPMSocket(cfg.TPMSocket),
			)
		default:
		}

		ready, err := tpmSocketReady(cfg.TPMSocket)
		if err != nil {
			return nil, errors.Join(err, tpm.Close())
		}

		if ready {
			select {
			case err := <-done:
				if err == nil {
					err = errors.New("process exited successfully")
				}

				return nil, errors.Join(
					fmt.Errorf("swtpm exited before its socket was ready: %w", err),
					removeTPMSocket(cfg.TPMSocket),
				)
			default:
				return tpm, nil
			}
		}

		select {
		case <-ctx.Done():
			return nil, errors.Join(fmt.Errorf("start swtpm: %w", ctx.Err()), tpm.Close())
		case <-timeout.C:
			return nil, errors.Join(fmt.Errorf("start swtpm: timed out waiting for %s", cfg.TPMSocket), tpm.Close())
		case <-ticker.C:
		}
	}
}

// Close stops and reaps the swtpm process and removes its control socket.
func (t *SWTPM) Close() error {
	t.closeOnce.Do(func() {
		t.closeErr = t.stop()
	})

	return t.closeErr
}

func (t *SWTPM) stop() error {
	select {
	case <-t.done:
		return removeTPMSocket(t.socketPath)
	default:
	}

	if err := t.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		if killErr := killProcess(t.cmd.Process); killErr != nil {
			return errors.Join(fmt.Errorf("signal swtpm: %w", err), fmt.Errorf("kill swtpm: %w", killErr))
		}
	}

	timer := time.NewTimer(t.shutdownGrace)
	defer timer.Stop()

	select {
	case <-t.done:
	case <-timer.C:
		if err := killProcess(t.cmd.Process); err != nil {
			return fmt.Errorf("kill swtpm after shutdown timeout: %w", err)
		}

		<-t.done
	}

	return removeTPMSocket(t.socketPath)
}

func tpmSocketReady(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("stat TPM socket %s: %w", path, err)
	}

	if info.Mode()&os.ModeSocket == 0 {
		return false, fmt.Errorf("TPM socket path %s is not a Unix socket", path)
	}

	return true, nil
}

func removeTPMSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("stat TPM socket %s: %w", path, err)
	}

	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("TPM socket path %s is not a Unix socket", path)
	}

	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove TPM socket %s: %w", path, err)
	}

	return nil
}
