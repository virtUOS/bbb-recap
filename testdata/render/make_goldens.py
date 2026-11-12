#!/usr/bin/env python3
"""Reference renderer for the render goldens (docs/reference/templates.md §5, §6).

Independent of the code under test: written from the spec text only, standard library plus `jsonschema`
(used only to check the inputs and the LLM reply fixtures). Run from the repository root:

    python3 -B -P testdata/render/make_goldens.py

It writes, deterministically:
  - testdata/render/{F-01,gap}/summary.md, F-01/summary.de.md  (templates.md §5)
  - testdata/render/{F-01,gap,mapping}/transcript.vtt          (templates.md §6)
  - testdata/render/llm/{summary.v1,chunk-notes.v1}/*.json      (templates.md §4, CT-TPL-01)
and checks that F-01/transcript.vtt equals the hand-written golden in docs/reference/fixtures.md §7.1, that every
transcript input validates against docs/reference/schemas/transcript.schema.json, and that every LLM reply fixture is
valid or invalid exactly as its file name says, against reference schemas written from templates.md §4.

Layout details follow templates.md §5.4 "Fixed details"; NOTE order follows §6 (pause NOTE before gap NOTE).
"""

import copy
import datetime
import json
import os
import re
import sys
import unicodedata
import zoneinfo

import jsonschema

ROOT = os.getcwd()
RENDER = os.path.join(ROOT, "testdata", "render")

FORMAT_ID = "meeting-summary"
FORMAT_VERSION = "1.0.0"

# String tables (templates.md §5.4 "Fixed details", §6, §7). The de labels are listed in §5.4; this table is the
# reference the spec points to.
STRINGS = {
    "en": {
        "recorded": "recorded",  # header "· recorded: {recorded_duration}" (spec)
        "notice": "*Automatically generated from the recorded parts of this meeting. It may contain errors.*",
        "gap": "> **Incomplete transcript:** no text for {speaker} from [{start}] to [{end}] (transcription failed).",
        "overview": "Overview",
        "topics": "Topics",
        "decisions": "Decisions",
        "action_items": "Action items",
        "open_questions": "Open questions",
        "polls": "Polls",
        "none": "None recorded.",
        "due": "due",  # {due_label}
        "option": "Option",  # {option_label}
        "votes": "Votes",  # {votes_label}
        "speaker": "Speaker {n}",  # n from spk-<n>
        "vtt_header": "NOTE Automatic transcript (speech recognition). Times are recorded time; pauses are removed.",
        "vtt_pause": "NOTE Recording paused here.",
        "vtt_gap": "NOTE Gap {start} to {end}, {name}: no transcript ({reason}).",
    },
    "de": {
        "recorded": "aufgezeichnet",
        "notice": "*Automatisch erstellt aus den aufgezeichneten Teilen dieser Besprechung. Kann Fehler enthalten.*",
        "gap": "> **Unvollständiges Transkript:** kein Text für {speaker} von [{start}] bis [{end}] (Transkription fehlgeschlagen).",
        "overview": "Überblick",
        "topics": "Themen",
        "decisions": "Beschlüsse",
        "action_items": "Aufgaben",
        "open_questions": "Offene Fragen",
        "polls": "Umfragen",
        "none": "Keine Einträge.",
        "due": "fällig",
        "option": "Option",
        "votes": "Stimmen",
        "speaker": "Sprecher {n}",
        "vtt_header": "NOTE Automatisches Transkript (Spracherkennung). Zeiten sind Aufnahmezeit; Pausen sind entfernt.",
        "vtt_pause": "NOTE Aufnahme hier pausiert.",
        "vtt_gap": "NOTE Lücke {start} bis {end}, {name}: kein Transkript ({reason}).",
    },
}



def load(*parts):
    with open(os.path.join(RENDER, *parts), encoding="utf-8") as f:
        return json.load(f)


