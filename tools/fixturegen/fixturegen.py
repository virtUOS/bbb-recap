#!/usr/bin/env python3
"""Generate the synthetic meeting fixtures under testdata/ (docs/reference/fixtures.md).

Writes testdata/meetings/<F-NN>/ (README, registration, raw/<record_id>/..., expected outcome files),
testdata/secrets/ and testdata/generator/manifest.txt. Package goldens (manifest.json, package_id.txt)
and testdata/manifests/invalid/ are written afterwards by the independent reference
scripts in tools/ref/ (see tools/fixturegen/generate.sh).

Everything is synthetic: speech from eSpeak NG formant voices, tones and black video from ffmpeg lavfi
sources, text written here. No recording of a person is used. Output is deterministic (fixed IDs, times
and bit-exact encoder flags), so running it twice gives byte-identical files.

Run: python3 -I tools/fixturegen/fixturegen.py   (or `make fixtures`)
"""
import datetime
import hashlib
import json
import os
import shlex
import shutil
import subprocess
import sys
import tempfile
from xml.sax.saxutils import escape, quoteattr

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
TESTDATA = os.path.join(ROOT, "testdata")
# Paths this script owns and recreates. testdata/ingest/exchanges and the other generators' dirs are untouched.
OWNED = ["meetings", "manifests/invalid", "secrets", "generator"]

EPOCH_MS = 1761732000000  # 2025-10-29T10:00:00Z, fixtures.md §2
MONO_BASE = 5_000_000  # events.xml `timestamp` (monotonic) base, fixtures.md §3.1
BBB_VERSION = "4.0.0"  # SYNTHETIC: not checked against a packaged BBB (see testdata/README.md)
RECORDER_DIR = "/var/lib/bbb-webrtc-recorder"  # bbb-webrtc-sfu lib/livekit/livekit-recorder-manager.js:400,419-441 @ c25fec6

ESPEAK_VERSION = "1.52.0"
FFMPEG_VERSION = "8.1.3"
# Licenses from the Fedora package metadata (`rpm -qi espeak-ng ffmpeg opus libvpx`) on the machine that
# produced the committed files.
LICENSES = {
    "espeak-ng": "eSpeak NG 1.52.0, Fedora package espeak-ng-1.52.0-3.fc44, license "
                 "GPL-3.0-only AND GPL-3.0-or-later AND Apache-2.0 AND BSD-2-Clause AND Unicode-DFS-2016 AND CC-BY-SA-3.0",
    "ffmpeg": "FFmpeg 8.1.3, Fedora package ffmpeg-8.1.3-1.fc44, license GPLv3+",
    "libopus": "libopus 1.6, Fedora package opus, license BSD-3-Clause AND BSD-2-Clause",
    "libvpx": "libvpx 1.15.0, Fedora package libvpx, license BSD-3-Clause",
    "python": "this script (tools/fixturegen/fixturegen.py), Python standard library only",
}

ADA = {"userId": "w_fixada0001", "externalUserId": "u1", "name": "Ada Testperson", "role": "MODERATOR",
       "voice": "en-us+f4"}  # no `breath`/`roughness` noise: variants with breath (f2, f3, f5) are not deterministic
BEN = {"userId": "w_fixben0002", "externalUserId": "u2", "name": "Ben Testperson", "role": "VIEWER",
       "voice": "en-us+m3"}

# Dialogue: the five cue texts of fixtures.md §7.1, in order. (speaker, espeak speed, text)
LINES = [
    (ADA, 180, "Welcome to the fixture meeting. Today we decide the colour of the test widget."),
    (BEN, 175, "I propose blue. Blue widgets passed every test last week."),
    (ADA, 185, "Agreed, blue it is. Ben, please update the widget specification by Friday."),
    (BEN, 175, "I will update the specification by Friday. Do we need a review?"),
    (ADA, 200, "Yes, I will review it on Monday."),
]

# Recap meta keys (metadata-interface.md §1.2) and the common origin key (fixtures.md §3).
OPTIN = {"bbb-origin": "PILOS", "recap-v": "1", "recap-enabled": "true",
         "recap-type": "meeting-summary", "recap-sink": "docs-main"}

OPUS_ARGS = ["-ar", "48000", "-ac", "1", "-c:a", "libopus", "-b:a", "24k",
             "-map_metadata", "-1", "-fflags", "+bitexact", "-flags:a", "+bitexact"]
FFMPEG = ["ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error", "-y"]

