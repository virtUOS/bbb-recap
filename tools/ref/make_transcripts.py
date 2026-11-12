#!/usr/bin/env python3
"""Write the transcript goldens and cases under testdata/transcripts/ (docs/reference/providers.md §4.2, §4.3).

Independent of the code under test: it parses events.xml with the standard library and applies the rules of
providers.md §4.2-§4.3 directly. For each case it writes
  <case>/case.json        inputs of `recap tool normalize` and `recap tool assemble` (contracts/transcript/README.md)
  <case>/events.xml       the mutated events.xml, only for cases that change it
  <case>/transcript.json  the golden transcript
and invalid/*.json (F-01's golden with exactly one defect each). Then it checks every golden against
docs/reference/schemas/transcript.schema.json and the rules of §4.2, and every invalid file against its one defect.

Usage: python3 -B -P tools/ref/make_transcripts.py   (-P, not -I: jsonschema may live in the user site)
"""
import copy
import datetime
import hashlib
import json
import os
import re
import sys
import xml.etree.ElementTree as ET

import jsonschema

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
TESTDATA = os.path.join(ROOT, "testdata")
OUT = os.path.join(TESTDATA, "transcripts")
SCHEMA = os.path.join(ROOT, "docs", "reference", "schemas", "transcript.schema.json")

F01_RECORD_ID = "536c0e9585fba3ee1c984b5af671f5be7ec5cf96-1761732000000"
F01_RAW = "meetings/F-01/raw/" + F01_RECORD_ID  # relative to testdata/

# Injected job inputs: job_id (a UUIDv7 for 2025-10-29T10:10:00Z) and created_at (first claim time).
JOB_ID = "019a2f8c-3b40-7000-8000-000000000006"
CREATED_AT = "2025-10-29T10:10:00Z"
PROFILE, DIALECT, MODEL = "whisperx-lab", "whisperx-family", "large-v3"  # providers.md §4.1

A1 = "audio/microphone-w_fixada0001-TR_fixA1-1761732005200.webm"
A2 = "audio/microphone-w_fixada0001-TR_fixA2-1761732090200.webm"
B1 = "audio/microphone-w_fixben0002-TR_fixB1-1761732005300.webm"
B2 = "audio/microphone-w_fixben0002-TR_fixB2-1761732090300.webm"
A2_REJOIN = "audio/microphone-w_fixada0001_1-TR_fixA2-1761732090200.webm"

# Dialogue of F-01 (fixtures.md §7.1) on the meeting axis (fixtures.md §3.1 table "Axis ms").
TEXT = [
    "Welcome to the fixture meeting. Today we decide the colour of the test widget.",
    "I propose blue. Blue widgets passed every test last week.",
    "Agreed, blue it is. Ben, please update the widget specification by Friday.",
    "I will update the specification by Friday. Do we need a review?",
    "Yes, I will review it on Monday.",
]
# fixtures.md §3.1: segment, file, axis start, axis end, recorded start, recorded end
F01_TABLE = [
    ("seg-000001", A1, 1000, 5500, 1000, 5500),
    ("seg-000002", B1, 6000, 10000, 6000, 10000),
    ("seg-000003", A2, 86000, 91000, 56000, 61000),
    ("seg-000004", B2, 92000, 96500, 62000, 66500),
    ("seg-000005", A2, 97000, 99000, 67000, 69000),
]
# Track start on the axis = AudioTrackPublishedEvent timestamp - 5005000 (F-01 events.xml).
PUBLISH = {A1: 200, B1: 300, A2: 85200, B2: 85300}

UNREADABLE = b"SYNTHETIC fixture: not a WebM file, so normalize cannot decode it (track_unreadable).\n"
CLIP_KEPT = "Clipping check, this line ends after the pause started."
CLIP_DROPPED = "Clipping check, this line lies inside the pause."


def asr_result(path, segs, language="en"):
    """An AsrResult (providers.md §1.3) for one file; segs are (axis start, axis end, text)."""
    off = PUBLISH[path]
    return {
        "path": path,
        "profile": PROFILE,
        "dialect": DIALECT,
        "result": {
            "language": language,
            "model": MODEL,
            "segments": [{"start_ms": s - off, "end_ms": e - off, "text": t} for s, e, t in segs],
        },
    }