def write(path, text):
    with open(os.path.join(RENDER, path), "w", encoding="utf-8", newline="\n") as f:
        f.write(text)
    print("wrote testdata/render/" + path)


# ---------------------------------------------------------------- time


def parse_utc(s):
    return datetime.datetime.fromisoformat(s.replace("Z", "+00:00"))


def period_of(periods, t):
    for p in periods:
        if p["start_ms"] <= t <= p["end_ms"]:
            return p
    raise ValueError("time %d ms lies in no recording period" % t)


def recorded(p, t):
    return t - p["start_ms"] + p["recorded_offset_ms"]


def hms(ms):  # [hh:mm:ss], floor to seconds
    s = ms // 1000
    return "%02d:%02d:%02d" % (s // 3600, s // 60 % 60, s % 60)


def vtt_time(ms):  # hh:mm:ss.ttt, hours at least two digits (templates.md §6)
    s, t = divmod(ms, 1000)
    return "%02d:%02d:%02d.%03d" % (s // 3600, s // 60 % 60, s % 60, t)


# ---------------------------------------------------------------- language and title


def output_language(transcript, default_language):
    lang = transcript.get("language")
    return lang if lang in STRINGS else default_language  # templates.md §7


def clean_title_name(name):  # templates.md §5.1: control characters and newlines -> space, collapse runs
    name = "".join(" " if unicodedata.category(c) == "Cc" else c for c in name)
    return re.sub(" +", " ", name)


def title(meeting_name, origin_utc, tz):
    date = parse_utc(origin_utc).astimezone(zoneinfo.ZoneInfo(tz)).strftime("%Y-%m-%d")
    name = clean_title_name(meeting_name)
    if len(name) + 1 + len(date) > 200:
        name = name[: 200 - 1 - len(date) - 1] + "…"
    return name + " " + date


# ---------------------------------------------------------------- Markdown (templates.md §5.2)

MD_ALWAYS = set("\\`*_~[]<>#|")


def md(s):
    """Escape one string from meeting content or the LLM (templates.md §5.2).

    Newlines in any value become spaces; backslash before \\ ` * _ ~ [ ] < > # |; at the start of the
    value a leading -, +, = gets a backslash ('>' is escaped anyway) and a leading <digits>. becomes <digits>\\.
    """
    s = s.replace("\r\n", " ").replace("\n", " ").replace("\r", " ")
    out = "".join("\\" + c if c in MD_ALWAYS else c for c in s)
    m = re.match(r"[0-9]+\.", out)
    if out[:1] in ("-", "+", "="):
        out = "\\" + out
    elif m:
        out = out[: m.end() - 1] + "\\" + out[m.end() - 1 :]
    return out


def speaker_name(transcript, ref, lang):
    for sp in transcript["speakers"]:
        if sp["ref"] == ref and sp.get("display_name"):
            return sp["display_name"]
    return STRINGS[lang]["speaker"].format(n=ref.split("-")[1])


def summary_md(ctx, transcript, summary, lang, tz):
    S = STRINGS[lang]
    zone = zoneinfo.ZoneInfo(tz)
    origin = parse_utc(transcript["time_base"]["origin_utc"])
    periods = transcript["recording_periods"]
    date = origin.astimezone(zone).strftime("%Y-%m-%d")
    first = (origin + datetime.timedelta(milliseconds=periods[0]["start_ms"])).astimezone(zone).strftime("%H:%M")
    last = (origin + datetime.timedelta(milliseconds=periods[-1]["end_ms"])).astimezone(zone).strftime("%H:%M")
    duration = sum(p["end_ms"] - p["start_ms"] for p in periods)
    blocks = []
    # 1 header, 2 notice
    blocks.append("*%s · %s · %s–%s %s · %s: %s*" % (md(ctx["meeting_name"]), date, first, last, tz, S["recorded"], hms(duration)))
    blocks.append(S["notice"])
    # 3 gaps
    for g in transcript["gaps"]:
        ps, pe = period_of(periods, g["start_ms"]), period_of(periods, g["end_ms"])
        blocks.append(S["gap"].format(
            speaker=md(speaker_name(transcript, g["speaker_ref"], lang)),
            start=hms(recorded(ps, g["start_ms"])), end=hms(recorded(pe, g["end_ms"]))))
    # 4 overview
    blocks.append("## " + S["overview"])
    blocks.append(md(summary["overview"]))
    # 5 topics
    blocks.append("## " + S["topics"])
    if not summary["topics"]:
        blocks.append(S["none"])
    for tp in summary["topics"]:
        blocks.append("### " + md(tp["title"]) + (" [%s]" % tp["start"] if tp.get("start") else ""))
        blocks.append(md(tp["summary"]))
    # 6 decisions
    blocks.append("## " + S["decisions"])
    items = ["- " + md(d["text"]) + (" [%s]" % d["at"] if d.get("at") else "") for d in summary["decisions"]]
    blocks.append("\n".join(items) if items else S["none"])
    # 7 action items
    blocks.append("## " + S["action_items"])
    items = []
    for a in summary["action_items"]:
        line = "- [ ] " + md(a["task"])
        if a.get("owner"):
            line += " — " + md(a["owner"])
        if a.get("due"):
            line += " — %s: %s" % (S["due"], md(a["due"]))
        if a.get("at"):
            line += " [%s]" % a["at"]
        items.append(line)
    blocks.append("\n".join(items) if items else S["none"])
    # 8 open questions
    blocks.append("## " + S["open_questions"])
    items = ["- " + md(q["text"]) for q in summary["open_questions"]]
    blocks.append("\n".join(items) if items else S["none"])
    # 9 polls (aggregate counts only)
    if ctx.get("polls"):
        blocks.append("## " + S["polls"])
        for poll in ctx["polls"]:
            votes = [0] * len(poll["options"])
            for r in poll["responses"]:
                votes[r["option"]] += 1
            blocks.append("**" + md(poll["question"]) + "**")
            rows = ["| %s | %s |" % (S["option"], S["votes"]), "| --- | --- |"]
            rows += ["| %s | %d |" % (md(o), v) for o, v in zip(poll["options"], votes)]
            blocks.append("\n".join(rows))
    # 10 footer
    models = []
    for tr in transcript["tracks"]:
        m = tr.get("provider", {}).get("model")
        if m and m not in models:
            models.append(m)
    blocks.append("---")
    blocks.append("*%s %s · ASR: %s · LLM: %s · %s*" % (
        FORMAT_ID, FORMAT_VERSION, md(", ".join(models)), md(ctx["llm_model"]), ctx["generated_at"]))
    return "\n\n".join(blocks) + "\n"


# ---------------------------------------------------------------- WebVTT (templates.md §6)


def vtt_clean(s):
    s = s.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")
    s = re.sub("[\r\n\t]", " ", s)
    return re.sub(" +", " ", s).strip()


def note_name(s):  # comment text: no character references; '-->' becomes '->' until none is left (§6)
    s = re.sub("[\r\n\t]", " ", s)
    s = re.sub(" +", " ", s).strip()
    while "-->" in s:
        s = s.replace("-->", "->")
    return s


def transcript_vtt(transcript, lang):
    periods = transcript["recording_periods"]
    cues = []
    for seg in transcript["segments"]:
        mid2 = seg["start_ms"] + seg["end_ms"]  # twice the midpoint, keeps integers
        idx = [i for i, p in enumerate(periods) if 2 * p["start_ms"] <= mid2 <= 2 * p["end_ms"]][0]
        p = periods[idx]
        s = recorded(p, min(max(seg["start_ms"], p["start_ms"]), p["end_ms"]))
        e = recorded(p, min(max(seg["end_ms"], p["start_ms"]), p["end_ms"]))
        if e <= s:
            e = s + 1
        name = vtt_clean(speaker_name(transcript, seg["speaker_ref"], lang))
        cues.append({"period": idx, "start": s, "block": "%s\n%s --> %s\n<v %s>%s</v>" % (
            seg["id"], vtt_time(s), vtt_time(e), name, vtt_clean(seg["text"]))})
    gap_notes = []
    for g in transcript["gaps"]:
        gs = recorded(period_of(periods, g["start_ms"]), g["start_ms"])
        ge = recorded(period_of(periods, g["end_ms"]), g["end_ms"])
        text = STRINGS[lang]["vtt_gap"].format(
            start=vtt_time(gs), end=vtt_time(ge), name=note_name(speaker_name(transcript, g["speaker_ref"], lang)), reason=g["reason"])
        gap_notes.append((gs, text))
    blocks = ["WEBVTT", STRINGS[lang]["vtt_header"]]
    pending = list(gap_notes)
    for i, c in enumerate(cues):
        pause = i > 0 and c["period"] != cues[i - 1]["period"] and c["period"] > 0
        if i == 0 and c["period"] > 0:
            pause = True
        here = [n for n in pending if n[0] <= c["start"]]
        if pause:
            blocks.append(STRINGS[lang]["vtt_pause"])
        for n in here:
            blocks.append(n[1])
            pending.remove(n)
        blocks.append(c["block"])
    blocks += [n[1] for n in pending]
    return "\n\n".join(blocks) + "\n"


# ---------------------------------------------------------------- LLM reply fixtures (templates.md §4)

TIME = {"type": "string", "pattern": "^[0-9]{2}:[0-5][0-9]:[0-5][0-9]$"}


def obj(props, required):
    return {"type": "object", "additionalProperties": False, "required": required, "properties": props}


def text(max_len, min_len=1):
    return {"type": "string", "minLength": min_len, "maxLength": max_len}


def reference_schema(head, head_max, caps):
    return obj({
        head: text(head_max),
        "topics": {"type": "array", "maxItems": caps[0], "items": obj(
            {"title": text(120), "summary": text(2000), "start": TIME}, ["title", "summary"])},
        "decisions": {"type": "array", "maxItems": caps[1], "items": obj({"text": text(500), "at": TIME}, ["text"])},
        "action_items": {"type": "array", "maxItems": caps[2], "items": obj(
            {"task": text(500), "owner": text(120, 0), "due": text(60, 0), "at": TIME}, ["task"])},
        "open_questions": {"type": "array", "maxItems": caps[3], "items": obj({"text": text(500)}, ["text"])},
    }, [head, "topics", "decisions", "action_items", "open_questions"])


SCHEMAS = {
    "summary.v1": reference_schema("overview", 2000, (12, 50, 50, 30)),
    "chunk-notes.v1": reference_schema("chunk_summary", 1500, (6, 25, 25, 15)),  # "each array capped at half"
}


def limits(head, head_len, caps):
    return {
        head: "o" * head_len,
        "topics": [{"title": "t" * 120, "summary": "s" * 2000, "start": "99:59:59"} for _ in range(caps[0])],
        "decisions": [{"text": "d" * 500, "at": "00:00:00"} for _ in range(caps[1])],
        "action_items": [{"task": "a" * 500, "owner": "w" * 120, "due": "u" * 60, "at": "00:00:01"} for _ in range(caps[2])],
        "open_questions": [{"text": "q" * 500} for _ in range(caps[3])],
    }


def llm_fixtures():
    f01 = load("F-01", "summary.json")
    base_chunk = {
        "chunk_summary": "Chunk notes fixture: Ben proposed blue for the test widget and Ada agreed.",
        "topics": [{"title": "Colour of the test widget", "summary": "Ben proposed blue. Ada agreed.", "start": "00:00:06"}],
        "decisions": [{"text": "The test widget is blue.", "at": "00:00:56"}],
        "action_items": [{"task": "Update the widget specification", "owner": "Ben Testperson", "due": "Friday"}],
        "open_questions": [],
    }

    def mut(base, fn):
        d = copy.deepcopy(base)
        fn(d)
        return d

    def topics(n):
        return [{"title": "Topic %d" % i, "summary": "Summary %d." % i} for i in range(n)]

    out = {"summary.v1": {}, "chunk-notes.v1": {}}
    s = out["summary.v1"]
    s["valid.f01"] = f01
    s["valid.minimal"] = {"overview": "x", "topics": [], "decisions": [], "action_items": [], "open_questions": []}
    s["valid.limits"] = limits("overview", 2000, (12, 50, 50, 30))
    s["invalid.not-an-object"] = [f01]
    s["invalid.missing-overview"] = mut(f01, lambda d: d.pop("overview"))
    s["invalid.empty-overview"] = mut(f01, lambda d: d.update(overview=""))
    s["invalid.overview-too-long"] = mut(f01, lambda d: d.update(overview="o" * 2001))
    s["invalid.missing-topics"] = mut(f01, lambda d: d.pop("topics"))
    s["invalid.missing-open-questions"] = mut(f01, lambda d: d.pop("open_questions"))
    s["invalid.llm-title"] = mut(f01, lambda d: d.update(title="Fixture Widget Meeting"))
    s["invalid.topics-not-array"] = mut(f01, lambda d: d.update(topics={"title": "x", "summary": "y"}))
    s["invalid.topic-extra-field"] = mut(f01, lambda d: d["topics"][0].update(speaker="Ada Testperson"))
    s["invalid.topic-missing-summary"] = mut(f01, lambda d: d["topics"][0].pop("summary"))
    s["invalid.topic-empty-title"] = mut(f01, lambda d: d["topics"][0].update(title=""))
    s["invalid.topic-title-too-long"] = mut(f01, lambda d: d["topics"][0].update(title="t" * 121))
    s["invalid.topics-too-many"] = mut(f01, lambda d: d.update(topics=topics(13)))
    s["invalid.start-one-digit-hour"] = mut(f01, lambda d: d["topics"][0].update(start="0:00:01"))
    s["invalid.at-minutes-60"] = mut(f01, lambda d: d["decisions"][0].update(at="00:60:00"))
    s["invalid.at-with-fraction"] = mut(f01, lambda d: d["decisions"][0].update(at="00:00:56.000"))
    s["invalid.decision-empty-text"] = mut(f01, lambda d: d["decisions"][0].update(text=""))
    s["invalid.decision-text-too-long"] = mut(f01, lambda d: d["decisions"][0].update(text="d" * 501))
    s["invalid.decisions-too-many"] = mut(f01, lambda d: d.update(decisions=[{"text": "d"}] * 51))
    s["invalid.action-missing-task"] = mut(f01, lambda d: d["action_items"][0].pop("task"))
    s["invalid.action-owner-too-long"] = mut(f01, lambda d: d["action_items"][0].update(owner="w" * 121))
    s["invalid.action-due-too-long"] = mut(f01, lambda d: d["action_items"][0].update(due="u" * 61))
    s["invalid.action-extra-field"] = mut(f01, lambda d: d["action_items"][0].update(owner_email="ben.testperson@example.org"))
    s["invalid.action-items-too-many"] = mut(f01, lambda d: d.update(action_items=[{"task": "a"}] * 51))
    s["invalid.open-questions-too-many"] = mut(f01, lambda d: d.update(open_questions=[{"text": "q"}] * 31))
    s["invalid.chunk-notes-reply"] = base_chunk

    c = out["chunk-notes.v1"]
    c["valid.base"] = base_chunk
    c["valid.limits"] = limits("chunk_summary", 1500, (6, 25, 25, 15))
    c["invalid.summary-v1-reply"] = f01
    c["invalid.missing-chunk-summary"] = mut(base_chunk, lambda d: d.pop("chunk_summary"))
    c["invalid.chunk-summary-too-long"] = mut(base_chunk, lambda d: d.update(chunk_summary="o" * 1501))
    c["invalid.topics-too-many"] = mut(base_chunk, lambda d: d.update(topics=topics(7)))
    c["invalid.decisions-too-many"] = mut(base_chunk, lambda d: d.update(decisions=[{"text": "d"}] * 26))
    c["invalid.action-items-too-many"] = mut(base_chunk, lambda d: d.update(action_items=[{"task": "a"}] * 26))
    c["invalid.open-questions-too-many"] = mut(base_chunk, lambda d: d.update(open_questions=[{"text": "q"}] * 16))
    c["invalid.topic-extra-field"] = mut(base_chunk, lambda d: d["topics"][0].update(speaker="Ben Testperson"))
    return out


def check_and_write_llm_fixtures():
    for schema_name, cases in llm_fixtures().items():
        d = os.path.join(RENDER, "llm", schema_name)
        os.makedirs(d, exist_ok=True)
        for old in os.listdir(d):
            os.remove(os.path.join(d, old))
        validator = jsonschema.Draft202012Validator(SCHEMAS[schema_name])
        for name, value in sorted(cases.items()):
            ok = validator.is_valid(value)
            if ok != name.startswith("valid."):
                raise SystemExit("llm/%s/%s.json: reference schema says valid=%s" % (schema_name, name, ok))
            write(os.path.join("llm", schema_name, name + ".json"), json.dumps(value, indent=2, ensure_ascii=False) + "\n")
    for case in ("F-01", "gap"):
        jsonschema.validate(load(case, "summary.json"), SCHEMAS["summary.v1"])


# ---------------------------------------------------------------- main


def fixtures_md_vtt():
    with open(os.path.join(ROOT, "docs", "reference", "fixtures.md"), encoding="utf-8") as f:
        doc = f.read()
    section = doc.split("### 7.1 ", 1)[1]
    return section.split("```\n", 2)[1]


def main():
    with open(os.path.join(ROOT, "docs", "reference", "schemas", "transcript.schema.json"), encoding="utf-8") as f:
        transcript_schema = json.load(f)
    for case in ("F-01", "gap", "mapping"):
        jsonschema.validate(load(case, "transcript.json"), transcript_schema)

    for case, out, language in (("F-01", "summary.md", None), ("F-01", "summary.de.md", "de"), ("gap", "summary.md", None),
                                ("gap", "summary.de.md", "de")):
        transcript = load(case, "transcript.json")
        if language:
            transcript["language"] = language
        lang = output_language(transcript, "en")
        write(os.path.join(case, out), summary_md(load(case, "context.json"), transcript, load(case, "summary.json"), lang, "UTC"))
    for case, out, language in (("F-01", "transcript.vtt", None), ("F-01", "transcript.de.vtt", "de"),
                                ("gap", "transcript.vtt", None), ("gap", "transcript.de.vtt", "de"),
                                ("mapping", "transcript.vtt", None)):
        transcript = load(case, "transcript.json")
        if language:
            transcript["language"] = language
        write(os.path.join(case, out), transcript_vtt(transcript, output_language(transcript, "en")))

    with open(os.path.join(RENDER, "F-01", "transcript.vtt"), encoding="utf-8") as f:
        if f.read() != fixtures_md_vtt():
            raise SystemExit("F-01/transcript.vtt differs from docs/reference/fixtures.md §7.1")
    print("F-01/transcript.vtt equals docs/reference/fixtures.md §7.1")

    f01 = load("F-01", "context.json")
    origin = load("F-01", "transcript.json")["time_base"]["origin_utc"]
    print("title F-01:", title(f01["meeting_name"], origin, "UTC"))
    check_and_write_llm_fixtures()


if __name__ == "__main__":
    sys.exit(main())
