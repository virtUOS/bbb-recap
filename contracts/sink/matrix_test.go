package sink

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The Matrix sink (sinks.md §4c, ADR-016): CT-SINK-30 to CT-SINK-35. They
// drive `recap tool sink` (RECAP_BIN) with testdata/sink/sinks-matrix.yaml
// against a mock homeserver that the test serves over HTTPS. The mock serves
// the five client-server endpoints of §4c.2 as the Matrix spec v1.19
// describes them (https://spec.matrix.org/v1.19/client-server-api/); error
// bodies are {"errcode", "error"} with the mock's own texts. Written before
// the implementation: they fail until the service has the matrix sink type.

const matrixProfile = "matrix-main"

// Fixture rooms of the mock homeserver (fixtures.md §5 host matrix.recap.test).
const (
	roomJoined   = "!fixturejoined01:matrix.recap.test"
	roomInvited  = "!fixtureinvited01:matrix.recap.test"
	roomPublic   = "!fixturepublic01:matrix.recap.test"                    // joinable, but the bot was not invited
	aliasTeam    = "#fixture-team:matrix.recap.test"                       // → roomJoined
	aliasUnknown = "#fixture-unknown:matrix.recap.test"                    // not mapped: 404
	roomNoServer = "!fixtureRoomV12abcdefghijklmnopqrstuvwxyz0123456789_-" // room version 12 form, joined
)

// TestMatrixSink holds the Matrix sink tests of sinks.md §4c.7.
func TestMatrixSink(t *testing.T) {
	t.Run("CT-SINK-30 notice to a joined room", testMatrixJoined)
	t.Run("CT-SINK-31 room alias", testMatrixAlias)
	t.Run("CT-SINK-32 pending invitation only", testMatrixInvite)
	t.Run("CT-SINK-33 error classes", testMatrixClasses)
	t.Run("CT-SINK-34 re-entry and transaction ID", testMatrixReentry)
	t.Run("CT-SINK-35 pre-checks, title only, config, logs", testMatrixPreChecks)
}

// --- mock homeserver ---

// mxRequest is one request the mock received. endpoint is one of
// joined_rooms, directory, sync, join, send (or unknown).
type mxRequest struct {
	method, endpoint, room, txn, alias string
	query                              url.Values
	header                             http.Header
	body                               []byte
}

// mxFault is one queued answer for an endpoint: a status with an errcode, or
// (send only) post the event and then hang or drop the connection.
type mxFault struct {
	status        int
	errcode       string
	location      string // Location header, for 3xx
	hangAfterPost bool
	dropAfterPost bool
}

type mxEvent struct {
	room, txn, eventID string
	content            []byte
}

type mockHomeserver struct {
	token string
	hold  time.Duration
	// onRequest, if set, runs for every request before it is answered.
	onRequest func(mxRequest)

	mu       sync.Mutex
	joined   map[string]bool
	invited  map[string]bool
	public   map[string]bool
	aliases  map[string]string
	faults   map[string][]mxFault
	requests []mxRequest
	events   []mxEvent
}

func newMockHomeserver(t *testing.T) *mockHomeserver {
	t.Helper()
	m := &mockHomeserver{token: fixtureMatrixToken(t), hold: 10 * time.Second}
	m.reset()
	return m
}

func fixtureMatrixToken(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(string(readFile(t, testdataPath(t, "secrets", "matrix-bot.token"))))
}

// reset restores the fixture rooms and forgets requests, events and faults.
func (m *mockHomeserver) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.joined = map[string]bool{roomJoined: true, roomNoServer: true}
	m.invited = map[string]bool{roomInvited: true}
	m.public = map[string]bool{roomPublic: true}
	m.aliases = map[string]string{aliasTeam: roomJoined}
	m.faults = map[string][]mxFault{}
	m.requests, m.events = nil, nil
}

func (m *mockHomeserver) queue(endpoint string, f ...mxFault) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.faults[endpoint] = append(m.faults[endpoint], f...)
}

