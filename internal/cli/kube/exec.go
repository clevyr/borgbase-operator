// Package kube holds thin Kubernetes helpers used by the corg CLI that are not
// available from the controller-runtime client the operator uses.
package kube

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	transportspdy "k8s.io/client-go/transport/spdy"
	"k8s.io/streaming/pkg/httpstream/spdy"
)

// defaultPingPeriod matches the SPDY round tripper's usual keepalive.
const defaultPingPeriod = 5 * time.Second

var ErrNoRESTConfig = errors.New("no REST client config")

// ExecOptions describes a single exec into a running container.
type ExecOptions struct {
	Namespace string
	Pod       string
	Container string
	Command   []string

	Stdin          io.Reader
	Stdout, Stderr io.Writer

	// TTY and SizeQueue drive an interactive session. SizeQueue may be nil.
	TTY       bool
	SizeQueue remotecommand.TerminalSizeQueue

	// DisablePing turns off SPDY keepalives. It must be set for long streams
	// such as a restic dump or a tar of a restored tree; see Exec.
	DisablePing bool
}

// Exec runs a command in an existing container and streams it to the caller.
//
// Adapted from github.com/clevyr/kubedb (internal/kubernetes/pod.go), whose
// exec path already solved the streaming problems this CLI hits.
func Exec(ctx context.Context, cfg *rest.Config, cs kubernetes.Interface, opts ExecOptions) error {
	if cfg == nil {
		return ErrNoRESTConfig
	}

	req := cs.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(opts.Namespace).
		Name(opts.Pod).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Command:   opts.Command,
			Container: opts.Container,
			Stdin:     opts.Stdin != nil,
			Stdout:    opts.Stdout != nil,
			Stderr:    opts.Stderr != nil,
			TTY:       opts.TTY,
		}, scheme.ParameterCodec)

	tlsConfig, err := rest.TLSConfigFor(cfg)
	if err != nil {
		return err
	}

	proxy := http.ProxyFromEnvironment
	if cfg.Proxy != nil {
		proxy = cfg.Proxy
	}

	// A nonzero ping period truncates long streams with an unexpected EOF, so
	// anything that pipes a dump or a tar has to turn keepalives off.
	// See https://github.com/kubernetes/kubernetes/issues/60140
	pingPeriod := defaultPingPeriod
	if opts.DisablePing {
		pingPeriod = 0
	}

	roundTripper, err := spdy.NewRoundTripperWithConfig(spdy.RoundTripperConfig{
		TLS:        tlsConfig,
		Proxier:    proxy,
		PingPeriod: pingPeriod,
	})
	if err != nil {
		return err
	}

	wrapper, err := rest.HTTPWrappersForConfig(cfg, roundTripper)
	if err != nil {
		return err
	}

	executor, err := remotecommand.NewSPDYExecutorForTransports(
		wrapper,
		transportspdy.NewUpgraderForStreaming(roundTripper),
		http.MethodPost,
		req.URL(),
	)
	if err != nil {
		return err
	}

	return executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:             opts.Stdin,
		Stdout:            opts.Stdout,
		Stderr:            opts.Stderr,
		Tty:               opts.TTY,
		TerminalSizeQueue: opts.SizeQueue,
	})
}
