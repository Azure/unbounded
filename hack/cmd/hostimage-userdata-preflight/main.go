// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// hostimage-userdata-preflight rejects cloud-init user-data that would replace
// an image-baked users list.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	metalcloudinit "github.com/Azure/unbounded/internal/metalman/cloudinit"
)

func validateUserData(reader io.Reader) error {
	return metalcloudinit.ValidateUserData(reader)
}

func openInput(args []string, stdin io.Reader) (io.Reader, func() error, error) {
	if len(args) > 1 {
		return nil, nil, errors.New("usage: hostimage-userdata-preflight [FILE|-]")
	}
	if len(args) == 0 || args[0] == "-" {
		return stdin, func() error { return nil }, nil
	}

	file, err := os.Open(args[0])
	if err != nil {
		return nil, nil, fmt.Errorf("open user-data: %w", err)
	}

	return file, file.Close, nil
}

func run(args []string, stdin io.Reader, stderr io.Writer) int {
	input, closeInput, err := openInput(args, stdin)
	if err != nil {
		fmt.Fprintln(stderr, err)

		return 2
	}
	defer func() {
		if err := closeInput(); err != nil {
			fmt.Fprintf(stderr, "close user-data: %v\n", err)
		}
	}()

	if err := validateUserData(input); err != nil {
		fmt.Fprintln(stderr, err)

		return 1
	}

	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stderr))
}