func (m *mockHomeserver) received() []mxRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]mxRequest(nil), m.requests...)
}

func (m *mockHomeserver) posted() []mxEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]mxEvent(nil), m.events...)
}

func endpoints(rs []mxRequest) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.endpoint)
	}
	return out
}

func mxError(w http.ResponseWriter, status int, errcode string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"errcode": errcode, "error": "mock-homeserver: " + strings.ToLower(errcode)})
}

func mxJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// parse maps a decoded path to an endpoint of sinks.md §4c.2.
func parseMatrixPath(method, path string) mxRequest {
	const p = "/_matrix/client/v3/"
	r := mxRequest{method: method, endpoint: "unknown"}
	rest, ok := strings.CutPrefix(path, p)
	if !ok {
		return r
	}
	parts := strings.Split(rest, "/")
	switch {
	case method == http.MethodGet && rest == "joined_rooms":
		r.endpoint = "joined_rooms"
	case method == http.MethodGet && rest == "sync":
		r.endpoint = "sync"
	case method == http.MethodGet && len(parts) == 3 && parts[0] == "directory" && parts[1] == "room":
		r.endpoint, r.alias = "directory", parts[2]
	case method == http.MethodPost && len(parts) == 3 && parts[0] == "rooms" && parts[2] == "join":
		r.endpoint, r.room = "join", parts[1]
	case method == http.MethodPost && len(parts) == 2 && parts[0] == "join":
		r.endpoint, r.room = "join", parts[1]
	case method == http.MethodPut && len(parts) == 5 && parts[0] == "rooms" && parts[2] == "send" && parts[3] == "m.room.message":
		r.endpoint, r.room, r.txn = "send", parts[1], parts[4]
	}
	return r
}

func (m *mockHomeserver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	req := parseMatrixPath(r.Method, r.URL.Path)
	req.query, req.header, req.body = r.URL.Query(), r.Header.Clone(), body
	if m.onRequest != nil {
		m.onRequest(req)
	}
	m.mu.Lock()
	m.requests = append(m.requests, req)
	var f mxFault
	if q := m.faults[req.endpoint]; len(q) > 0 {
		f, m.faults[req.endpoint] = q[0], q[1:]
	}
	m.mu.Unlock()

	switch auth := r.Header.Get("Authorization"); {
	case auth == "":
		mxError(w, http.StatusUnauthorized, "M_MISSING_TOKEN")
		return
	case auth != "Bearer "+m.token:
		mxError(w, http.StatusUnauthorized, "M_UNKNOWN_TOKEN")
		return
	}
	if f.status != 0 {
		if f.status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "1")
		}
		if f.location != "" {
			w.Header().Set("Location", f.location)
		}
		mxError(w, f.status, f.errcode)
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	switch req.endpoint {
	case "joined_rooms":
		rooms := []string{}
		for id := range m.joined {
			rooms = append(rooms, id)
		}
		slices.Sort(rooms)
		mxJSON(w, map[string]any{"joined_rooms": rooms})
	case "directory":
		id, ok := m.aliases[req.alias]
		if !ok {
			mxError(w, http.StatusNotFound, "M_NOT_FOUND")
			return
		}
		mxJSON(w, map[string]any{"room_id": id, "servers": []string{"matrix.recap.test"}})
	case "sync":
		only := syncFilterRooms(req.query.Get("filter"))
		invite, join := map[string]any{}, map[string]any{}
		for id := range m.invited {
			if only == nil || slices.Contains(only, id) {
				invite[id] = map[string]any{"invite_state": map[string]any{"events": []any{}}}
			}
		}
		for id := range m.joined {
			if only == nil || slices.Contains(only, id) {
				join[id] = map[string]any{}
			}
		}
		mxJSON(w, map[string]any{"next_batch": "s1", "rooms": map[string]any{"invite": invite, "join": join}})
	case "join":
		if !m.invited[req.room] && !m.public[req.room] {
			mxError(w, http.StatusForbidden, "M_FORBIDDEN")
			return
		}
		delete(m.invited, req.room)
		m.joined[req.room] = true
		mxJSON(w, map[string]any{"room_id": req.room})
	case "send":
		if !m.joined[req.room] {
			mxError(w, http.StatusForbidden, "M_FORBIDDEN")
			return
		}
		// Transaction identifiers: same txnId and path → the original answer.
		for _, e := range m.events {
			if e.room == req.room && e.txn == req.txn {
				mxJSON(w, map[string]any{"event_id": e.eventID})
				return
			}
		}
		id := "$fixtureevent" + strconv.Itoa(len(m.events)+1)
		m.events = append(m.events, mxEvent{room: req.room, txn: req.txn, eventID: id, content: body})
		switch {
		case f.hangAfterPost:
			m.mu.Unlock()
			select {
			case <-r.Context().Done():
			case <-time.After(m.hold):
			}
			m.mu.Lock()
		case f.dropAfterPost:
			if hj, ok := w.(http.Hijacker); ok {
				if c, _, err := hj.Hijack(); err == nil {
					_ = c.Close()
				}
			}
		default:
			mxJSON(w, map[string]any{"event_id": id})
		}
	default:
		mxError(w, http.StatusNotFound, "M_UNRECOGNIZED")
	}
}

