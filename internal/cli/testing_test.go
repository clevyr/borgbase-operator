package cli

import (
	"bytes"
	"io"

	"k8s.io/cli-runtime/pkg/genericiooptions"
)

func testStreams() genericiooptions.IOStreams {
	return genericiooptions.IOStreams{In: &bytes.Buffer{}, Out: io.Discard, ErrOut: io.Discard}
}