def f01_asr():
    by_file = {}
    for (_, path, s, e, _, _), text in zip(F01_TABLE, TEXT):
        by_file.setdefault(path, []).append((s, e, text))
    return by_file


# --- events.xml mutations (exact text edits on F-01's file) ---

EVENT_RE = re.compile(r'  (?:<!--[^\n]*-->\n  )?<event timestamp="(\d+)"[^>]*>.*?</event>\n', re.S)


def blocks(xml):
    return [(int(m.group(1)), m.group(0)) for m in EVENT_RE.finditer(xml)]


def replace_once(text, old, new):
    assert text.count(old) == 1, (old, text.count(old))
    return text.replace(old, new)


def mutate_rejoin(xml):
    """Ada leaves at 5070000 and rejoins as w_fixada0001_1 (BBB rejoin suffix) before period 2."""
    out = xml
    for ts, block in blocks(xml):
        if ts >= 5080000 and "<userId>w_fixada0001</userId>" in block:
            new = block.replace("<userId>w_fixada0001</userId>", "<userId>w_fixada0001_1</userId>")
            out = replace_once(out, block, new.replace("microphone-w_fixada0001-", "microphone-w_fixada0001_1-"))
    left = (
        '  <event timestamp="5070000" module="PARTICIPANT" eventname="ParticipantLeftEvent">\n'
        "    <timestampUTC>1761732070000</timestampUTC>\n"
        "    <userId>w_fixada0001</userId>\n"
        "    <date>2025-10-29T10:01:10.000Z</date>\n"
        "  </event>\n"
        '  <event timestamp="5080000" module="PARTICIPANT" eventname="ParticipantJoinEvent">\n'
        "    <timestampUTC>1761732080000</timestampUTC>\n"
        "    <userId>w_fixada0001_1</userId>\n"
        "    <externalUserId>u1</externalUserId>\n"
        "    <name>Ada Testperson</name>\n"
        "    <role>MODERATOR</role>\n"
        "    <date>2025-10-29T10:01:20.000Z</date>\n"
        "  </event>\n"
    )
    anchor = '  <event timestamp="5090000" '
    out = replace_once(out, anchor, left + anchor)
    assert out.count("microphone-w_fixada0001_1-TR_fixA2") == 2
    return out


def mutate_clip(xml):
    """B1 stays published into the pause (as with recordFullDurationMedia=true); the last stop mark is removed.

    Clipping always applies, whatever the package says about recordFullDurationMedia (providers.md §4.3 item 5).
    """
    by_ts = blocks(xml)
    (b1_unpub,) = [b for ts, b in by_ts if ts == 5060000 and "AudioTrackUnpublishedEvent" in b and "TR_fixB1" in b]
    (last_stop,) = [b for ts, b in by_ts if ts == 5150000 and "RecordStatusEvent" in b]
    (a2_pub,) = [b for ts, b in by_ts if ts == 5090200]
    out = replace_once(xml, b1_unpub, "")
    out = replace_once(out, last_stop, "")
    moved = b1_unpub.replace('timestamp="5060000"', 'timestamp="5090250"').replace(
        "<timestampUTC>1761732060000</timestampUTC>", "<timestampUTC>1761732090250</timestampUTC>"
    )
    return replace_once(out, a2_pub, a2_pub + moved)


# --- reference assembly (providers.md §4.2, §4.3) ---