# --- fixture catalogue (fixtures.md §3) -------------------------------------------------------------
# kind: full (F-01), minimal (one recording period, F-01's period-1 audio), legacy (F-02), nomic (F-10),
#       ref (uses F-01's raw dir; nothing of its own under raw/).
# hook: decision of bbb-exporter.md §3.3. exporter: (state, reason) or None (no spool entry).
# service: (input, job) with input exporter|direct|none and job (state, reason) or None.
FIXTURES = {
    "F-01": dict(name="two-speakers-pause", kind="full", meta=OPTIN, registration=True,
                 purpose="happy path, all content types",
                 hook="spool", exporter=("acked", None), service=("exporter", ("delivered", None))),
    "F-02": dict(name="legacy-freeswitch", kind="legacy", meta=OPTIN, registration=False,
                 purpose="non-LiveKit audio bridge (ADR-001): one mixed FreeSWITCH recording",
                 hook="spool", exporter=("failed", "unsupported_audio_bridge"),
                 service=("direct", ("failed", "unsupported_audio_bridge"))),
    "F-03": dict(name="no-optin", kind="minimal", meta={"bbb-origin": "PILOS"}, registration=False,
                 purpose="no recap-* keys", hook="not_opted_in", exporter=None, service=("none", None)),
    "F-04": dict(name="invalid-type", kind="minimal", meta=dict(OPTIN, **{"recap-type": "no-such-type"}),
                 registration=True, purpose="recap-type=no-such-type",
                 hook="spool", exporter=("acked", None), service=("exporter", ("held", "unknown_type"))),
    "F-05": dict(name="optin-invalid-value", kind="minimal", meta=dict(OPTIN, **{"recap-enabled": "yes"}),
                 registration=False, purpose="recap-enabled=yes (only the literal true opts in)",
                 hook="not_opted_in", exporter=None, service=("none", None)),
    "F-06": dict(name="invalid-metadata", kind="minimal",
                 meta={k: v for k, v in OPTIN.items() if k != "recap-sink"}, registration=True,
                 purpose="recap-sink missing",
                 hook="spool", exporter=("acked", None), service=("exporter", ("held", "invalid_metadata"))),
    "F-07": dict(name="no-registration", kind="ref", meta=OPTIN, registration=False,
                 purpose="F-01 raw dir, no registration",
                 hook="spool", exporter=("acked", None), service=("exporter", ("held", "no_registration"))),
    "F-08": dict(name="breakout", kind="minimal", breakout=True, meta=OPTIN, registration=False,
                 purpose="breakout room inheriting the opt-in",
                 hook="breakout_skipped", exporter=None, service=("none", None)),
    "F-09": dict(name="oversize", kind="ref", meta=OPTIN, registration=False,
                 purpose="F-01 plus a sparse 3 GiB presentation/ file made at test time (never committed)",
                 hook="spool", exporter=("failed", "limit_exceeded"), service=("none", None)),
    "F-10": dict(name="livekit-no-mic", kind="nomic", meta=OPTIN, registration=False,
                 purpose="opted in, nobody published a microphone",
                 hook="spool", exporter=("failed", "no_microphone_tracks"), service=("none", None)),
    "F-11": dict(name="optin-false", kind="minimal", meta=dict(OPTIN, **{"recap-enabled": "false"}),
                 registration=False, purpose="recap-enabled=false (as F-05)",
                 hook="not_opted_in", exporter=None, service=("none", None)),
    "F-12": dict(name="no-marks", kind="nomarks", meta=OPTIN, registration=False,
                 purpose="opted in, record=true, but nobody pressed record (no RecordStatusEvent)",
                 hook="no_marks", exporter=None, service=("none", None)),
}


def record_id(fid):
    """fixtures.md §2: <sha1(fixture ID)>-1761732000000."""
    return hashlib.sha1(fid.encode()).hexdigest() + "-" + str(EPOCH_MS)


def meeting_id(fid):
    return "fixture-meeting-" + fid[2:]


def utc_date(offset_ms):
    """`date` child as BBB writes it: SimpleDateFormat yyyy-MM-dd'T'HH:mm:ss.SSSX (RecordEvent.scala)."""
    t = datetime.datetime.fromtimestamp((EPOCH_MS + offset_ms) / 1000, tz=datetime.timezone.utc)
    return t.strftime("%Y-%m-%dT%H:%M:%S.") + "%03dZ" % ((EPOCH_MS + offset_ms) % 1000)


def mic_name(user, track, publish_off):
    """LiveKit microphone file name, suffix = Date.now() at start (livekit-recorder-manager.js:264-285,400 @ c25fec6)."""
    return "microphone-%s-%s-%d.webm" % (user["userId"], track, EPOCH_MS + publish_off)


