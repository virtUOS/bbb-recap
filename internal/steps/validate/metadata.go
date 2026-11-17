package validate

import (
	"regexp"
	"slices"
	"strings"

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
	DetailSinkListInvalid   = "sink-list-invalid"   // row 6a
	DetailMatrixRoomInvalid = "matrix-room-invalid" // row 6b
	DetailNoRegistration    = "no-registration"     // row 7
	DetailServerNotAllowed  = "server-not-allowed"  // row 8
	DetailMeetingIDMismatch = "meeting-id-mismatch" // row 9
	DetailTypeNotAllowed    = "type-not-allowed"    // row 9a
	DetailSinkNotAllowed    = "sink-not-allowed"    // row 9a
)

// nameRE is the grammar of meeting types and sink profiles (README "Grammars").
var nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// sinkListRE is the grammar of recap-sink: one to four names joined by ","
// without spaces (metadata-interface.md §1.2).
var sinkListRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}(,[a-z][a-z0-9-]{0,62}){0,3}$`)

// roomRE is the grammar of recap-matrix-room (metadata-interface.md §1.2):
// a room ID "!" opaque [":" server] or a room alias "#" opaque ":" server,
// in an ASCII subset of the Matrix grammar
// (https://spec.matrix.org/v1.19/appendices/#room-ids, #room-aliases,
// #server-name). The length (at most 255) is checked apart.
var roomRE = regexp.MustCompile(`^(?:!` + roomOpaque + `(?::` + roomServer + `)?|#` + roomOpaque + `:` + roomServer + `)$`)

const (
	roomOpaque = `[A-Za-z0-9._~=+/-]+`
	roomServer = `(?:[A-Za-z0-9.-]+|\[[0-9A-Fa-f:.]+\])(?::[0-9]{1,5})?`
	maxRoomLen = 255
)

// Sink types the checks of rows 6a and 6b and the delivery order look at
// (sinks.md §1.1).
const (
	sinkTypeFrontend = "frontend"
	sinkTypeWebhook  = "webhook"
	sinkTypeMatrix   = "matrix"
)

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
	// SinkTypes maps a sink profile to its type, for rows 6a and 6b and
	// the delivery order; a profile without an entry counts as docs.
	SinkTypes map[string]string
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

// CheckMetadata applies rows 2–6b of metadata-interface.md §3 to the
// manifest's metadata in order and returns the first hold, or nil.
func CheckMetadata(meta map[string]string, prefix string, c Configured) *Hold {
	typ, hasType := meta[prefix+"-type"]
	switch {
	case meta[prefix+"-v"] != "1":
		return &Hold{HeldInvalidMetadata, DetailVersion}
	case !hasType || !nameRE.MatchString(typ):
		return &Hold{HeldInvalidMetadata, DetailTypeInvalid}
	case !slices.Contains(c.MeetingTypes, typ):
		return &Hold{HeldUnknownType, DetailTypeUnknown}
	}
	return checkSinks(meta, prefix, c)
}

// checkSinks applies rows 5–6b: the list grammar with distinct names, every
// name configured, at most one frontend and one matrix profile, and the
// Matrix room when a matrix profile is listed.
func checkSinks(meta map[string]string, prefix string, c Configured) *Hold {
	list, ok := meta[prefix+"-sink"]
	if !ok || !sinkListRE.MatchString(list) {
		return &Hold{HeldInvalidMetadata, DetailSinkInvalid}
	}
	names := strings.Split(list, ",")
	if len(slices.Compact(slices.Sorted(slices.Values(names)))) != len(names) {
		return &Hold{HeldInvalidMetadata, DetailSinkInvalid}
	}
	for _, n := range names {
		if !slices.Contains(c.Sinks, n) {
			return &Hold{HeldUnknownSink, DetailSinkUnknown}
		}
	}
	count := map[string]int{}
	for _, n := range names {
		count[c.SinkTypes[n]]++
	}
	if count[sinkTypeFrontend] > 1 || count[sinkTypeMatrix] > 1 {
		return &Hold{HeldInvalidMetadata, DetailSinkListInvalid}
	}
	if count[sinkTypeMatrix] == 1 {
		room, ok := meta[prefix+"-matrix-room"]
		if !ok || len(room) > maxRoomLen || !roomRE.MatchString(room) {
			return &Hold{HeldInvalidMetadata, DetailMatrixRoomInvalid}
		}
	}
	return nil
}

// DeliveryOrder returns the sink list of recap-sink in delivery order
// (sinks.md §1.1): frontend and docs profiles first, then webhook and
// matrix profiles, each group in list order. The list has passed
// CheckMetadata.
func DeliveryOrder(list string, c Configured) []string {
	var first, second []string
	for _, n := range strings.Split(list, ",") {
		if t := c.SinkTypes[n]; t == sinkTypeWebhook || t == sinkTypeMatrix {
			second = append(second, n)
		} else {
			first = append(first, n)
		}
	}
	return append(first, second...)
}

// matrixRoom returns the job's Matrix room: the recap-matrix-room value when
// the list names a matrix profile, else "" (the key is ignored).
func matrixRoom(meta map[string]string, prefix string, names []string, c Configured) string {
	for _, n := range names {
		if c.SinkTypes[n] == sinkTypeMatrix {
			return meta[prefix+"-matrix-room"]
		}
	}
	return ""
}
