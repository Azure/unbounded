package main

import (
	"os"

	"github.com/spf13/pflag"
	"k8s.io/cli-runtime/pkg/genericiooptions"

	"github.com/project-unbounded/unbounded-kube/cmd/kubectl-unbounded/internal/cmd"
)

func main() {
	flags := pflag.NewFlagSet("kubectl-unbounded", pflag.ExitOnError)
	pflag.CommandLine = flags

	root := cmd.New(genericiooptions.IOStreams{In: os.Stdin, Out: os.Stdout, ErrOut: os.Stderr})
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