# --- events.xml -----------------------------------------------------------------------------------

SHAPE_SFU = "SYNTHETIC: shape not checked against a real recording (child order, no <date> for SFU events)"
SHAPE_POLL = "SYNTHETIC: shape not checked against a real recording (poll values; answers JSON omitted, fixtures.md §3.1)"


def event(offset, module, name, fields, date=True, note=None):
    """One <event>, rendered like events_archiver.rb:323-340 @ 7d27e7e. Returns (offset, seq, xml)."""
    lines = []
    if note:
        lines.append("  <!-- %s -->" % note)
    lines.append('  <event timestamp="%d" module="%s" eventname="%s">' % (MONO_BASE + offset, module, name))
    lines.append("    <timestampUTC>%d</timestampUTC>" % (EPOCH_MS + offset))
    for k, v in fields:
        lines.append("    <%s>%s</%s>" % (k, escape(str(v)), k))
    if date:
        lines.append("    <date>%s</date>" % utc_date(offset))
    lines.append("  </event>")
    return (offset, "\n".join(lines))


def joins(left_at):
    out = []
    for off, u in ((0, ADA), (1000, BEN)):
        out.append(event(off, "PARTICIPANT", "ParticipantJoinEvent",
                         [("userId", u["userId"]), ("externalUserId", u["externalUserId"]),
                          ("name", u["name"]), ("role", u["role"])]))
    for u in (ADA, BEN):
        out.append(event(left_at, "PARTICIPANT", "ParticipantLeftEvent", [("userId", u["userId"])]))
    return out


def record_status(offset, on):
    return event(offset, "PARTICIPANT", "RecordStatusEvent",
                 [("userId", ADA["userId"]), ("status", "true" if on else "false")])


def track_events(rid, user, track, source, publish_off, unpublish_off, filename):
    path = "%s/audio/%s/%s" % (RECORDER_DIR, rid, filename)
    out = []
    for off, name in ((publish_off, "AudioTrackPublishedEvent"), (unpublish_off, "AudioTrackUnpublishedEvent")):
        out.append(event(off, "bbb-webrtc-sfu", name,
                         [("userId", user["userId"]), ("filename", path), ("source", source)],
                         date=False, note=SHAPE_SFU))
    return out


def webrtc_share(rid, kind, user, track, start, stop):
    """Camera / screen share events (livekit-recorder-manager.js:917-941 @ c25fec6). kind: camera or screen_share."""
    if kind == "camera":
        names, sub = ("StartWebRTCShareEvent", "StopWebRTCShareEvent"), "recordings"
    else:
        names, sub = ("StartWebRTCDesktopShareEvent", "StopWebRTCDesktopShareEvent"), "screenshare"
    path = "%s/%s/%s/%s-%s-%s-%d.webm" % (RECORDER_DIR, sub, rid, kind, user["userId"], track, EPOCH_MS + start)
    return [event(off, "bbb-webrtc-sfu", n, [("filename", path), ("userId", user["userId"])],
                  date=False, note=SHAPE_SFU) for off, n in ((start, names[0]), (stop, names[1]))]


def events_xml(fid, rid, mid, meta, evs, breakout=False):
    md = {"isBreakout": "true" if breakout else "false", "meetingId": mid, "meetingName": "Fixture Widget Meeting"}
    md.update(meta)
    attrs = " ".join("%s=%s" % (k, quoteattr(v)) for k, v in sorted(md.items()))
    out = ['<?xml version="1.0" encoding="UTF-8"?>',
           "<!-- SYNTHETIC fixture %s (docs/reference/fixtures.md). Not a real meeting; all names are fictional. -->" % fid,
           '<recording meeting_id="%s" bbb_version="%s">' % (rid, BBB_VERSION),
           '  <meeting id="%s" externalId=%s name="Fixture Widget Meeting" breakout="%s"/>'
           % (rid, quoteattr(mid), "true" if breakout else "false"),
           "  <metadata %s/>" % attrs]
    if breakout:
        out.append("  <!-- SYNTHETIC: shape not checked against a real recording (breakout encoding; attributes unknown) -->")
        out.append("  <breakout/>")
    # stable sort by offset keeps the given order for equal timestamps
    out += [x for _, x in sorted(evs, key=lambda e: e[0])]
    out.append("</recording>")
    return "\n".join(out) + "\n"


# --- small synthetic documents --------------------------------------------------------------------