// syncFilter is the part of an inline sync filter the tests check
// (sinks.md §4c.2).
type syncFilter struct {
	Room struct {
		Rooms    []string `json:"rooms"`
		Timeline struct {
			Limit *int `json:"limit"`
		} `json:"timeline"`
	} `json:"room"`
}

func parseSyncFilter(filter string) (syncFilter, bool) {
	var f syncFilter
	if !strings.HasPrefix(filter, "{") || json.Unmarshal([]byte(filter), &f) != nil {
		return f, false
	}
	return f, true
}

// syncFilterRooms returns room.rooms of an inline sync filter, or nil.
func syncFilterRooms(filter string) []string {
	f, _ := parseSyncFilter(filter)
	return f.Room.Rooms
}

// matrixSetup starts a mock homeserver over TLS and a tool environment whose
// homeserver_url points at it.
func matrixSetup(t *testing.T) (*mockHomeserver, sinkEnv) {
	t.Helper()
	m, _, e := matrixSetupSrv(t)
	return m, e
}

func matrixSetupSrv(t *testing.T) (*mockHomeserver, *httptest.Server, sinkEnv) {
	t.Helper()
	m := newMockHomeserver(t)
	srv := httptest.NewUnstartedServer(m)
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return m, srv, newProfileEnv(t, "sinks-matrix.yaml", srv, "homeserver_url", srv.URL)
}

// withExtraKey returns a copy of e whose profile has one more line after its
// type line (sinks.md §4c.5: unknown keys are configuration errors).
func (e sinkEnv) withExtraKey(t *testing.T, line string) sinkEnv {
	t.Helper()
	c := e
	c.text = strings.Replace(e.text, "    type: matrix\n", "    type: matrix\n    "+line+"\n", 1)
	if c.text == e.text {
		t.Fatal("sinks file has no type: matrix line")
	}
	c.sinksFile = filepath.Join(t.TempDir(), "sinks.yaml")
	writeFile(t, c.sinksFile, []byte(c.text))
	return c
}

func matrixReq(t *testing.T, room string) hookRequest { return hookReq(t, matrixProfile, strp(room)) }

func txnID(req hookRequest) string { return "recap-" + req.JobID + "-" + req.SinkProfile }

// noticeBody is the event body of sinks.md §4c.2 for req.
func noticeBody(req hookRequest) string {
	lines := []string{req.Title}
	for _, l := range req.Links {
		lines = append(lines, l.URL)
	}
	return strings.Join(lines, "\n")
}

