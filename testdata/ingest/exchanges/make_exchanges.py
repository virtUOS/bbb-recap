"""Write the golden ingest exchanges (docs/reference/bbb-exporter.md §6.6) from the fixture manifests.

Run from the repository root:  python3 -B -I testdata/ingest/exchanges/make_exchanges.py
Reads only testdata/ and the JCS helper in tools/ref/. Output is deterministic. The format is described in
README.md next to this script. The script is independent of the code under test.
"""

import hashlib
import json
import os
import sys

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
sys.path.insert(0, os.path.join(ROOT, "tools", "ref"))
import jcs  # noqa: E402

TESTDATA = os.path.join(ROOT, "testdata")
OUT = os.path.join(TESTDATA, "ingest", "exchanges")
BASE = "/api/v1/ingest/packages/"
CRED_A = "bbb_server.bbb-fixture-a"
CRED_B = "bbb_server.bbb-fixture-b"
JSON = {"Content-Type": "application/json"}
PROBLEM = {"Content-Type": "application/problem+json"}


def sha256(b):
    return hashlib.sha256(b).hexdigest()


def read(rel):
    with open(os.path.join(TESTDATA, rel), "rb") as f:
        return f.read()


def write(rel, data):
    path = os.path.join(TESTDATA, rel)
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "wb") as f:
        f.write(data)


def body(rel, flip_byte=None):
    data = bytearray(read(rel))
    b = {"source": rel}
    if flip_byte is not None:
        data[flip_byte] ^= 0xFF
        b["flip_byte"] = flip_byte
    b["sha256"] = sha256(bytes(data))
    b["size"] = len(data)
    return b


class Package:
    """A manifest plus the raw dir its files come from."""

    def __init__(self, fixture, manifest_rel, dup_ok=False):
        self.fixture = fixture
        self.manifest_rel = manifest_rel
        raw = read(manifest_rel)
        self.manifest = json.loads(raw)
        if jcs.canonicalize(self.manifest) != raw:
            raise SystemExit("not canonical: " + manifest_rel)
        self.id = sha256(raw)
        self.record_id = self.manifest["record_id"]
        # One entry per hash (missing lists hashes, bbb-exporter.md §6.3 step 5), taken from a path that exists in
        # the fixture raw dir.
        self.files = []
        for f in self.manifest["files"]:
            if any(g["sha256"] == f["sha256"] for g in self.files):
                continue
            same = [g for g in self.manifest["files"] if g["sha256"] == f["sha256"]]
            self.files.append(next(g for g in same if os.path.exists(os.path.join(TESTDATA, self.raw_rel(g)))))
        self.dups = len(self.files) != len(self.manifest["files"])
        if self.dups and not dup_ok:
            raise SystemExit("duplicate file hashes in " + manifest_rel)
        self.size = self.manifest["totals"]["size"]
        self.job = "job-" + fixture

    def raw_rel(self, f):
        return "meetings/%s/raw/%s/%s" % (self.fixture, self.record_id, f["path"])

    def path(self, suffix=""):
        return BASE + self.id + suffix

    def put_manifest(self, cred=CRED_A):
        return {"method": "PUT", "path": self.path(), "credential": cred, "headers": JSON,
                "body": body(self.manifest_rel)}

    def get(self):
        return {"method": "GET", "path": self.path(), "credential": CRED_A, "body": None}

    def put_file(self, f, flip_byte=None):
        return {"method": "PUT", "path": self.path("/files/" + f["sha256"]), "credential": CRED_A,
                "body": body(self.raw_rel(f), flip_byte)}

    def commit(self):
        return {"method": "POST", "path": self.path("/commit"), "credential": CRED_A, "headers": JSON,
                "body": body("ingest/exchanges/bodies/empty-object.json")}

    def status(self, state, missing, ack=False):
        received = self.size - sum(f["size"] for f in missing)
        b = {"package_id": self.id, "state": state, "missing": [f["sha256"] for f in missing],
             "received_size": received, "expected_size": self.size}
        if self.dups:  # whether sizes count a shared blob once or per path is not specified
            del b["received_size"], b["expected_size"]
        if ack:
            b.update(self.ack_fields())
        return {"status": 200, "headers": JSON, "body": b}

    def ack_fields(self):
        return {"job_id": "{{uuid:%s}}" % self.job, "record_id": self.record_id,
                "file_count": self.manifest["totals"]["file_count"], "size": self.size,
                "committed_at": "{{time:%s}}" % self.job}

    def ack(self):
        b = {"package_id": self.id, "state": "committed"}
        b.update(self.ack_fields())
        return {"status": 200, "headers": JSON, "body": b}

    def created(self):
        r = self.status("receiving", self.files)
        r["status"] = 201
        return r

    def uploads(self, files, status=204, **extra):
        return [dict(step(self.put_file(f), {"status": status, "body": None}), **extra) for f in files]

    def finish(self):
        """Re-check (missing empty) and commit."""
        return [step(self.put_manifest(), self.status("complete", [])), step(self.commit(), self.ack())]