def minimal_pdf(pages):
    """A tiny valid PDF (one line of Helvetica text per page). Deterministic: no dates, no IDs."""
    objs = ["<< /Type /Catalog /Pages 2 0 R >>",
            None,  # pages, filled below
            "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"]
    kids = []
    for text in pages:
        stream = "BT /F1 24 Tf 72 720 Td (%s) Tj ET" % text.replace("\\", "\\\\").replace("(", "\\(").replace(")", "\\)")
        objs.append("<< /Length %d >>\nstream\n%s\nendstream" % (len(stream), stream))
        objs.append("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 3 0 R >> >> "
                    "/Contents %d 0 R >>" % (len(objs)))
        kids.append("%d 0 R" % len(objs))
    objs[1] = "<< /Type /Pages /Kids [%s] /Count %d >>" % (" ".join(kids), len(pages))
    out = b"%PDF-1.4\n"
    offsets = []
    for i, o in enumerate(objs, 1):
        offsets.append(len(out))
        out += ("%d 0 obj\n%s\nendobj\n" % (i, o)).encode("ascii")
    xref = len(out)
    out += ("xref\n0 %d\n0000000000 65535 f \n" % (len(objs) + 1)).encode("ascii")
    for off in offsets:
        out += ("%010d 00000 n \n" % off).encode("ascii")
    out += ("trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n" % (len(objs) + 1, xref)).encode("ascii")
    return out


def slide_svg(n, text):
    return ('<svg xmlns="http://www.w3.org/2000/svg" width="1600" height="900" viewBox="0 0 1600 900">\n'
            '  <!-- SYNTHETIC slide %d; presentation/ layout not checked against a real recording -->\n'
            '  <rect width="1600" height="900" fill="#ffffff"/>\n'
            '  <text x="100" y="200" font-family="sans-serif" font-size="72">%s</text>\n'
            '</svg>\n') % (n, escape(text))


# notes.html stays 350 bytes: contracts/package/package_test.go replaces the `"size":350` entry of F-01.
NOTES_HTML = """<!DOCTYPE html>
<!-- SYNTHETIC notes export; BlockNote HTML shape not checked against a real export. -->
<html><head><meta charset="utf-8"><title>Shared notes</title></head>
<body>
<h1>Widget colour</h1>
<p>Decision: the test widget is blue.</p>
<p>Action: Ben updates the widget specification by Friday. Ada reviews it on Monday.</p>
</body></html>
"""

LEARNING_DASHBOARD = {"_synthetic": "SYNTHETIC placeholder; learning-dashboard schema not checked against a real recording"}


# --- generator ------------------------------------------------------------------------------------

