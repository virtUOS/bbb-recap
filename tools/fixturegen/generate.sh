#!/usr/bin/env bash
# Regenerate all synthetic fixtures and goldens (docs/reference/fixtures.md). Called by `make fixtures`.
# Needs espeak-ng, ffmpeg (libopus, libvpx) and python3 with jsonschema; works offline.
set -euo pipefail
cd "$(dirname "$0")/../.."
python3 -B -I tools/fixturegen/fixturegen.py
python3 -B -I tools/ref/make_goldens.py
# jsonschema may live in the user site-packages, which -I would hide; -P still keeps the cwd off sys.path.
python3 -B -P tools/ref/validate_manifests.py
