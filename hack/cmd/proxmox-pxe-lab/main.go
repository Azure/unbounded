package main

import (
	"context"
	"fmt"
	"os"
)

var newRunExecutor = func() Executor {
	return realExecutor{}
}

func main() {
	cmd, err := parseCommand()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := cmd.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := runCommand(cmd); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runCommand(cmd Command) error {
	return runCommandWithExecutor(context.Background(), cmd, newRunExecutor())
}