class Gen:
    def __init__(self, tmp):
        self.tmp = tmp
        self.log = []  # manifest.txt blocks
        self.wavs = {}
        self.tools = {}  # output path -> tools used, for copies

    def show(self, cmd):
        return shlex.join(cmd).replace(self.tmp, "$TMP")

    def run(self, cmd):
        subprocess.run(cmd, cwd=ROOT, check=True)

    def line_wav(self, i):
        """Synthesize dialogue line i (0-based) once; intermediate WAV in $TMP."""
        if i not in self.wavs:
            user, speed, text = LINES[i]
            wav = os.path.join(self.tmp, "line-%d.wav" % (i + 1))
            cmd = ["espeak-ng", "-v", user["voice"], "-s", str(speed), "-w", wav, text]
            self.run(cmd)
            self.log.append("[intermediate] $TMP/line-%d.wav  (deleted after the run)\n"
                            "  content: synthetic speech, dialogue line %d (%s, eSpeak NG voice %s)\n"
                            "  tool:    %s\n  cmd:     %s\n"
                            % (i + 1, i + 1, user["name"], user["voice"], LICENSES["espeak-ng"], self.show(cmd)))
            self.wavs[i] = wav
        return self.wavs[i]

    def speech_track(self, out, duration_s, placements, fmt="webm"):
        """Encode speech lines placed at given ms offsets into silence of duration_s. placements: [(line, ms)]."""
        cmd = list(FFMPEG)
        for line, _ in placements:
            cmd += ["-i", self.line_wav(line)]
        parts = ["[%d:a]adelay=%d:all=1[a%d]" % (k, ms, k) for k, (_, ms) in enumerate(placements)]
        if len(placements) > 1:
            parts.append("".join("[a%d]" % k for k in range(len(placements)))
                         + "amix=inputs=%d:normalize=0:duration=longest[m]" % len(placements))
            last = "m"
        else:
            last = "a0"
        parts.append("[%s]apad=whole_dur=%s[out]" % (last, duration_s))
        cmd += ["-filter_complex", ";".join(parts), "-map", "[out]"] + OPUS_ARGS + ["-f", fmt, out]
        self.run(cmd)
        what = ", ".join("line %d at %d ms" % (line + 1, ms) for line, ms in placements)
        self.record(out, "synthetic speech (%s); silence elsewhere; %s s" % (what, duration_s),
                    ["ffmpeg", "libopus", "espeak-ng"], cmd)

    def tone(self, out, seconds):
        cmd = FFMPEG + ["-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=%d" % seconds] \
            + OPUS_ARGS + ["-f", "webm", out]
        self.run(cmd)
        self.record(out, "%d s 440 Hz sine tone" % seconds, ["ffmpeg", "libopus"], cmd)

    def black_video(self, out, seconds):
        cmd = FFMPEG + ["-f", "lavfi", "-i", "color=c=black:s=320x240:r=15:d=%d" % seconds,
                        "-c:v", "libvpx", "-b:v", "50k", "-threads", "1", "-map_metadata", "-1",
                        "-fflags", "+bitexact", "-flags:v", "+bitexact", "-f", "webm", out]
        self.run(cmd)
        self.record(out, "%d s black VP8 video, no audio" % seconds, ["ffmpeg", "libvpx"], cmd)

    def copy(self, src, dst, what):
        shutil.copyfile(os.path.join(ROOT, src), os.path.join(ROOT, dst))
        self.log.append("file: %s\n  content: byte-identical copy of %s (%s); made with tools below.\n%s"
                        "  cmd:     cp %s %s   (shutil.copyfile in fixturegen.py)\n"
                        % (dst, src, what, "".join("  tool:    %s\n" % LICENSES[t] for t in self.tools[src]),
                           shlex.quote(src), shlex.quote(dst)))
        self.tools[dst] = self.tools[src]

    def record(self, out, content, tools, cmd):
        self.tools[out] = tools
        self.log.append("file: %s\n  content: %s. Not a recording of a person.\n%s  cmd:     %s\n"
                        % (out, content, "".join("  tool:    %s\n" % LICENSES[t] for t in tools), self.show(cmd)))


def write(path, data):
    path = os.path.join(ROOT, path)
    os.makedirs(os.path.dirname(path), exist_ok=True)
    mode = "wb" if isinstance(data, bytes) else "w"
    with open(path, mode, **({} if isinstance(data, bytes) else {"encoding": "utf-8", "newline": "\n"})) as f:
        f.write(data)
    os.chmod(path, 0o644)


def write_json(path, obj):
    write(path, json.dumps(obj, indent=2, ensure_ascii=False) + "\n")


def mkdirs(path):
    os.makedirs(os.path.join(ROOT, path), exist_ok=True)


# Recording periods of F-01 (fixtures.md §3.1): microphone tracks with publish/unpublish offsets and the
# dialogue lines placed in each file. Offset in file = event offset - publish offset.
F01_TRACKS = [
    # user, track, publish, unpublish, [(line, ms into file)]
    (ADA, "TR_fixA1", 5200, 60000, [(0, 6000 - 5200)]),
    (BEN, "TR_fixB1", 5300, 60000, [(1, 11000 - 5300)]),
    (ADA, "TR_fixA2", 90200, 150000, [(2, 91000 - 90200), (4, 102000 - 90200)]),
    (BEN, "TR_fixB2", 90300, 150000, [(3, 97000 - 90300)]),
]


