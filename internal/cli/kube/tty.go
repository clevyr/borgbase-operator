package kube

import (
	"time"

	"golang.org/x/term"
	"k8s.io/client-go/tools/remotecommand"
)

// resizePoll is how often the terminal is re-measured. SIGWINCH would be
// tidier, but polling keeps this portable and a quarter second is well under
// what anyone notices while dragging a window edge.
const resizePoll = 250 * time.Millisecond

// IsTerminal reports whether fd is an interactive terminal.
func IsTerminal(fd uintptr) bool { return term.IsTerminal(int(fd)) }

// NewSizeQueue reports terminal size changes to a remote session, so a resized
// window reflows the remote program instead of leaving it at 80x24.
func NewSizeQueue(fd uintptr) remotecommand.TerminalSizeQueue {
	return &sizeQueue{fd: int(fd)}
}

type sizeQueue struct {
	fd   int
	last remotecommand.TerminalSize
}

// Next blocks until the size changes, which is the contract remotecommand
// expects. Returning nil ends resize handling.
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

// WithRawTerminal puts fd in raw mode for the duration of fn, so keystrokes
// reach the remote shell instead of being line-buffered locally.
//
// The restore runs even if fn panics; leaving a terminal in raw mode makes the
// user's shell unusable.
func WithRawTerminal(fd uintptr, fn func() error) error {
	state, err := term.MakeRaw(int(fd))
	if err != nil {
		return err
	}
	defer func() { _ = term.Restore(int(fd), state) }()
	return fn()
}
