// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

func confirmAgentReset(name string, in *os.File, out io.Writer) error {
	return confirmAgentResetWithTerminal(name, in, out, isTerminal(in))
}

func confirmAgentResetWithTerminal(name string, in io.Reader, out io.Writer, terminal bool) error {
	if !terminal {
		return fmt.Errorf("agent-reset removes the unbounded-agent and managed resources; rerun with --force to confirm")
	}

	if _, err := fmt.Fprintf(out, "This will remove the unbounded-agent and managed resources from host %q. Type the machine name to continue: ", name); err != nil {
		return fmt.Errorf("write confirmation prompt: %w", err)
	}

	line, err := bufio.NewReader(io.LimitReader(in, 4096)).ReadString('\n')
	if err != nil {
		return fmt.Errorf("read confirmation: %w; rerun with --force to confirm", err)
	}

	if strings.TrimSpace(line) != name {
		return fmt.Errorf("confirmation did not match machine name %q", name)
	}

	return nil
}
