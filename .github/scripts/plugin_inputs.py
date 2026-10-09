"""Snapshot the exact sibling SDK used by a local plugin build."""
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys

root = Path.cwd()
sdk = root.parent / 'levis'

def git(args, cwd):
    return subprocess.check_output(['git'] + args, cwd=cwd, text=True, encoding='utf-8').strip()

def record(path, inputs):
    sha = git(['rev-parse', 'HEAD'], path)
    if not re.fullmatch('[a-f0-9]{40}', sha): raise ValueError('full commit SHA required')
    data = {}
    for source in inputs:
        if source.is_file(): data[source.relative_to(path).as_posix()] = hashlib.sha256(source.read_bytes()).hexdigest()
    return {'commit_sha': sha, 'dirty': bool(git(['status','--porcelain','--untracked-files=no'],path)),
            'input_sha256': data, 'input_record_sha256': hashlib.sha256(json.dumps(data,sort_keys=True).encode()).hexdigest()}

sdk_inputs = [sdk/'go.mod',sdk/'go.sum'] + sorted((sdk/'pkg/plugin').rglob('*.go')) + sorted((sdk/'pkg/plugin').rglob('*.proto'))
plugin_inputs = [root/'go.mod',root/'go.sum',root/'frontend/index.html'] + sorted(root.glob('*.go'))
result = {'schema':1,'provenance':'Unsigned local build-input record; a dirty SHA does not identify all working-tree bytes.',
          'sdk_repository':'SakuraOpenSource/levis', 'sdk':record(sdk,sdk_inputs), 'plugin':record(root,plugin_inputs),
          'go_version': subprocess.check_output(['go','version'],text=True).strip()}
expected = os.environ.get('LEVIS_SDK_SHA')
if expected and (not re.fullmatch('[a-f0-9]{40}',expected) or expected != result['sdk']['commit_sha']):
    raise ValueError('LEVIS_SDK_SHA does not match the sibling checkout')
out=Path(sys.argv[1]); out.parent.mkdir(parents=True,exist_ok=True)
if len(sys.argv)>2 and sys.argv[2]=='--verify':
    previous=json.loads(out.read_text(encoding='utf-8'))
    if previous != result: raise ValueError('SDK or plugin inputs changed during the build')
else: out.write_text(json.dumps(result,indent=2)+'\n',encoding='utf-8')
