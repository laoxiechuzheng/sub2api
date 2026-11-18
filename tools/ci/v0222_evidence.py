"""本轮 CI 公共收据：精确源码、tracked 输入、命令与计数，不读取配置/凭据。"""
import datetime as dt
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import platform
import subprocess
import sys

VERSION = 'v0222-ci-v8.1'

_cache_spec = importlib.util.spec_from_file_location('v0222_cache_manifest', Path(__file__).with_name('v0222_cache_manifest.py'))
cache_manifest = importlib.util.module_from_spec(_cache_spec)
_cache_spec.loader.exec_module(cache_manifest)


def require(condition, message):
    if not condition:
        raise ValueError(message)


def sha(data):
    return hashlib.sha256(data).hexdigest()


def git(root, *args):
    return subprocess.check_output(['git', *args], cwd=root, timeout=30)


def tracked_inputs(root, paths):
    """diff quiet 不涵盖 untracked；逐项证明 index、HEAD blob 与工作副本相同。"""
    result = {}
    for path in sorted(set(Path(p).resolve() for p in paths)):
        relative = path.relative_to(root.resolve()).as_posix()
        require(path.is_file() and not path.is_symlink(), 'input missing or symlink: ' + relative)
        git(root, 'ls-files', '--error-unmatch', '--', relative)
        entries = git(root, 'ls-files', '--stage', '--', relative).decode().splitlines()
        require(len(entries) == 1 and entries[0].split(' ', 1)[0] in ('100644', '100755'),
                'input is not a tracked regular file: ' + relative)
        committed = git(root, 'show', 'HEAD:' + relative)
        require(path.read_bytes() == committed, 'input differs from HEAD: ' + relative)
        staged = git(root, 'show', ':' + relative)
        require(staged == committed, 'staged input differs from HEAD: ' + relative)
        result[relative] = sha(committed)
    return result


def race_inputs(root):
    """capture 与命名收据共用显式测试输入；新增PG不进入rollback10 manifest。"""
    repository = root / 'backend/internal/repository'
    return [*Path(__file__).parent.glob('v0222_*'), *cache_manifest.source_paths(root),
            root / '.github/workflows/backend-ci.yml', root / '.github/workflows/publish-personal-ghcr.yml',
            root / 'backend/go.mod', root / 'backend/go.sum',
            *root.glob('backend/internal/service/*v0222*test.go'),
            root / 'backend/internal/service/account_test_service_openai_v8_contract_test.go',
            *[root / 'backend/internal/service' / name for name in (
                'gateway_upstream_response.go', 'gateway_v0222_fastpath_contract_test.go',
                'gateway_stream_reader_lifecycle_test.go', 'openai_gateway_count_tokens_test.go',
                'openai_input_tokens_live_boundary_test.go')],
            *[root / 'backend/internal/service' / name for name in (
                'openai_ws_v2_passthrough_ttft_mode_test.go', 'openai_ws_v2_passthrough_adapter.go',
                'openai_ws_v2/passthrough_relay.go', 'openai_ws_v2/entry.go', 'openai_ws_v2/caddy_adapter.go',
                'openai_gateway_passthrough.go', 'openai_ws_forwarder.go')],
            *[repository / name for name in (
                'usage_log_v0222_persistence_contract_unit_test.go',
                'usage_billing_v0222_apply_contract_unit_test.go',
                'usage_billing_v0222_same_request_integration_test.go',
                'integration_harness_test.go', 'usage_billing_batch_repo_integration_test.go')]]


def context(root, paths, native=True):
    source = git(root, 'rev-parse', 'HEAD').decode().strip()
    if native:
        require(os.environ.get('CI') == 'true' and os.environ.get('GITHUB_ACTIONS') == 'true'
                and sys.platform == 'linux' and source == os.environ.get('GITHUB_SHA'),
                'native evidence requires the exact Linux Actions checkout')
    return {'schema_version': 1, 'runner_version': VERSION, 'source_commit': source,
            'github_sha': os.environ.get('GITHUB_SHA'),
            'run_url': ('https://github.com/' + os.environ['GITHUB_REPOSITORY'] + '/actions/runs/'
                        + os.environ['GITHUB_RUN_ID']) if os.environ.get('GITHUB_RUN_ID') else None,
            'run_attempt': os.environ.get('GITHUB_RUN_ATTEMPT'),
            'python_version': platform.python_version(), 'platform': platform.platform(),
            'native_execution': native, 'inputs': tracked_inputs(root, paths),
            'started_at': dt.datetime.now(dt.timezone.utc).isoformat()}


def save(path, receipt):
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(json.dumps(receipt, indent=2, sort_keys=True) + '\n', encoding='utf-8')


def run_go(root, command, env, output, stage):
    events, stderr = output.with_suffix('.jsonl'), output.with_suffix('.stderr')
    with events.open('wb') as stdout, stderr.open('wb') as errors:
        result = subprocess.run(command, cwd=root / 'backend', env=env,
                                stdout=stdout, stderr=errors, timeout=1200)
    return {'stage': stage, 'command': command, 'working_directory': 'backend',
            'exit_code': result.returncode, 'events_path': events.name,
            'events_sha256': sha(events.read_bytes()), 'stderr_path': stderr.name,
            'stderr_sha256': sha(stderr.read_bytes())}
