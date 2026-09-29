//go:build linux

package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestExecSignal(t *testing.T) {
	mode := os.Getenv("RACER_EXEC_TEST")
	if mode == "exec" {
		path, err := os.Executable()
		if err != nil {
			panic(err)
		}

		if err := os.Setenv("RACER_EXEC_TEST", "child"); err != nil {
			panic(err)
		}

		if err := syscall.Exec(path, []string{path, "-test.run=TestExecSignal", "--", "unchanged-arg"}, os.Environ()); err != nil {
			panic(err)
		}
	}

	if mode == "child" {
		if os.Args[len(os.Args)-1] != "unchanged-arg" {
			os.Exit(24)
		}

		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM)
		fmt.Println(os.Getpid())
		<-ch
		os.Exit(23)
	}

	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(path, "-test.run=TestExecSignal")

	cmd.Env = append(os.Environ(), "RACER_EXEC_TEST=exec")

	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = cmd.Process.Kill() })

	line := make(chan string, 1)

	go func() {
		scanner := bufio.NewScanner(output)
		if scanner.Scan() {
			line <- scanner.Text()
		}
	}()

	select {
	case pid := <-line:
		if pid != fmt.Sprint(cmd.Process.Pid) {
			t.Fatal("exec changed PID")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("child readiness timeout")
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)

	go func() { done <- cmd.Wait() }()

	select {
	case <-done:
		if cmd.ProcessState.ExitCode() != 23 {
			t.Fatalf("exit=%d", cmd.ProcessState.ExitCode())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("TERM not delivered to exec child")
	}
}

func TestProof(t *testing.T) {
	good := response{Nonce: "nonce", Node: "node", NodeUID: "uid", BootID: "boot", Digest: strings.Repeat("a", 64), Sequence: 1, Until: 130}
	if err := validate(good, "nonce", "node", "boot", 100); err != nil {
		t.Fatal(err)
	}

	for _, mutate := range []func(*response){
		func(r *response) { r.Nonce = "replay" },
		func(r *response) { r.BootID = "previous" },
		func(r *response) { r.Node = "other" },
		func(r *response) { r.NodeUID = "" },
		func(r *response) { r.Digest = "bad" },
		func(r *response) { r.Until = 100 },
		func(r *response) { r.Until = 200 },
	} {
		bad := good
		mutate(&bad)

		if validate(bad, "nonce", "node", "boot", 100) == nil {
			t.Fatal("accepted invalid proof")
		}
	}
}
