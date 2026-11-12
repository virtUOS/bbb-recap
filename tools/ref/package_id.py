#!/usr/bin/env python3
"""Print the package_id of a fixture: lowercase hex SHA-256 of the JCS (RFC 8785) canonical manifest.

Usage: python3 -I tools/ref/package_id.py testdata/meetings/F-01
Reads <fixture>/expected/manifest.json. Exits 1 if the file bytes are not already canonical
(docs/reference/meeting-package.md §4 and V-I01: the file bytes themselves hash to package_id).
"""
import hashlib
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))  # -I drops the script dir from sys.path
import jcs  # noqa: E402


def package_id(manifest_bytes):
    return hashlib.sha256(jcs.canonicalize(json.loads(manifest_bytes))).hexdigest()


def main():
    if len(sys.argv) != 2:
        sys.exit("usage: package_id.py <fixture dir>")
    path = os.path.join(sys.argv[1], "expected", "manifest.json")
    with open(path, "rb") as f:
        data = f.read()
    print(package_id(data))
    if jcs.canonicalize(json.loads(data)) != data:
        sys.exit("package_id.py: %s is not JCS-canonical" % path)


if __name__ == "__main__":
    main()
