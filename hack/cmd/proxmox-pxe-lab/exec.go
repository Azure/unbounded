package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

type Executor interface {
	Run(ctx context.Context, name string, args ...string) error
	Output(ctx context.Context, name string, args ...string) (string, error)
}

type realExecutor struct{}

func (realExecutor) Run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (realExecutor) Output(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

type fakeExecutor struct {
	calls     []string
	responses map[string]error
	outputs   map[string]string
}

func (f *fakeExecutor) Run(_ context.Context, name string, args ...string) error {
	key := strings.TrimSpace(name + " " + strings.Join(args, " "))
	f.calls = append(f.calls, key)
	if f.responses == nil {
		return fmt.Errorf("unexpected command: %s", key)
	}
	resp, ok := f.responses[key]
	if !ok {
		return fmt.Errorf("unexpected command: %s", key)
	}
	return resp
}

func (f *fakeExecutor) Output(_ context.Context, name string, args ...string) (string, error) {
	key := strings.TrimSpace(name + " " + strings.Join(args, " "))
	f.calls = append(f.calls, key)
	if f.outputs == nil {
		return "", fmt.Errorf("unexpected command: %s", key)
	}
	out, ok := f.outputs[key]
	if !ok {
		return "", fmt.Errorf("unexpected command: %s", key)
	}
	return out, nil
}