// expectEndpoints fails unless the mock got exactly these endpoints in order.
func expectEndpoints(t *testing.T, m *mockHomeserver, want ...string) []mxRequest {
	t.Helper()
	got := m.received()
	if !slices.Equal(endpoints(got), want) {
		t.Fatalf("homeserver requests %v, want %v (sinks.md §4c.2)", endpoints(got), want)
	}
	return got
}

// expectSend checks the send request of sinks.md §4c.2: room, transaction
// ID, bearer token, content exactly {msgtype: m.notice, body}.
func expectSend(t *testing.T, m *mockHomeserver, r mxRequest, room string, req hookRequest, body string) {
	t.Helper()
	if r.room != room || r.txn != txnID(req) {
		t.Fatalf("send to room %q with txnId %q, want %q and %q", r.room, r.txn, room, txnID(req))
	}
	if got := r.header.Values("Authorization"); len(got) != 1 || got[0] != "Bearer "+m.token {
		t.Fatal("Authorization header is not exactly \"Bearer <fixture token>\"")
	}
	if r.query.Get("access_token") != "" {
		t.Error("the token must not be in the query string")
	}
	var c map[string]json.RawMessage
	if err := json.Unmarshal(r.body, &c); err != nil {
		t.Fatalf("send body is not a JSON object: %v", err)
	}
	if len(c) != 2 || string(c["msgtype"]) != `"m.notice"` {
		t.Fatalf("event content has keys %v and msgtype %s; want exactly msgtype m.notice and body", keysOf(c), c["msgtype"])
	}
	var got string
	if err := json.Unmarshal(c["body"], &got); err != nil || got != body {
		t.Errorf("event body %q, want %q", got, body)
	}
	for _, v := range []string{req.Owner.Sub, req.Owner.Email} {
		if strings.Contains(string(r.body), v) {
			t.Errorf("event contains the owner value %q", v)
		}
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// expectSentEvent checks an ok result whose document_id is the event ID.
func expectSentEvent(t *testing.T, o output, req hookRequest, eventID string) {
	t.Helper()
	if o.Outcome != "ok" || o.Error != nil {
		t.Fatalf("got %s, want outcome ok without error", o.raw)
	}
	r := o.Delivery
	if r == nil || r.State != "sent" || r.JobID != req.JobID || r.SinkProfile != req.SinkProfile || str(r.DocumentID) != eventID {
		t.Fatalf("delivery record %s, want state sent with document_id %s (the event ID)", o.raw, eventID)
	}
}

func expectOneEvent(t *testing.T, m *mockHomeserver) mxEvent {
	t.Helper()
	ev := m.posted()
	if len(ev) != 1 {
		t.Fatalf("homeserver holds %d posted events, want 1", len(ev))
	}
	return ev[0]
}

// --- tests ---

func testMatrixJoined(t *testing.T) {
	m, e := matrixSetup(t)
	req := matrixReq(t, roomJoined)
	o := e.runHook(t, hookInput{Request: req})
	got := expectEndpoints(t, m, "joined_rooms", "send")
	expectSend(t, m, got[1], roomJoined, req, noticeBody(req))
	expectSentEvent(t, o, req, expectOneEvent(t, m).eventID)
}

func testMatrixAlias(t *testing.T) {
	t.Run("alias", func(t *testing.T) {
		m, e := matrixSetup(t)
		req := matrixReq(t, aliasTeam)
		o := e.runHook(t, hookInput{Request: req})
		got := expectEndpoints(t, m, "directory", "joined_rooms", "send")
		if got[0].alias != aliasTeam {
			t.Errorf("directory lookup of %q, want %q", got[0].alias, aliasTeam)
		}
		expectSend(t, m, got[2], roomJoined, req, noticeBody(req))
		expectSentEvent(t, o, req, expectOneEvent(t, m).eventID)
	})
	t.Run("room-id-without-server", func(t *testing.T) {
		m, e := matrixSetup(t)
		req := matrixReq(t, roomNoServer)
		o := e.runHook(t, hookInput{Request: req})
		got := expectEndpoints(t, m, "joined_rooms", "send")
		expectSend(t, m, got[1], roomNoServer, req, noticeBody(req))
		expectSentEvent(t, o, req, expectOneEvent(t, m).eventID)
	})
}

func testMatrixInvite(t *testing.T) {
	t.Run("invited", func(t *testing.T) {
		m, e := matrixSetup(t)
		req := matrixReq(t, roomInvited)
		o := e.runHook(t, hookInput{Request: req})
		got := expectEndpoints(t, m, "joined_rooms", "sync", "join", "send")
		f, ok := parseSyncFilter(got[1].query.Get("filter"))
		if !ok || !slices.Equal(f.Room.Rooms, []string{roomInvited}) {
			t.Errorf("sync filter room.rooms %v, want only the target room (sinks.md §4c.2)", f.Room.Rooms)
		}
		if f.Room.Timeline.Limit == nil || *f.Room.Timeline.Limit != 0 {
			t.Error("sync filter room.timeline.limit is not 0 (sinks.md §4c.2)")
		}
		if got[1].query.Get("timeout") != "0" {
			t.Errorf("sync timeout %q, want 0", got[1].query.Get("timeout"))
		}
		if got[2].room != roomInvited {
			t.Errorf("join of %q, want %q", got[2].room, roomInvited)
		}
		expectSend(t, m, got[3], roomInvited, req, noticeBody(req))
		expectSentEvent(t, o, req, expectOneEvent(t, m).eventID)
	})
	t.Run("public-not-invited", func(t *testing.T) {
		m, e := matrixSetup(t)
		req := matrixReq(t, roomPublic)
		o := e.runHook(t, hookInput{Request: req})
		expectEndpoints(t, m, "joined_rooms", "sync") // never join, never send
		expectClassRecord(t, o, "rejected")
	})
}

func testMatrixClasses(t *testing.T) {
	t.Run("joined-301-not-followed", func(t *testing.T) {
		m, srv, e := matrixSetupSrv(t)
		m.queue("joined_rooms", mxFault{status: 301, errcode: "M_UNKNOWN", location: srv.URL + "/redirected/_matrix/client/v3/joined_rooms"})
		expectClassRecord(t, e.runHook(t, hookInput{Request: matrixReq(t, roomJoined)}), "config")
		expectEndpoints(t, m, "joined_rooms") // the redirect target is never called
	})
	cases := []struct {
		name     string
		room     string
		endpoint string
		fault    mxFault
		class    string
		want     []string
	}{
		{"send-429", roomJoined, "send", mxFault{status: 429, errcode: "M_LIMIT_EXCEEDED"}, "not_sent", []string{"joined_rooms", "send"}},
		{"send-500", roomJoined, "send", mxFault{status: 500, errcode: "M_UNKNOWN"}, "uncertain", []string{"joined_rooms", "send"}},
		{"send-hang", roomJoined, "send", mxFault{hangAfterPost: true}, "uncertain", []string{"joined_rooms", "send"}},
		{"send-drop", roomJoined, "send", mxFault{dropAfterPost: true}, "uncertain", []string{"joined_rooms", "send"}},
		{"joined-401", roomJoined, "joined_rooms", mxFault{status: 401, errcode: "M_UNKNOWN_TOKEN"}, "config", []string{"joined_rooms"}},
		{"send-403", roomJoined, "send", mxFault{status: 403, errcode: "M_FORBIDDEN"}, "rejected", []string{"joined_rooms", "send"}},
		{"alias-unknown", aliasUnknown, "", mxFault{}, "rejected", []string{"directory"}},
		{"joined-500", roomJoined, "joined_rooms", mxFault{status: 500, errcode: "M_UNKNOWN"}, "not_sent", []string{"joined_rooms"}},
		{"sync-500", roomInvited, "sync", mxFault{status: 500, errcode: "M_UNKNOWN"}, "not_sent", []string{"joined_rooms", "sync"}},
		{"join-403", roomInvited, "join", mxFault{status: 403, errcode: "M_FORBIDDEN"}, "rejected", []string{"joined_rooms", "sync", "join"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, e := matrixSetup(t)
			if c.endpoint != "" {
				m.queue(c.endpoint, c.fault)
			}
			o := e.runHook(t, hookInput{Request: matrixReq(t, c.room), RequestTimeoutMS: ms(1000)})
			expectClassRecord(t, o, c.class)
			expectEndpoints(t, m, c.want...)
		})
	}
}

func testMatrixReentry(t *testing.T) {
	m, e := matrixSetup(t)
	req := matrixReq(t, roomJoined)
	o := e.runHook(t, hookInput{Request: req, Delivery: &record{JobID: req.JobID, SinkProfile: req.SinkProfile, State: "sending"}})
	expectClassRecord(t, o, "uncertain")
	expectEndpoints(t, m) // no request at all
	// Posted, answer lost; the admin retry deletes the record and calls again
	// with the same transaction ID, and the homeserver answers with the
	// original event (sinks.md §4c.4).
	m.queue("send", mxFault{dropAfterPost: true})
	expectClassRecord(t, e.runHook(t, hookInput{Request: req}), "uncertain")
	o = e.runHook(t, hookInput{Request: req})
	ev := expectOneEvent(t, m)
	expectSentEvent(t, o, req, ev.eventID)
	var txns []string
	for _, r := range m.received() {
		if r.endpoint == "send" {
			txns = append(txns, r.txn)
		}
	}
	if len(txns) != 2 || txns[0] != txnID(req) || txns[1] != txns[0] {
		t.Errorf("send transaction IDs %v, want %s twice", txns, txnID(req))
	}
}

func testMatrixPreChecks(t *testing.T) {
	m, e := matrixSetup(t)
	t.Run("rejected without a request", func(t *testing.T) {
		for name, room := range map[string]string{
			"room-without-sigil":   strings.TrimPrefix(roomJoined, "!"),
			"alias-without-server": "#fixture-team",
		} {
			t.Run(name, func(t *testing.T) {
				m.reset()
				expectClassRecord(t, e.runHook(t, hookInput{Request: matrixReq(t, room)}), "rejected")
				expectEndpoints(t, m)
			})
		}
	})
	t.Run("title only without links", func(t *testing.T) {
		m.reset()
		req := matrixReq(t, roomJoined)
		req.Links = []link{}
		o := e.runHook(t, hookInput{Request: req})
		got := expectEndpoints(t, m, "joined_rooms", "send")
		expectSend(t, m, got[1], roomJoined, req, req.Title)
		expectSentEvent(t, o, req, expectOneEvent(t, m).eventID)
	})
	t.Run("no summary in the notice", func(t *testing.T) {
		for _, ev := range m.posted() {
			if strings.Contains(string(ev.content), contentCanaries[0]) {
				t.Error("a Matrix event carries summary text (sinks.md §4c: notice only)")
			}
		}
	})
	t.Run("content key refused", func(t *testing.T) {
		code, stdout, _ := e.withExtraKey(t, "content: summary").runHookRaw(t, hookInput{Request: matrixReq(t, roomJoined)})
		if code != 2 || len(stdout) != 0 {
			t.Errorf("matrix profile with content: exit %d, stdout %q; want exit 2 (sinks.md §4c.5)", code, stdout)
		}
	})
	t.Run("declaration required", func(t *testing.T) {
		m.reset()
		expectDeclarationRequired(t, e, hookInput{Request: matrixReq(t, roomJoined)})
	})
	t.Run("no content in logs", func(t *testing.T) {
		req := matrixReq(t, roomJoined)
		canaries := append(append([]string{m.token, roomJoined, roomInvited, roomPublic, roomNoServer, aliasTeam, req.Title, resultLink.URL},
			contentCanaries...), identityCanaries...)
		scanHookLogs(t, canaries)
	})
}
