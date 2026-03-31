package kube

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"

	"github.com/project-unbounded/unbounded-kube/internal/helpers"
)

type KubectlFunc func(context.Context) *exec.Cmd

const (
	kubectlBinary = "kubectl"
)

func CheckKubectlAvailable() error {
	if _, err := exec.LookPath(kubectlBinary); err != nil {
		return fmt.Errorf("kubectl not found in PATH: %w", err)
	}

	return nil
}

func Kubectl(env []string, kubeconfig string) KubectlFunc {
	return func(ctx context.Context) *exec.Cmd {
		envMap := helpers.EnvSliceToMap(env)
		envMap["KUBECONFIG"] = kubeconfig

		c := exec.CommandContext(ctx, kubectlBinary)
		c.Env = helpers.EnvMapToSlice(envMap)

		return c
	}
}

func KubectlCmd(ctx context.Context, logger *slog.Logger, kubectl KubectlFunc, args ...string) error {
	k := kubectl(ctx)
	k.Args = append(k.Args, args...)

	stdout, err := k.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	stderr, err := k.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	if err := k.Start(); err != nil {
		return fmt.Errorf("failed to start kubectl: %w", err)
	}

	go streamLogs(ctx, logger, stdout, slog.LevelInfo)
	go streamLogs(ctx, logger, stderr, slog.LevelError)

	if err := k.Wait(); err != nil {
		return fmt.Errorf("kubectl command failed: %w", err)
	}

	return nil
}

func streamLogs(ctx context.Context, logger *slog.Logger, reader io.Reader, level slog.Level) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
			logger.Log(ctx, level, scanner.Text())
		}
	}
}
