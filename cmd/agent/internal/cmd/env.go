package cmd

import (
	"fmt"
	"os"
	"strings"
)

func requiredEnv(n string) (string, error) {
	v := strings.TrimSpace(os.Getenv(n))
	if v == "" {
		return "", fmt.Errorf("env var %q is required", n)
	}

	return v, nil
}
