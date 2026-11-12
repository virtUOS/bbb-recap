#!/usr/bin/env python3
"""Check the manifest and registration fixtures.

- every testdata/meetings/*/expected/manifest.json passes every rule of manifest_rules.py;
- every testdata/manifests/invalid/<M-ID>.<rule>.json fails exactly <rule>;
- every testdata/meetings/*/registration.json validates against docs/reference/schemas/registration.schema.json.
Prints one line per file; exits 1 if any check fails.
Usage: python3 -I tools/ref/validate_manifests.py
"""
import glob
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import manifest_rules  # noqa: E402

ROOT = manifest_rules.ROOT


def rel(p):
    return os.path.relpath(p, ROOT)


def main():
    bad = 0
    for path in sorted(glob.glob(os.path.join(ROOT, "testdata/meetings/*/expected/manifest.json"))):
        with open(path, "rb") as f:
            failed = manifest_rules.check(f.read())
        print("%s %s%s" % ("OK  " if not failed else "FAIL", rel(path), " violates " + ", ".join(failed) if failed else " valid"))
        bad += bool(failed)
    for path in sorted(glob.glob(os.path.join(ROOT, "testdata/manifests/invalid/*.json"))):
        name = os.path.basename(path)[: -len(".json")]
        expected = name.split(".", 1)[1] if "." in name else "?"
        with open(path, "rb") as f:
            failed = manifest_rules.check(f.read())
        ok = failed == [expected]
        print("%s %s fails %s (expected exactly %s)" % ("OK  " if ok else "FAIL", rel(path), failed, expected))
        bad += not ok
    reg = manifest_rules.load_validator(os.path.join(ROOT, "docs/reference/schemas/registration.schema.json"))
    for path in sorted(glob.glob(os.path.join(ROOT, "testdata/meetings/*/registration.json"))):
        with open(path, encoding="utf-8") as f:
            errors = [e.message for e in reg.iter_errors(json.load(f))]
        print("%s %s%s" % ("OK  " if not errors else "FAIL", rel(path), " " + "; ".join(errors) if errors else " valid"))
        bad += bool(errors)
    if bad:
        print("%d check(s) failed" % bad)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
