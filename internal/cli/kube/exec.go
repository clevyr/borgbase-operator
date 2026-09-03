// Package kube wraps the client-go plumbing for exec-ing into a pod.
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

const defaultPingPeriod = 5 * time.Second

// ErrNoRESTConfig means no usable client config was found.
var ErrNoRESTConfig = errors.New("no REST client config")

// ExecOptions are the settings for Exec.
type ExecOptions struct {
	Namespace string
	Pod       string
	Container string
	Command   []string

	Stdin          io.Reader
	Stdout, Stderr io.Writer

	TTY       bool
	SizeQueue remotecommand.TerminalSizeQueue
}

// Exec runs a command in a running container and streams its I/O.
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

	roundTripper, err := spdy.NewRoundTripperWithConfig(spdy.RoundTripperConfig{
		TLS:        tlsConfig,
		Proxier:    proxy,
		PingPeriod: defaultPingPeriod,
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
