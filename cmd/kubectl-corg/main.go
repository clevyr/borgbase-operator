package main

import (
	"fmt"
	"os"

	"k8s.io/cli-runtime/pkg/genericiooptions"

	"github.com/clevyr/borgbase-operator/internal/cli"
)

func main() {
	streams := genericiooptions.IOStreams{
		In:     os.Stdin,
		Out:    os.Stdout,
		ErrOut: os.Stderr,
	}

	if err := cli.New(streams, os.Args[0]).Execute(); err != nil {
		_, _ = fmt.Fprintln(streams.ErrOut, "error:", err)
		os.Exit(1)
	}
}
