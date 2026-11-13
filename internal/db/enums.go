package db

// Values of the job enum columns (docs/reference/README.md "States and
// enums"), shared by the packages that list or count them. Read only.
var (
	JobStates   = []string{"received", "running", "held", "failed", "delivered", "cleaned"}
	HeldReasons = []string{"invalid_metadata", "unknown_type", "unknown_sink", "no_registration",
		"owner_unresolved", "delivery_uncertain"}
	FailureReasons = []string{"unsupported_audio_bridge", "events_mismatch", "manifest_metadata_mismatch",
		"retries_exhausted", "all_tracks_failed", "config_error", "rejected_by_provider", "rejected_by_sink",
		"internal_error"}
)
