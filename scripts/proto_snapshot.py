#!/usr/bin/env python3
"""Explicit pinned contract sync, or offline integrity verification."""
import base64
import hashlib
import json
from pathlib import Path
import subprocess
import sys

root = Path(__file__).resolve().parents[1]
source = json.loads((root / 'proto/source.json').read_text())
path = root / 'proto/whisper.proto'
if sys.argv[1:] == ['sync']:
    result = subprocess.check_output([
        'gh', 'api', f"repos/{source['repository']}/contents/{source['path']}?ref={source['revision']}",
        '--jq', '.content',
    ])
    content = base64.b64decode(result)
    if hashlib.sha256(content).hexdigest() != source['sha256']:
        raise SystemExit('Canonical snapshot checksum mismatch')
    path.write_bytes(content)
elif sys.argv[1:] != ['check']:
    raise SystemExit('Usage: proto_snapshot.py check|sync')
if hashlib.sha256(path.read_bytes()).hexdigest() != source['sha256']:
    raise SystemExit('Snapshot differs from pinned canonical source; update source.json explicitly')
print('Pinned protobuf snapshot verified')
