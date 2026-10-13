// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/pflag"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/Azure/unbounded/internal/logger"
	"github.com/Azure/unbounded/internal/racer"
	"github.com/Azure/unbounded/internal/version"
)

const usage = "usage: racer-controller [--log-level=LEVEL] [--log-format=FORMAT]"

func main() {
	err := run(os.Args[1:], os.Stderr)
	if errors.Is(err, pflag.ErrHelp) {
		if _, err := fmt.Fprintln(os.Stdout, helpText()); err != nil {
			os.Exit(1)
		}

		return
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// newFlagSet binds the command line, which configures only logging. Everything
// else comes from the environment, from the racer-config ConfigMap.
func newFlagSet(logOpts *logger.Options) *pflag.FlagSet {
	*logOpts = logger.Options{Format: logger.FormatJSON}

	fs := pflag.NewFlagSet("racer-controller", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	logOpts.BindFlags(fs)

	return fs
}

func helpText() string {
	var logOpts logger.Options

	return usage + "\n" + newFlagSet(&logOpts).FlagUsages()
}

func parseFlags(args []string) (logger.Options, error) {
	var logOpts logger.Options

	fs := newFlagSet(&logOpts)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return logger.Options{}, err
		}

		return logger.Options{}, fmt.Errorf("%w\n%s", err, helpText())
	}

	if fs.NArg() != 0 {
		return logger.Options{}, errors.New(usage)
	}

	return logOpts, nil
}

func run(args []string, logOutput io.Writer) error {
	logOpts, err := parseFlags(args)
	if err != nil {
		return err
	}

	// Install the logger before loading the configuration so the startup line
	// is logged even when the configuration then fails to load.
	logOpts.Setup(logOutput)

	ctrl.Log.Info("starting racer-controller", "version", version.String())

	cfg, err := racer.LoadConfig()
	if err != nil {
		return err
	}

	return racer.Run(ctrl.SetupSignalHandler(), cfg)
}