def gen_full(g, fid, rid, mid, meta, base):
    raw = base + "/raw/" + rid
    evs = joins(155000)
    evs.append(event(200, "PAD", "PadCreatedEvent", [("padId", "fixturepad0001")]))
    pres = hashlib.sha1(b"F-01-deck").hexdigest() + "-%d" % (EPOCH_MS + 1000)
    pod = "DEFAULT_PRESENTATION_POD"
    evs.append(event(2000, "PRESENTATION", "SharePresentationEvent",
                     [("presentationName", pres), ("podId", pod), ("share", "true")]))
    evs.append(event(2001, "PRESENTATION", "GotoSlideEvent",
                     [("presentationName", pres), ("podId", pod), ("id", pres + "/1"), ("slide", "0")]))
    evs.append(event(40000, "PRESENTATION", "GotoSlideEvent",
                     [("presentationName", pres), ("podId", pod), ("id", pres + "/2"), ("slide", "1")]))
    evs += [record_status(5000, True), record_status(60000, False),
            record_status(90000, True), record_status(150000, False)]
    for user, track, pub, unpub, placements in F01_TRACKS:
        name = mic_name(user, track, pub)
        evs += track_events(rid, user, track, "microphone", pub, unpub, name)
        mkdirs(raw + "/audio")
        g.speech_track(raw + "/audio/" + name, "%.1f" % ((unpub - pub) / 1000), placements)
    # chat (fixtures.md §3.1)
    for off, user, mid_, text in ((12000, BEN, "fixmsg-0001", "Link to the widget spec will follow."),
                                  (14000, ADA, "fixmsg-0002", "Thanks.")):
        evs.append(event(off, "CHAT", "PublicChatEvent",
                         [("senderId", user["userId"]), ("senderRole", user["role"]), ("messageId", mid_),
                          ("message", text), ("chatEmphasizedText", "false")]))
    # poll "Blue or green?", both answer blue, stopped, published
    poll = "fixpoll-0001"
    evs.append(event(20000, "POLL", "PollStartedRecordEvent",
                     [("pollId", poll), ("userId", ADA["userId"]), ("question", "Blue or green?"),
                      ("secretPoll", "false")], note=SHAPE_POLL))
    for off, user in ((25000, ADA), (26000, BEN)):
        evs.append(event(off, "POLL", "UserRespondedToPollRecordEvent",
                         [("pollId", poll), ("userId", user["userId"]), ("answerId", "0"), ("answer", "Blue")],
                         note=SHAPE_POLL))
    evs.append(event(30000, "POLL", "PollStoppedRecordEvent", [("pollId", poll)], note=SHAPE_POLL))
    evs.append(event(31000, "POLL", "PollPublishedRecordEvent",
                     [("pollId", poll), ("question", "Blue or green?"), ("numRespondents", "2"),
                      ("numResponders", "2"),
                      # Boolean.toString (PollPublishedRecordEvent.scala:38-43 @ 7d27e7e)
                      ("isQuiz", "false"), ("showCorrectAnswer", "false")], note=SHAPE_POLL))
    # excluded content: screen-share audio (5 s tone), camera and desk share (2 s black VP8)
    ss_name = "screen_share_audio-%s-TR_fixS1-%d.webm" % (BEN["userId"], EPOCH_MS + 45000)
    evs += track_events(rid, BEN, "TR_fixS1", "screen_share_audio", 45000, 50000, ss_name)
    g.tone(raw + "/audio/" + ss_name, 5)
    evs += webrtc_share(rid, "screen_share", BEN, "TR_fixD1", 45000, 47000)
    mkdirs(raw + "/deskshare")
    g.black_video(raw + "/deskshare/screen_share-%s-TR_fixD1-%d.webm" % (BEN["userId"], EPOCH_MS + 45000), 2)
    evs += webrtc_share(rid, "camera", ADA, "TR_fixV1", 50000, 52000)
    mkdirs(raw + "/video/" + rid)
    g.black_video(raw + "/video/%s/camera-%s-TR_fixV1-%d.webm" % (rid, ADA["userId"], EPOCH_MS + 50000), 2)
    os.symlink("/var/lib/bbb-webrtc-recorder/elsewhere.webm",
               os.path.join(ROOT, raw, "audio/link-to-elsewhere.webm"))
    write(raw + "/notes/notes.html", NOTES_HTML)
    write(raw + "/notes/notes.pdf", minimal_pdf(["SYNTHETIC notes: the test widget is blue."]))
    write(raw + "/presentation/%s/%s.pdf" % (pres, pres),
          minimal_pdf(["SYNTHETIC deck, page 1: Widget colour", "SYNTHETIC deck, page 2: Blue or green?"]))
    write(raw + "/presentation/%s/svgs/slide1.svg" % pres, slide_svg(1, "Widget colour"))
    write(raw + "/presentation/%s/svgs/slide2.svg" % pres, slide_svg(2, "Blue or green?"))
    write_json(raw + "/learning-dashboard/data.json", LEARNING_DASHBOARD)
    write(raw + "/events.xml", events_xml(fid, rid, mid, meta, evs))