def utc(ms):
    t = datetime.datetime.fromtimestamp(ms // 1000, tz=datetime.timezone.utc)
    return t.strftime("%Y-%m-%dT%H:%M:%S") + ".%03dZ" % (ms % 1000)


def child(ev, name):
    c = ev.find(name)
    return None if c is None else (c.text or "")


def assemble(xml, files, asr, unreadable):
    """files: package path -> bytes; asr: list of case asr entries; unreadable: paths normalize cannot decode."""
    root = ET.fromstring(xml)
    events = sorted(root.findall("event"), key=lambda e: int(e.get("timestamp")))  # stable: document order on ties
    name = lambda e: e.get("eventname")  # noqa: E731
    ts = lambda e: int(e.get("timestamp"))  # noqa: E731

    marks = [e for e in events if name(e) == "RecordStatusEvent"]
    origin_ev = next(e for e in marks if child(e, "status") == "true")  # §4.3.1
    origin = ts(origin_ev)
    last = max(ts(e) for e in events)

    periods, start = [], None  # §4.3.2
    for e in marks:
        if child(e, "status") == "true":
            if start is None:
                start = ts(e) - origin
        elif start is not None:
            periods.append([start, ts(e) - origin])
            start = None
    if start is not None:
        periods.append([start, last - origin])
    rec, offset = [], 0
    for s, e in periods:
        rec.append({"start_ms": s, "end_ms": e, "recorded_offset_ms": offset})
        offset += e - s

    joins = {}
    for e in events:
        if name(e) == "ParticipantJoinEvent":
            uid = re.sub(r"_\d*$", "", child(e, "userId"))
            joins.setdefault(uid, e)

    mic = {p for p in files if re.fullmatch(r"audio/microphone-[^/]*\.webm", p)}
    tracks, speakers, spk_of = [], [], {}
    for e in events:  # tracks numbered by publish order (first appearance)
        if name(e) != "AudioTrackPublishedEvent" or child(e, "source") != "microphone":
            continue
        path = "audio/" + os.path.basename(child(e, "filename"))
        if path not in mic:
            continue
        uid = re.sub(r"_\d*$", "", child(e, "userId"))  # §4.2 rejoin grouping
        if uid not in spk_of:
            spk_of[uid] = "spk-%d" % (len(spk_of) + 1)
            sp = {"ref": spk_of[uid]}
            j = joins.get(uid)
            if j is not None:
                sp["display_name"] = child(j, "name")
                sp["role"] = child(j, "role").lower()
            speakers.append(sp)
        unpub = [
            u
            for u in events
            if name(u) == "AudioTrackUnpublishedEvent" and os.path.basename(child(u, "filename")) == os.path.basename(path)
        ]
        assert unpub, "every case track has an unpublish event; decoded duration is not needed"
        tracks.append(
            {
                "path": path,
                "ref": "trk-%d" % (len(tracks) + 1),
                "speaker_ref": spk_of[uid],
                "kind": "microphone",
                "blob_sha256": hashlib.sha256(files[path]).hexdigest(),
                "start_ms": ts(e) - origin,
                "end_ms": ts(unpub[0]) - origin,
            }
        )
    assert len(tracks) == len(mic), "every microphone file of a case matches a publish event"

    by_path = {a["path"]: a for a in asr}
    segs, gaps, speech = [], [], {}
    for t in tracks:
        a = by_path.get(t["path"])
        if t["path"] in unreadable:
            assert a is None
            t["status"], reason = "failed", "track_unreadable"
        elif "gap" in a:
            t["status"], reason = "failed", a["gap"]
        else:
            r = a["result"]
            reason = None
            if r.get("language"):
                t["language"] = r["language"]
            t["provider"] = {"profile": a["profile"], "dialect": a["dialect"], "model": r["model"]}
            kept = 0
            for s in r["segments"]:
                start, end = t["start_ms"] + s["start_ms"], t["start_ms"] + s["end_ms"]  # §4.3.4
                mid = (start + end) / 2
                if not any(p["start_ms"] <= mid <= p["end_ms"] for p in rec):  # §4.3.5
                    continue
                kept += 1
                segs.append({"track_ref": t["ref"], "speaker_ref": t["speaker_ref"], "start_ms": start, "end_ms": end, "text": s["text"]})
                if r.get("language"):
                    speech[r["language"]] = speech.get(r["language"], 0) + end - start
            t["status"] = "transcribed" if kept else "empty"
        if reason:
            gaps.append({"track_ref": t["ref"], "speaker_ref": t["speaker_ref"], "start_ms": t["start_ms"], "end_ms": t["end_ms"], "reason": reason})

    segs.sort(key=lambda s: (s["start_ms"], refnum(s["track_ref"])))
    gaps.sort(key=lambda g: (g["start_ms"], refnum(g["track_ref"])))  # §4.2; speakers and tracks are built in ref order
    for i, s in enumerate(segs, 1):
        s["id"] = "seg-%06d" % i
    langs = {t["language"] for t in tracks if "language" in t}
    if not langs:
        language = None
    else:
        best = max(speech.values(), default=0)
        top = sorted(lang for lang in langs if speech.get(lang, 0) == best)
        assert len(top) == 1, "cases avoid language ties"
        language = top[0]

    track_keys = ["ref", "speaker_ref", "kind", "blob_sha256", "start_ms", "end_ms", "status", "language", "provider"]
    seg_keys = ["id", "track_ref", "speaker_ref", "start_ms", "end_ms", "text"]
    return {
        "schema_version": "1.0.0",
        "job_id": JOB_ID,
        "created_at": CREATED_AT,
        "source": "asr",
        "language": language,
        "time_base": {
            "clock": "bbb-event-timestamp",
            "origin": "first-recording-start",
            "origin_event_ts": origin,
            "origin_utc": utc(int(child(origin_ev, "timestampUTC"))),
        },
        "recording_periods": rec,
        "speakers": speakers,
        "tracks": [{k: t[k] for k in track_keys if k in t} for t in tracks],
        "segments": [{k: s[k] for k in seg_keys} for s in segs],
        "gaps": gaps,
    }


# --- checks (schema + §4.2 rules) ---


def refnum(ref):
    return int(ref.split("-")[1])


def sorted_by(items, key):
    keys = [key(x) for x in items]
    return keys == sorted(keys)


def rule_errors(tr):
    """The rules of providers.md §4.2 beyond the schema; returns a list of rule names that fail."""
    errs = []
    tr = dict(tr, gaps=tr.get("gaps", []))
    spk = {s["ref"] for s in tr["speakers"]}
    trk = {t["ref"]: t for t in tr["tracks"]}
    spans = list(tr["recording_periods"]) + tr["tracks"] + tr["segments"] + tr["gaps"]
    spans += [w for s in tr["segments"] for w in s.get("words", [])]
    if any(x["end_ms"] < x["start_ms"] for x in spans):
        errs.append("end-before-start")
    refs_ok = all(t["speaker_ref"] in spk for t in tr["tracks"]) and all(
        x["track_ref"] in trk and x["speaker_ref"] in spk for x in tr["segments"] + tr["gaps"]
    )
    if not refs_ok:
        errs.append("unresolved-ref")
    elif any(x["speaker_ref"] != trk[x["track_ref"]]["speaker_ref"] for x in tr["segments"] + tr["gaps"]):
        errs.append("speaker-mismatch")
    if refs_ok and any(trk[g["track_ref"]]["status"] != "failed" for g in tr["gaps"]):
        errs.append("gap-track-not-failed")
    ps = tr["recording_periods"]
    if any(ps[i]["start_ms"] < ps[i - 1]["end_ms"] for i in range(1, len(ps))):
        errs.append("periods-unsorted-or-overlapping")
    # Ordering and content rules of the §4.2 table.
    if not sorted_by(tr["speakers"], lambda x: refnum(x["ref"])):
        errs.append("speakers-unsorted")
    if {s["ref"] for s in tr["speakers"]} - {t["speaker_ref"] for t in tr["tracks"]}:
        errs.append("speaker-without-track")
    if not sorted_by(tr["tracks"], lambda x: refnum(x["ref"])):
        errs.append("tracks-unsorted")
    if any(t["status"] == "failed" and ("language" in t or "provider" in t) for t in tr["tracks"]):
        errs.append("failed-track-has-language-or-provider")
    if refs_ok and not sorted_by(tr["segments"], lambda x: (x["start_ms"], refnum(x["track_ref"]))):
        errs.append("segments-unsorted")
    if refs_ok and not sorted_by(tr["gaps"], lambda x: (x["start_ms"], refnum(x["track_ref"]))):
        errs.append("gaps-unsorted")
    # docs/reference/README.md "Units and time": timestamps are RFC 3339 UTC.
    if any(not (x.endswith("Z") or x.endswith("+00:00")) for x in (tr["created_at"], tr["time_base"]["origin_utc"])):
        errs.append("non-utc-time")
    return errs


def schema_errors(validator, tr):
    return [e.message for e in validator.iter_errors(tr)]


# One defect each; the name says which (contracts/transcript/README.md).
def invalid_cases(golden, gaps_golden):
    def edit(fn, base=golden):
        t = copy.deepcopy(base)
        fn(t)
        return t

    def swap01(key):
        return lambda t: t[key].insert(0, t[key].pop(1))

    def seg0(**kw):
        return lambda t: t["segments"][0].update(kw)

    return {
        "schema-float-time": edit(seg0(start_ms=1000.5)),
        "schema-negative-time": edit(seg0(start_ms=-1)),
        "schema-unknown-source": edit(lambda t: t.update(source="manual")),
        "schema-missing-gaps": edit(lambda t: t.pop("gaps")),
        "schema-extra-field": edit(lambda t: t.update(extra=True)),
        "rule-end-before-start": edit(seg0(end_ms=500)),
        "rule-unresolved-speaker": edit(seg0(speaker_ref="spk-9")),
        "rule-unresolved-track": edit(seg0(track_ref="trk-9")),
        "rule-speaker-mismatch": edit(seg0(speaker_ref="spk-2")),
        "rule-gap-track-not-failed": edit(
            lambda t: t["gaps"].append(
                {"track_ref": "trk-1", "speaker_ref": "spk-1", "start_ms": 200, "end_ms": 55000, "reason": "asr_failed"}
            )
        ),
        "rule-periods-overlap": edit(lambda t: t["recording_periods"][1].update(start_ms=50000)),
        "rule-periods-unsorted": edit(lambda t: t["recording_periods"].reverse()),
        "rule-speakers-unsorted": edit(swap01("speakers")),
        "rule-speaker-without-track": edit(
            lambda t: t["speakers"].append({"ref": "spk-3", "display_name": "Cy Testperson", "role": "viewer"})
        ),
        "rule-tracks-unsorted": edit(swap01("tracks")),
        "rule-segments-unsorted": edit(swap01("segments")),
        "rule-failed-track-language": edit(lambda t: t["tracks"][1].update(language="en"), gaps_golden),
        "rule-failed-track-provider": edit(
            lambda t: t["tracks"][1].update(provider={"profile": PROFILE, "dialect": DIALECT, "model": MODEL}),
            gaps_golden,
        ),
        "rule-gaps-unsorted": edit(swap01("gaps"), gaps_golden),
        "rule-non-utc-time": edit(lambda t: t.update(created_at="2025-10-29T12:10:00+02:00")),
    }


def dump(obj):
    return (json.dumps(obj, indent=2, ensure_ascii=False) + "\n").encode()


def write(path, data):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "wb") as f:
        f.write(data)


