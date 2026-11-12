#!/usr/bin/env python3
"""Write the package goldens with the reference scripts (docs/reference/fixtures.md §2, §6, §7).

For each fixture that the exporter turns into a package: expected/manifest.json (canonical bytes)
and expected/package_id.txt. Then testdata/manifests/invalid/*.json
(M-03...M-06), derived from F-01's manifest so that each breaks exactly one rule.
Usage: python3 -I tools/ref/make_goldens.py   (run after tools/fixturegen/fixturegen.py)
"""
import hashlib
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import build_manifest  # noqa: E402
import jcs  # noqa: E402
import manifest_rules  # noqa: E402

ROOT = manifest_rules.ROOT
MEETINGS = os.path.join(ROOT, "testdata", "meetings")

# Injected exporter inputs (CT-EXP-02 "injected clock"): server credential, build time, exporter version.
BBB_SERVER_ID = "bbb-fixture-a"
CREATED_AT = "2025-10-29T10:10:00Z"
EXPORTER_VERSION = "0.0.0-fixture"

# fixture -> fixture whose raw dir it uses (F-07 is F-01's raw dir without a registration)
PACKAGES = {"F-01": "F-01", "F-04": "F-04", "F-06": "F-06", "F-07": "F-01"}


def raw_dir(fid):
    base = os.path.join(MEETINGS, fid, "raw")
    (rid,) = os.listdir(base)
    return os.path.join(base, rid)


def write(path, data):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "wb") as f:
        f.write(data)
    os.chmod(path, 0o644)


def invalid_manifests(manifest):
    """M-03...M-06 (fixtures.md §6). Each must fail exactly the rule in its file name."""
    def canon(m):
        return jcs.canonicalize(m)

    def copy():
        return json.loads(json.dumps(manifest))

    out = {}
    m = copy()  # M-03: path ../events.xml (kept sorted, totals unchanged)
    for e in m["files"]:
        if e["path"] == "events.xml":
            e["path"] = "../events.xml"
    m["files"].sort(key=lambda e: e["path"].encode("utf-8"))
    out["M-03.V-I02-schema"] = canon(m)
    m = copy()  # M-04a: duplicate path, adjacent so the list stays sorted; totals include it
    i = next(k for k, e in enumerate(m["files"]) if e["role"] == "audio-microphone")
    m["files"].insert(i + 1, dict(m["files"][i]))
    m["totals"] = {"file_count": len(m["files"]), "size": sum(e["size"] for e in m["files"])}
    out["M-04a.V-I04-paths-duplicate"] = canon(m)
    m = copy()  # M-04b: unsorted files
    m["files"][0], m["files"][1] = m["files"][1], m["files"][0]
    out["M-04b.V-I04-paths-unsorted"] = canon(m)
    m = copy()  # M-04c: wrong totals
    m["totals"]["size"] += 1
    out["M-04c.V-I04-totals"] = canon(m)
    m = copy()  # M-05a / M-05b: schema_version 2.0.0 / 1.9.0
    m["schema_version"] = "2.0.0"
    out["M-05a.V-I02-unsupported-schema-version"] = canon(m)
    m["schema_version"] = "1.9.0"
    out["M-05b.V-I02-unsupported-schema-version"] = canon(m)
    # M-06: valid content, non-canonical bytes (indented)
    out["M-06.V-I01-non-canonical"] = (json.dumps(manifest, indent=2, ensure_ascii=False) + "\n").encode("utf-8")
    return out


def main():
    f01 = None
    for fid, src in PACKAGES.items():
        rd = raw_dir(src)
        manifest = build_manifest.build(rd, BBB_SERVER_ID, CREATED_AT, EXPORTER_VERSION)
        data = jcs.canonicalize(manifest)
        exp = os.path.join(MEETINGS, fid, "expected")
        write(os.path.join(exp, "manifest.json"), data)
        write(os.path.join(exp, "package_id.txt"), (hashlib.sha256(data).hexdigest() + "\n").encode())
        if fid == "F-01":
            f01 = manifest
    for name, data in invalid_manifests(f01).items():
        write(os.path.join(ROOT, "testdata", "manifests", "invalid", name + ".json"), data)


if __name__ == "__main__":
    main()