def gen_minimal(g, fid, rid, mid, meta, base, breakout=False):
    """One recording period with F-01's period-1 microphone files (byte-identical copies)."""
    raw = base + "/raw/" + rid
    f01 = "testdata/meetings/F-01/raw/%s/audio/" % record_id("F-01")
    evs = joins(65000) + [record_status(5000, True), record_status(60000, False)]
    mkdirs(raw + "/audio")
    for user, track, pub, unpub, _ in F01_TRACKS[:2]:
        name = mic_name(user, track, pub)
        evs += track_events(rid, user, track, "microphone", pub, unpub, name)
        g.copy(f01 + name, raw + "/audio/" + name, "F-01 period-1 microphone track of %s" % user["name"])
    write(raw + "/events.xml", events_xml(fid, rid, mid, meta, evs, breakout=breakout))


def gen_legacy(g, fid, rid, mid, meta, base):
    """F-02: FreeSWITCH bridge, VOICE Start/StopRecordingEvent, one mixed Ogg/Opus file (docs/reference/upstream-facts.md)."""
    raw = base + "/raw/" + rid
    name = "%s-%d.opus" % (rid, EPOCH_MS + 5000)
    evs = joins(65000) + [record_status(5000, True), record_status(60000, False)]
    for off, n in ((5000, "StartRecordingEvent"), (60000, "StopRecordingEvent")):
        # archive.rb:39-44 @ 7d27e7e rewrites <filename> to the .opus basename
        evs.append(event(off, "VOICE", n, [("bridge", "70001"), ("recordingTimestamp", str(EPOCH_MS + 5000)),
                                           ("filename", name)],
                         note="SYNTHETIC: values of bridge/recordingTimestamp not checked against a real recording"))
    mkdirs(raw + "/audio")
    g.speech_track(raw + "/audio/" + name, "55.0", [(0, 1000), (1, 6000)], fmt="ogg")
    write(raw + "/events.xml", events_xml(fid, rid, mid, meta, evs))


def gen_nomic(g, fid, rid, mid, meta, base):
    """F-10: opted in and marked, but no publish events and no audio files."""
    raw = base + "/raw/" + rid
    evs = joins(65000) + [record_status(5000, True), record_status(60000, False)]
    write(raw + "/events.xml", events_xml(fid, rid, mid, meta, evs))


def gen_nomarks(g, fid, rid, mid, meta, base):
    """F-12: opted in, no recording marks, so no recorded media either (bbb-exporter.md §3.3)."""
    raw = base + "/raw/" + rid
    write(raw + "/events.xml", events_xml(fid, rid, mid, meta, joins(65000)))


def outcome_files(fid, fx, base):
    hook = {"decision": fx["hook"], "exit_code": 0}
    raw_fid = "F-01" if fx["kind"] == "ref" else fid
    hook["spool_entry"] = {"spool_version": 1, "record_id": record_id(raw_fid)} if fx["hook"] == "spool" else None
    write_json(base + "/expected/hook.json", hook)
    ex = fx["exporter"]
    write_json(base + "/expected/exporter.json",
               {"spooled": ex is not None, "state": ex[0] if ex else None, "reason": ex[1] if ex else None})
    inp, job = fx["service"]
    write_json(base + "/expected/service.json",
               {"input": inp, "job": {"state": job[0], "reason": job[1]} if job else None})


def readme(fid, fx, rid):
    ex = fx["exporter"]
    inp, job = fx["service"]
    lines = ["# %s `%s`" % (fid, fx["name"]), "",
             "SYNTHETIC fixture (docs/reference/fixtures.md §3). Generated by `tools/fixturegen/fixturegen.py`; do not edit.", "",
             "- Purpose: %s" % fx["purpose"],
             "- record_id: `%s`" % rid,
             "- meeting_id: `%s`" % meeting_id("F-01" if fx["kind"] == "ref" else fid),
             "- metadata: %s" % ", ".join("`%s=%s`" % kv for kv in sorted(fx["meta"].items())),
             "- registration: %s" % ("`registration.json`" if fx["registration"] else "none"),
             "- hook: `%s`" % fx["hook"],
             "- exporter: %s" % ("no spool entry" if ex is None else "`%s`" % "/".join(x for x in ex if x)),
             "- service: %s" % ("none" if job is None else "`%s` (input: %s)" % ("/".join(x for x in job if x), inp)),
             ""]
    if fx["kind"] == "ref":
        lines.append("Raw directory: none of its own; uses `../F-01/raw/%s/` by reference." % rid)
    if fid == "F-09":
        lines.append("The 3 GiB sparse file is created at test time below `presentation/` of a copy of F-01's raw"
                     " dir; it is never committed.")
    if fid == "F-10":
        lines.append("No `audio/` directory: git cannot store an empty directory, and an absent `audio/` has no"
                     " other files, so the exporter reason stays `no_microphone_tracks` (meeting-package.md §6).")
    if fid == "F-12":
        lines.append("No `RecordStatusEvent` and no `audio/`: without marks LiveKit records no microphone files by"
                     " default (bbb-webrtc-sfu lib/livekit/livekit-recorder-manager.js:30-32,195-246 @ c25fec6). In real BBB, archive halts without marks and the hook would not run"
                     " (bbb-exporter.md §2); the hook's `no_marks` check is defence in depth, logged at WARN.")
    if fid == "F-01":
        lines.append("Excluded from the package on purpose: `audio/screen_share_audio-*` (5 s tone), `notes/notes.pdf`,"
                     " `video/`, `deskshare/` (2 s black VP8), `learning-dashboard/data.json`, and the dangling symlink"
                     " `audio/link-to-elsewhere.webm` (skipped and counted).")
    lines.append("Parts of `events.xml` marked `SYNTHETIC: shape` were not checked against a real recording"
                 " (docs/reference/fixtures.md §8).")
    return "\n".join(lines) + "\n"


