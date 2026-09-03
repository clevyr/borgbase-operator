package v1

// Annotations that request one-off work from the operator. They are part of the
// user-facing contract: setting one with `kubectl annotate` is as valid as
// using the CLI.
const (
	// AnnotationTriggerAt requests a backup outside the schedule. Its value is
	// an RFC3339 timestamp, and the operator runs a backup whenever that
	// timestamp is newer than status.lastTriggerTime, so re-annotating with a
	// fresh timestamp triggers another run while a repeated reconcile of the
	// same value does not.
	//
	//	kubectl annotate scheduledbackup/web-files \
	//	  borgbase.clevyr.com/trigger-at="$(date -Is)" --overwrite
	AnnotationTriggerAt = "borgbase.clevyr.com/trigger-at"
)
