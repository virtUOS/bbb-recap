package validate

import (
	"regexp"
	"slices"

	"github.com/virtUOS/bbb-recap/internal/config"
)

// Held reasons of metadata-interface.md §3 (docs/reference/README.md).
const (
	HeldInvalidMetadata = "invalid_metadata"
	HeldUnknownType     = "unknown_type"
	HeldUnknownSink     = "unknown_sink"
	HeldNoRegistration  = "no_registration"
)

// Detail codes, one per check of metadata-interface.md §3 (rows 2–9a). The
// admin job shows the code of the first failing check (architecture.md §4).
// server-not-allowed and meeting-id-mismatch are pinned by CT-META-05 and
// CT-JOIN-01, type-not-allowed and sink-not-allowed by the reference table (row
// 9a); the others are chosen here.
const (
	DetailVersion           = "unsupported-version" // row 2
	DetailTypeInvalid       = "type-invalid"        // row 3
	DetailTypeUnknown       = "type-not-configured" // row 4
	DetailSinkInvalid       = "sink-invalid"        // row 5
	DetailSinkUnknown       = "sink-not-configured" // row 6
	DetailNoRegistration    = "no-registration"     // row 7
	DetailServerNotAllowed  = "server-not-allowed"  // row 8
	DetailMeetingIDMismatch = "meeting-id-mismatch" // row 9
	DetailTypeNotAllowed    = "type-not-allowed"    // row 9a
	DetailSinkNotAllowed    = "sink-not-allowed"    // row 9a
)

// nameRE is the grammar of meeting types and sink profiles (README "Grammars").
var nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// Hold is a held outcome: the reason and the detail code.
type Hold struct {
	Reason, Detail string
}

// Configured lists the names the service knows (metadata-interface.md §3
// rows 4 and 6) and which front ends may request them (row 9a). A meeting
// type is configured when it is in the meeting-types file and its format
// directory exists (cmd/recap).
type Configured struct {
	MeetingTypes []string
	Sinks        []string
	// FrontendTypes and FrontendSinks are RECAP_FRONTEND_TYPES and
	// RECAP_FRONTEND_SINKS. A front end without an entry in a list may
	// request every configured name.
	FrontendTypes []config.FrontendName
	FrontendSinks []config.FrontendName
}

// allowed reports whether frontendID may request name under list (row 9a).
func allowed(list []config.FrontendName, frontendID, name string) bool {
	listed := false
	for _, e := range list {
		if e.FrontendID == frontendID {
			if e.Name == name {
				return true
			}
			listed = true
		}
	}
	return !listed
}

// CheckMetadata applies rows 2–6 of metadata-interface.md §3 to the
// manifest's metadata in order and returns the first hold, or nil.
func CheckMetadata(meta map[string]string, prefix string, c Configured) *Hold {
	typ, hasType := meta[prefix+"-type"]
	sink, hasSink := meta[prefix+"-sink"]
	switch {
	case meta[prefix+"-v"] != "1":
		return &Hold{HeldInvalidMetadata, DetailVersion}
	case !hasType || !nameRE.MatchString(typ):
		return &Hold{HeldInvalidMetadata, DetailTypeInvalid}
	case !slices.Contains(c.MeetingTypes, typ):
		return &Hold{HeldUnknownType, DetailTypeUnknown}
	case !hasSink || !nameRE.MatchString(sink):
		return &Hold{HeldInvalidMetadata, DetailSinkInvalid}
	case !slices.Contains(c.Sinks, sink):
		return &Hold{HeldUnknownSink, DetailSinkUnknown}
	}
	return nil
}