def secrets():
    def hex32(label):
        return hashlib.sha256(("recap-fixture:" + label).encode()).hexdigest()[:32]
    files = {
        "clients/bbb_server.bbb-fixture-a": "bbb-a",
        "clients/bbb_server.bbb-fixture-b": "bbb-b",
        "clients/frontend.pilos-fixture": "fe",
        "clients/frontend.other-fixture": "fe-other",
        "clients/admin.fixture-admin": "admin",
        "asr-fixture.key": "asr",
        "llm-fixture.key": "llm",
        "docs-s2s.key": "docs-s2s",
    }
    for path, label in files.items():
        write("testdata/secrets/" + path, "FIXTURE-NOT-A-SECRET-%s-%s\n" % (label, hex32(label)))


def check_versions():
    esp = subprocess.run(["espeak-ng", "--version"], capture_output=True, text=True, check=True).stdout
    ff = subprocess.run(["ffmpeg", "-version"], capture_output=True, text=True, check=True).stdout.split("\n")[0]
    if ESPEAK_VERSION not in esp or ("ffmpeg version %s " % FFMPEG_VERSION) not in ff:
        print("fixturegen: WARNING: tool versions differ from the committed fixtures (espeak-ng %s, ffmpeg %s); "
              "regenerated audio may differ byte for byte" % (ESPEAK_VERSION, FFMPEG_VERSION), file=sys.stderr)


def main():
    check_versions()
    for d in OWNED:
        shutil.rmtree(os.path.join(TESTDATA, d), ignore_errors=True)
    tmp = tempfile.mkdtemp(prefix="recap-fixturegen-")
    try:
        g = Gen(tmp)
        for fid, fx in FIXTURES.items():  # F-01 first: minimal fixtures copy its audio
            base = "testdata/meetings/" + fid
            rid = record_id("F-01" if fx["kind"] == "ref" else fid)
            mid = meeting_id("F-01" if fx["kind"] == "ref" else fid)
            mkdirs(base + "/expected")
            if fx["kind"] == "full":
                gen_full(g, fid, rid, mid, fx["meta"], base)
            elif fx["kind"] == "minimal":
                gen_minimal(g, fid, rid, mid, fx["meta"], base, breakout=fx.get("breakout", False))
            elif fx["kind"] == "legacy":
                gen_legacy(g, fid, rid, mid, fx["meta"], base)
            elif fx["kind"] == "nomic":
                gen_nomic(g, fid, rid, mid, fx["meta"], base)
            elif fx["kind"] == "nomarks":
                gen_nomarks(g, fid, rid, mid, fx["meta"], base)
            if fx["registration"]:
                write_json(base + "/registration.json", {
                    "schema_version": "1.0.0", "meeting_id": mid, "record_id": rid,
                    "owner": {"sub": "fixture-sub-ada-0001", "email": "ada.testperson@example.org"}})
            outcome_files(fid, fx, base)
            write(base + "/README.md", readme(fid, fx, rid))
        secrets()
        header = ("# Generated by tools/fixturegen/fixturegen.py (run `make fixtures`); do not edit.\n"
                  "# Every audio and video file under testdata/ with the tool, version, license and command that\n"
                  "# produced it. All speech is eSpeak NG formant synthesis; no file is a recording of a person.\n"
                  "# Commands run from the repository root; $TMP is a temporary directory removed after the run.\n\n")
        write("testdata/generator/manifest.txt", header + "\n".join(g.log))
    finally:
        shutil.rmtree(tmp, ignore_errors=True)


if __name__ == "__main__":
    main()