def main():
    raw = os.path.join(TESTDATA, F01_RAW)
    with open(os.path.join(raw, "events.xml"), encoding="utf-8") as f:
        f01_xml = f.read()
    files = {}
    for p in (A1, A2, B1, B2):
        with open(os.path.join(raw, p), "rb") as f:
            files[p] = f.read()
    asr = f01_asr()

    cases = {}
    # F-01 as it is: offsets, recorded_offset_ms, speakers.
    cases["F-01"] = dict(events=None, rename=[], replace=[], asr=[asr_result(p, asr[p]) for p in (A1, B1, A2, B2)])
    # Rejoin: Ada's second track carries userId w_fixada0001_1; still one speaker.
    cases["F-01-rejoin"] = dict(
        events=mutate_rejoin(f01_xml),
        rename=[{"from": A2, "to": A2_REJOIN}],
        replace=[],
        asr=[asr_result(p, asr[p]) for p in (A1, B1)]
        + [dict(asr_result(A2, asr[A2]), path=A2_REJOIN), asr_result(B2, asr[B2])],
    )
    # Gaps: A1 has a result without segments (empty, language de with 0 ms), B1 is rejected by the ASR,
    # B2 cannot be decoded by normalize; only A2 has text.
    cases["F-01-gaps"] = dict(
        events=None,
        rename=[],
        replace=[{"path": B2, "with": "unreadable.webm"}],
        asr=[asr_result(A1, [], language="de"), {"path": B1, "gap": "asr_rejected_input"}, asr_result(A2, asr[A2])],
    )
    # Clipping: B1 runs into the pause; one segment's midpoint is inside period 1, one inside the pause.
    # No stop mark after period 2, so it ends at the last event.
    cases["F-01-clip"] = dict(
        events=mutate_clip(f01_xml),
        rename=[],
        replace=[],
        asr=[
            asr_result(A1, asr[A1]),
            asr_result(B1, asr[B1] + [(53000, 56000, CLIP_KEPT), (60000, 64000, CLIP_DROPPED)]),
            asr_result(A2, asr[A2]),
            asr_result(B2, asr[B2]),
        ],
    )

    with open(SCHEMA, encoding="utf-8") as f:
        schema = json.load(f)
    validator = jsonschema.Draft202012Validator(schema, format_checker=jsonschema.FormatChecker())

    goldens = {}
    for name, c in cases.items():
        d = os.path.join(OUT, name)
        xml = c["events"] or f01_xml
        cfiles = dict(files)
        for r in c["rename"]:
            cfiles[r["to"]] = cfiles.pop(r["from"])
        unreadable = set()
        for r in c["replace"]:
            write(os.path.join(d, r["with"]), UNREADABLE)
            cfiles[r["path"]] = UNREADABLE
            unreadable.add(r["path"])
        tr = assemble(xml, cfiles, c["asr"], unreadable)
        goldens[name] = tr
        case = {
            "raw_dir": F01_RAW,
            "events": "events.xml" if c["events"] else None,
            "rename": c["rename"],
            "replace": c["replace"],
            "assemble": {"job_id": JOB_ID, "created_at": CREATED_AT, "asr": c["asr"]},
        }
        if c["events"]:
            write(os.path.join(d, "events.xml"), c["events"].encode())
        write(os.path.join(d, "case.json"), dump(case))
        write(os.path.join(d, "transcript.json"), dump(tr))

    # Self-checks against the reference text, independent of the arithmetic above.
    g = goldens["F-01"]
    assert g["recording_periods"] == [
        {"start_ms": 0, "end_ms": 55000, "recorded_offset_ms": 0},
        {"start_ms": 85000, "end_ms": 145000, "recorded_offset_ms": 55000},
    ]
    trk_file = {t["ref"]: p for t, p in zip(g["tracks"], (A1, B1, A2, B2))}
    for seg, (sid, path, s, e, rs, re_) in zip(g["segments"], F01_TABLE):
        assert (seg["id"], trk_file[seg["track_ref"]], seg["start_ms"], seg["end_ms"]) == (sid, path, s, e), seg
        p = next(p for p in g["recording_periods"] if p["start_ms"] <= s <= p["end_ms"])
        assert (s - p["start_ms"] + p["recorded_offset_ms"], e - p["start_ms"] + p["recorded_offset_ms"]) == (rs, re_)
    assert [s["text"] for s in g["segments"]] == TEXT
    rj = copy.deepcopy(goldens["F-01-rejoin"])
    assert rj == g, "the rejoin case yields the F-01 transcript"
    assert [x["reason"] for x in goldens["F-01-gaps"]["gaps"]] == ["asr_rejected_input", "track_unreadable"]
    assert [t["status"] for t in goldens["F-01-gaps"]["tracks"]] == ["empty", "failed", "transcribed", "failed"]
    assert goldens["F-01-gaps"]["language"] == "en"
    clip = goldens["F-01-clip"]
    assert CLIP_KEPT in [s["text"] for s in clip["segments"]] and CLIP_DROPPED not in [s["text"] for s in clip["segments"]]
    assert clip["recording_periods"][1]["end_ms"] == 150000

    for name, tr in goldens.items():
        errs = schema_errors(validator, tr) + rule_errors(tr)
        assert not errs, (name, errs)
    for name, tr in invalid_cases(g, goldens["F-01-gaps"]).items():
        s_errs, r_errs = schema_errors(validator, tr), rule_errors(tr)
        if name.startswith("schema-"):
            assert len(s_errs) == 1 and not r_errs, (name, s_errs, r_errs)
        else:
            assert not s_errs and len(r_errs) == 1, (name, s_errs, r_errs)
        write(os.path.join(OUT, "invalid", name + ".json"), dump(tr))
    print("transcripts: %d cases, %d invalid files; all checks passed" % (len(goldens), len(invalid_cases(g, goldens["F-01-gaps"]))))


if __name__ == "__main__":
    sys.exit(main())
