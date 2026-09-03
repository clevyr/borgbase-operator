// Package healthchecks wires dead-man's-switch reporting into a command.
//
// It contains no healthchecks API client. runitor does the work: it pings
// <apiURL>/<pingKey>/<slug>?create=1 around the wrapped command, and
// healthchecks auto-provisions the check on the first ping, attaching the
// project's notification channels.
package healthchecks

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// Environment variables runitor reads.
const (
	EnvAPIURL  = "HC_API_URL"
	EnvSlug    = "CHECK_SLUG"
	EnvPingKey = "PING_KEY"
	EnvUUID    = "CHECK_UUID"
)

// Config is the operator-level configuration, applied to every job unless
// overridden.
//
// Note that there is no ping key here. Ping keys are per healthchecks project,
// and projects are per client, so a cluster-wide key would file checks into the
// wrong project. It is always supplied per resource.
type Config struct {
	// Enabled turns reporting on by default.
	Enabled bool

	// APIURL is the ping endpoint, for example
	// http://healthchecks.healthchecks:8000/ping.
	APIURL string

	// AutoCreate provisions a check on its first ping.
	AutoCreate bool
}

// Overrides are the per-resource settings. A nil pointer or empty string means
// "inherit from Config".
type Overrides struct {
	Enabled    *bool
	Create     *bool
	APIURL     string
	Slug       string
	PingKeyRef *corev1.SecretKeySelector
	UUIDRef    *corev1.SecretKeySelector
}

// Reporter is a fully resolved configuration, ready to render.
type Reporter struct {
	enabled    bool
	create     bool
	apiURL     string
	slug       string
	pingKeyRef *corev1.SecretKeySelector
	uuidRef    *corev1.SecretKeySelector
}

// Resolve applies overrides on top of the operator configuration.
//
// defaultSlug is used when no slug is given; callers normally pass the
// namespace, which is unique within a healthchecks project.
func Resolve(cfg Config, o Overrides, defaultSlug string) Reporter {
	r := Reporter{
		enabled:    cfg.Enabled,
		create:     cfg.AutoCreate,
		apiURL:     cfg.APIURL,
		slug:       defaultSlug,
		pingKeyRef: o.PingKeyRef,
		uuidRef:    o.UUIDRef,
	}
	if o.Enabled != nil {
		r.enabled = *o.Enabled
	}
	if o.Create != nil {
		r.create = *o.Create
	}
	if o.APIURL != "" {
		r.apiURL = o.APIURL
	}
	if o.Slug != "" {
		r.slug = o.Slug
	}
	return r
}

// Enabled reports whether this job pings healthchecks at all.
func (r Reporter) Enabled() bool { return r.enabled }

// Slug returns the check slug this job reports to.
func (r Reporter) Slug() string { return r.slug }

// byUUID reports whether this job pings a check by UUID rather than by slug.
// That is the adoption path for a check that cannot be given a slug.
func (r Reporter) byUUID() bool { return r.uuidRef != nil }

// Wrap returns cmd wrapped in runitor, so that healthchecks sees a start ping,
// the exit status, and the captured output. A disabled Reporter returns cmd
// unchanged.
func (r Reporter) Wrap(cmd []string) []string {
	if !r.enabled {
		return cmd
	}
	out := []string{"runitor"}
	// -create only means anything for slug pings. runitor reads the slug and
	// ping key from the environment, so no other flag is needed.
	if r.create && !r.byUUID() {
		out = append(out, "-create")
	}
	out = append(out, "--")
	return append(out, cmd...)
}

// Env returns the environment runitor needs, or nil when disabled.
func (r Reporter) Env() ([]corev1.EnvVar, error) {
	if !r.enabled {
		return nil, nil
	}
	if r.apiURL == "" {
		return nil, fmt.Errorf("healthchecks is enabled but no API URL is configured")
	}

	env := []corev1.EnvVar{{Name: EnvAPIURL, Value: r.apiURL}}

	if r.byUUID() {
		return append(env, corev1.EnvVar{
			Name:      EnvUUID,
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: r.uuidRef},
		}), nil
	}

	if r.pingKeyRef == nil {
		return nil, fmt.Errorf(
			"healthchecks is enabled but neither pingKeySecretRef nor uuidSecretRef is set")
	}
	if r.slug == "" {
		return nil, fmt.Errorf("healthchecks is enabled but no slug could be determined")
	}
	return append(env,
		corev1.EnvVar{Name: EnvSlug, Value: r.slug},
		corev1.EnvVar{
			Name:      EnvPingKey,
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: r.pingKeyRef},
		},
	), nil
}