def step(request, response, **extra):
    s = {"request": request, "response": response}
    s.update(extra)
    return s


def problem(status, slug):
    return {"status": status, "headers": PROBLEM, "body": {"type": "urn:recap:problem:" + slug}}


SERVICE = ["service"]
EXPORTER = ["exporter"]


def case(name, pins, pkg, exporter, steps):
    return {"case": name, "pins": pins, "fixture": pkg.fixture, "record_id": pkg.record_id,
            "exporter": exporter, "steps": steps}


def main():
    write("ingest/exchanges/bodies/empty-object.json", b"{}")

    f01_raw = read("meetings/F-01/expected/manifest.json")
    for name, old, new in [
        # M-12 (fixtures.md §6): recap-enabled=yes sent to ingest.
        ("M-12.not-opted-in.manifest.json", b'"recap-enabled":"true"', b'"recap-enabled":"yes"'),
        # M-08 (fixtures.md §6): a second package for the same record (rebuilt later, new created_at).
        ("M-08.rebuilt.manifest.json", b'"created_at":"2025-10-29T10:10:00Z"', b'"created_at":"2025-10-29T11:10:00Z"'),
    ]:
        if f01_raw.count(old) != 1:
            raise SystemExit("cannot derive " + name)
        write("ingest/exchanges/bodies/" + name, f01_raw.replace(old, new))

    a = Package("F-01", "meetings/F-01/expected/manifest.json")
    b = Package("F-04", "meetings/F-04/expected/manifest.json")
    c = Package("F-06", "meetings/F-06/expected/manifest.json")
    m12 = Package("F-01", "ingest/exchanges/bodies/M-12.not-opted-in.manifest.json")
    m08 = Package("F-01", "ingest/exchanges/bodies/M-08.rebuilt.manifest.json")

    # F-01 plus a byte-identical copy of a slide under a second path (bbb-exporter.md §6.3 step 5). The exporter
    # side makes the copy in its temp raw dir (raw_copies); the manifest is F-01's with that one entry added.
    slide = next(f for f in a.files if f["path"].endswith("/svgs/slide1.svg"))
    copy_path = slide["path"].replace("slide1.svg", "slide1-copy.svg")
    dm = json.loads(f01_raw)
    dm["files"] = sorted(dm["files"] + [dict(slide, path=copy_path)], key=lambda f: f["path"].encode())
    dm["totals"] = {"file_count": len(dm["files"]), "size": sum(f["size"] for f in dm["files"])}
    write("ingest/exchanges/bodies/F-01.duplicate-hash.manifest.json", jcs.canonicalize(dm))
    dup = Package("F-01", "ingest/exchanges/bodies/F-01.duplicate-hash.manifest.json", dup_ok=True)
    acked = {"state": "acked", "reason": None}

    cases = []

    cases.append(case(
        "not-opted-in",
        ["architecture.md §4.1 (not opted in: nothing is stored), §14", "meeting-package.md §7 V-I07", "fixtures.md §6 M-12"],
        m12, None, [
            step(m12.put_manifest(), problem(422, "not-opted-in")),
            step(m12.get(), {"status": 404, "body": None}),
            # The record is not reserved: the opted-in package for it is new.
            step(a.put_manifest(), a.created()),
        ]))

    unauth = b.put_manifest()
    unauth["credential"] = "invalid"
    cases.append(case(
        "unauthorized-401", ["bbb-exporter.md §6.2 row 1 (401), §5.1 (unauthorized)", "README.md HTTP"],
        b, {"state": "failed", "reason": "unauthorized"}, [
            step(unauth, {"status": 401, "body": None}),
        ]))

    cases.append(case(
        "server-mismatch-403", ["bbb-exporter.md §6.2 row 1 (403), §5.1 (unauthorized)", "meeting-package.md §7 V-I03",
                                "fixtures.md §6 M-07"],
        b, {"state": "failed", "reason": "unauthorized"}, [
            step(b.put_manifest(CRED_B), problem(403, "server-mismatch")),
        ]))

    cases.append(case(
        "happy-path", ["bbb-exporter.md §6.2, §6.3", "architecture.md §4.1 (idempotency: new package; atomic commit)"],
        a, acked,
        [step(a.put_manifest(), a.created())]
        + a.uploads(a.files)
        + a.finish()
        + [step(a.get(), a.status("committed", [], ack=True), sides=SERVICE)]))

    # The commit response is lost: the exporter's fake server drops it (the service side still checks it).
    # The re-check then says committed, and a second commit returns the identical ack.
    lost = a.ack()
    lost["fault"] = "drop"
    cases.append(case(
        "lost-commit-response",
        ["bbb-exporter.md §6.3 step 3, §6.4, §6.5", "architecture.md §4.1 (idempotency: already committed → identical ack)"],
        a, acked,
        [step(a.put_manifest(), a.created())]
        + a.uploads(a.files)
        + [step(a.put_manifest(), a.status("complete", [])),
           step(a.commit(), lost),
           step(a.put_manifest(), a.status("committed", []), min_delay_s=24),
           step(a.commit(), a.ack()),
           step(a.get(), a.status("committed", [], ack=True), sides=SERVICE)]))

    cases.append(case(
        "record-already-ingested-409",
        ["bbb-exporter.md §6.2 row 1 (409), §5.1 (conflict)", "architecture.md §4.1 (idempotency: other package_id)",
         "meeting-package.md §7 V-I06", "fixtures.md §6 M-08"],
        m08, {"state": "failed", "reason": "conflict"}, [
            step(a.put_manifest(), a.created(), sides=SERVICE),
            step(m08.put_manifest(), problem(409, "record-already-ingested")),
        ]))

    cases.append(case(
        "hash-mismatch", ["bbb-exporter.md §6.2 row 2 (422), §5.1 (rejected)", "meeting-package.md §7 V-I05",
                          "fixtures.md §6 M-01"],
        b, {"state": "failed", "reason": "rejected"}, [
            step(b.put_manifest(), b.created()),
            step(b.put_file(b.files[0], flip_byte=0), problem(422, "file-hash-mismatch")),
            step(b.get(), b.status("receiving", b.files), sides=SERVICE),
        ]))

    interrupted = b.put_file(b.files[1])
    interrupted["fault"] = "abort_body"
    cases.append(case(
        "resume", ["bbb-exporter.md §6.1, §6.3 step 1, §6.5", "architecture.md §4.1 (idempotency: same package, resume)"],
        b, acked,
        [step(b.put_manifest(), b.created())]
        + b.uploads(b.files[:1])
        + [step(interrupted, None)]
        + [step(b.put_manifest(), b.status("receiving", b.files[1:]), min_delay_s=24)]
        + b.uploads(b.files[1:])
        + b.finish()))

    cases.append(case(
        "duplicate-file", ["bbb-exporter.md §6.2 row 2 (200 already there), §6.4"],
        c, acked,
        [step(c.put_manifest(), c.created())]
        + c.uploads(c.files[:1])
        + c.uploads(c.files[:1], status=200, sides=SERVICE)
        + c.uploads(c.files[1:])
        + c.finish()))

    # 429 and 503 cannot be provoked from outside a running service; these two play the exporter side only.
    busy = {"status": 429, "headers": {"Retry-After": "40"}, "body": None}
    cases.append(case(
        "retry-after-429", ["bbb-exporter.md §6.5 (at least Retry-After)", "architecture.md §4.1, §7 (ingest limit)"],
        a, acked,
        [step(a.put_manifest(), busy, sides=EXPORTER),
         step(a.put_manifest(), a.created(), sides=EXPORTER, min_delay_s=40)]
        + a.uploads(a.files, sides=EXPORTER)
        + [dict(s, sides=EXPORTER) for s in a.finish()]))

    cases.append(case(
        "unavailable-503", ["bbb-exporter.md §6.5 (transient, backoff 30 s ± 20 %)"],
        a, acked,
        [step(a.put_manifest(), a.created(), sides=EXPORTER),
         step(a.put_file(a.files[0]), {"status": 503, "body": None}, sides=EXPORTER),
         step(a.put_manifest(), a.status("receiving", a.files), sides=EXPORTER, min_delay_s=24)]
        + a.uploads(a.files, sides=EXPORTER)
        + [dict(s, sides=EXPORTER) for s in a.finish()]))

    dup_case = case(
        "duplicate-hash", ["bbb-exporter.md §6.3 step 5 (same hash under several paths: uploaded once)"],
        dup, acked,
        [step(dup.put_manifest(), dup.created())]
        + dup.uploads(dup.files)
        + dup.finish())
    dup_case["raw_copies"] = [{"from": slide["path"], "to": copy_path}]
    cases.append(dup_case)

    for i, cs in enumerate(cases, 1):
        name = "%02d-%s.json" % (i, cs["case"])
        write("ingest/exchanges/" + name, (json.dumps(cs, indent=2, ensure_ascii=False) + "\n").encode("utf-8"))
        print("wrote testdata/ingest/exchanges/" + name)


if __name__ == "__main__":
    main()
