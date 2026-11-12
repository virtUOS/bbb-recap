#!/usr/bin/env python3
"""Reference manifest builder for a raw recording directory (docs/reference/meeting-package.md §2, §4, §5).

Usage: python3 -I tools/ref/build_manifest.py <raw/<record_id> dir> --bbb-server-id ID --created-at TS
           [--exporter-version V] [--meta-prefix recap]
Prints the JCS-canonical manifest bytes (no trailing newline). Independent of the Go code under test.
"""
import argparse
import hashlib
import os
import re
import sys
import xml.etree.ElementTree as ET

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import jcs  # noqa: E402

# meeting-package.md §8 limits
MAX_FILE = 2 * 1024**3
MAX_PACKAGE = 8 * 1024**3
MAX_FILES = 10000

MIC = re.compile(r"^audio/microphone-[^/]+\.webm$")
NOTES = re.compile(r"^notes/(notes\.(html|md|txt)|export_failed)$")


class BuildError(Exception):
    """An exporter failure reason (README "Exporter states per recording")."""


class Skipped(Exception):
    """Not exported and not a failure: bbb-exporter.md §4.1 step 2 drops a record whose opt-in or breakout
    check no longer passes ("delete the record, log, no failure"). The only token the reference defines for a
    breakout is the hook's `breakout_skipped` (§3.3); there is no exporter failure reason for it."""


def role_of(path):
    """Allow-list of meeting-package.md §5. Returns the role or None (excluded)."""
    if path == "events.xml":
        return "events"
    if MIC.match(path):
        return "audio-microphone"
    if NOTES.match(path):
        return "notes"
    if path.startswith("presentation/"):
        return "presentation"
    return None


def select_files(raw_dir):
    """Walk raw_dir without following symlinks. Returns (sorted [(path, role, abspath)], skipped count)."""
    selected, skipped = [], 0
    for dirpath, dirnames, filenames in os.walk(raw_dir, followlinks=False):
        for name in dirnames + filenames:
            full = os.path.join(dirpath, name)
            rel = os.path.relpath(full, raw_dir).replace(os.sep, "/")
            if os.path.islink(full):
                skipped += 1  # symlinks are skipped and counted (§4 "Paths")
                continue
            if name in filenames:
                if not os.path.isfile(full):
                    skipped += 1
                    continue
                role = role_of(rel)
                if role:
                    selected.append((rel, role, full))
    selected.sort(key=lambda t: t[0].encode("utf-8"))
    return selected, skipped


def sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def build(raw_dir, bbb_server_id, created_at, exporter_version, meta_prefix="recap"):
    raw_dir = os.path.normpath(raw_dir)
    events = os.path.join(raw_dir, "events.xml")
    if not os.path.isfile(events) or os.path.islink(events):
        raise BuildError("no_events")
    root = ET.parse(events).getroot()
    meeting = root.find("meeting")
    if meeting is not None and meeting.get("breakout") == "true":
        raise Skipped("breakout_skipped")
    files, skipped = select_files(raw_dir)
    if not any(role == "audio-microphone" for _, role, _ in files):
        audio = os.path.join(raw_dir, "audio")
        other = os.path.isdir(audio) and len(os.listdir(audio)) > 0
        raise BuildError("unsupported_audio_bridge" if other else "no_microphone_tracks")
    entries = []
    for rel, role, full in files:
        size = os.path.getsize(full)
        if size > MAX_FILE:
            raise BuildError("limit_exceeded")
        entries.append({"path": rel, "role": role, "size": size, "sha256": sha256_file(full)})
    total = sum(e["size"] for e in entries)
    if total > MAX_PACKAGE or len(entries) > MAX_FILES:
        raise BuildError("limit_exceeded")
    md = root.find("metadata")
    attrs = dict(md.attrib) if md is not None else {}
    metadata = {k: v for k, v in attrs.items()
                if k.startswith(meta_prefix + "-") or k in ("bbb-origin", "bbb-origin-server-name")}
    manifest = {
        "schema_version": "1.0.0",
        "created_at": created_at,
        "exporter": {"name": "recap-exporter", "version": exporter_version},
        "bbb_server_id": bbb_server_id,
        "record_id": os.path.basename(raw_dir),
        "meeting_id": meeting.get("externalId"),
        "breakout": False,
        "meta_prefix": meta_prefix,
        "metadata": metadata,
        "files": entries,
        "totals": {"file_count": len(entries), "size": total},
    }
    if root.get("bbb_version") is not None:
        manifest["bbb_version"] = root.get("bbb_version")
    if skipped:
        print("build_manifest: skipped %d non-regular entries" % skipped, file=sys.stderr)
    return manifest


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("raw_dir")
    ap.add_argument("--bbb-server-id", required=True)
    ap.add_argument("--created-at", required=True)
    ap.add_argument("--exporter-version", default="0.0.0-fixture")
    ap.add_argument("--meta-prefix", default="recap")
    a = ap.parse_args()
    try:
        m = build(a.raw_dir, a.bbb_server_id, a.created_at, a.exporter_version, a.meta_prefix)
    except BuildError as e:
        sys.exit("build_manifest: failed: %s" % e)
    except Skipped as e:
        sys.exit("build_manifest: skipped: %s" % e)
    sys.stdout.buffer.write(jcs.canonicalize(m))


if __name__ == "__main__":
    main()
