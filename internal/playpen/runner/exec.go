// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
)

type Commander interface {
	Run(ctx context.Context, name string, args ...string) error
	Start(ctx context.Context, name string, args []string, stdoutPath, stderrPath string) (Process, error)
}

type Process interface {
	PID() int
	Exited() bool
	Signal(sig os.Signal) error
	Kill() error
	Wait() error
}

type OSCommander struct{}

func (OSCommander) Run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

func (OSCommander) Start(ctx context.Context, name string, args []string, stdoutPath, stderrPath string) (Process, error) {
	cmd := exec.CommandContext(ctx, name, args...)

	stdout, err := os.OpenFile(stdoutPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}

	stderr, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, errors.Join(err, stdout.Close())
	}

	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return nil, errors.Join(err, stdout.Close(), stderr.Close())
	}

	p := &osProcess{cmd: cmd, done: make(chan struct{})}

	go func() {
		waitErr := cmd.Wait()
		stdoutErr := stdout.Close()
		stderrErr := stderr.Close()

		if waitErr != nil {
			p.err = waitErr
		} else {
			p.err = errors.Join(stdoutErr, stderrErr)
		}

		close(p.done)
	}()

	return p, nil
}

type osProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

func (p *osProcess) PID() int {
	if p.cmd.Process == nil {
		return 0
	}

	return p.cmd.Process.Pid
}

func (p *osProcess) Exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *osProcess) Signal(sig os.Signal) error {
	if p.cmd.Process == nil {
		return nil
	}

	return p.cmd.Process.Signal(sig)
}

func (p *osProcess) Kill() error {
	if p.cmd.Process == nil {
		return nil
	}

	return p.cmd.Process.Kill()
}

func (p *osProcess) Wait() error {
	<-p.done

	return p.err
}
