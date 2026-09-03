package kube

import (
	"time"

	"golang.org/x/term"
	"k8s.io/client-go/tools/remotecommand"
)

const resizePoll = 250 * time.Millisecond

// IsTerminal reports whether fd is a terminal.
func IsTerminal(fd uintptr) bool { return term.IsTerminal(int(fd)) }

// NewSizeQueue returns a queue that reports terminal resizes on fd.
func NewSizeQueue(fd uintptr) remotecommand.TerminalSizeQueue {
	return &sizeQueue{fd: int(fd)}
}

type sizeQueue struct {
	fd   int
	last remotecommand.TerminalSize
}

func (q *sizeQueue) Next() *remotecommand.TerminalSize {
	for {
		w, h, err := term.GetSize(q.fd)
		if err != nil {
			return nil
		}
		size := remotecommand.TerminalSize{Width: uint16(w), Height: uint16(h)}
		if size != q.last {
			q.last = size
			return &size
		}
		time.Sleep(resizePoll)
	}
}

// WithRawTerminal puts fd into raw mode for the duration of fn.
func WithRawTerminal(fd uintptr, fn func() error) error {
	state, err := term.MakeRaw(int(fd))
	if err != nil {
		return err
	}
	defer func() { _ = term.Restore(int(fd), state) }()
	return fn()
}
