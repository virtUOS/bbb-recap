"""Manifest validation rules that the ingest side applies (docs/reference/meeting-package.md §7).

check(data) returns the list of rule names a manifest's bytes violate:
  V-I01-non-canonical              bytes are not JCS-canonical (400 package-id-mismatch)
  V-I02-unsupported-schema-version major != 1 or minor newer than 1.0 (422 unsupported-schema-version)
  V-I02-schema                     JSON Schema docs/reference/schemas/manifest.schema.json, incl. role path
                                   patterns (422 manifest-invalid)
  V-I04-paths-duplicate            a path occurs twice (422 manifest-invalid)
  V-I04-paths-unsorted             paths not sorted bytewise by UTF-8 (422 manifest-invalid)
  V-I04-totals                     totals != count and sum of sizes of the entries (422 manifest-invalid)
  V-I08-breakout                   breakout is true (422 manifest-invalid)
An unsupported schema_version stops further checks (the reader has no schema for it).
"""
import json
import os
import re
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import jcs  # noqa: E402

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
SCHEMA_PATH = os.path.join(ROOT, "docs", "reference", "schemas", "manifest.schema.json")
SUPPORTED_MINOR = 0
SEMVER = re.compile(r"^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$")


def load_validator(path):
    import jsonschema  # imported here so scripts that only build goldens run under python3 -I

    with open(path, encoding="utf-8") as f:
        schema = json.load(f)
    cls = jsonschema.validators.validator_for(schema)
    cls.check_schema(schema)
    return cls(schema, format_checker=cls.FORMAT_CHECKER)


_VALIDATOR = None


def check(data):
    global _VALIDATOR
    if _VALIDATOR is None:
        _VALIDATOR = load_validator(SCHEMA_PATH)
    failed = []
    obj = json.loads(data)
    if jcs.canonicalize(obj) != data:
        failed.append("V-I01-non-canonical")
    m = SEMVER.match(str(obj.get("schema_version", ""))) if isinstance(obj, dict) else None
    if m and (int(m.group(1)) != 1 or int(m.group(2)) > SUPPORTED_MINOR):
        return failed + ["V-I02-unsupported-schema-version"]
    if any(True for _ in _VALIDATOR.iter_errors(obj)):
        failed.append("V-I02-schema")
    files = obj.get("files") if isinstance(obj, dict) else None
    if isinstance(files, list) and all(isinstance(e, dict) for e in files):
        paths = [e.get("path", "") for e in files]
        if len(set(paths)) != len(paths):
            failed.append("V-I04-paths-duplicate")
        if paths != sorted(paths, key=lambda p: p.encode("utf-8")):
            failed.append("V-I04-paths-unsorted")
        totals = obj.get("totals", {})
        if totals.get("file_count") != len(files) or totals.get("size") != sum(e.get("size", 0) for e in files):
            failed.append("V-I04-totals")
    if isinstance(obj, dict) and obj.get("breakout") is True:
        failed.append("V-I08-breakout")
    return failed
