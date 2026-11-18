"""只在准确 Linux Actions 中执行一条 Go test，保留实际 argv/exit/hash；不把非零当 PASS。"""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys

_spec = importlib.util.spec_from_file_location('v0222_evidence', Path(__file__).with_name('v0222_evidence.py'))
evidence = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(evidence)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--events', required=True)
    parser.add_argument('--stderr', required=True)
    parser.add_argument('--execution', required=True)
    parser.add_argument('command', nargs=argparse.REMAINDER)
    args = parser.parse_args()
    command = args.command[1:] if args.command[:1] == ['--'] else args.command
    root = Path(__file__).resolve().parents[2]
    record = {'verified': False, 'command': command, 'exit_code': 1, 'stage': 'race-contract-command'}
    result = None
    try:
        evidence.require(command[:2] == ['go', 'test'] and '-json' in command and '-race' in command,
                         'capture only accepts JSON race Go tests')
        record.update(evidence.context(root, evidence.race_inputs(root)))
        if command == evidence.cache_manifest.CACHE_COMMAND:
            evidence.cache_manifest.check_sources(root)
        events, stderr = Path(args.events), Path(args.stderr)
        events.parent.mkdir(parents=True, exist_ok=True)
        stderr.parent.mkdir(parents=True, exist_ok=True)
        env = dict(os.environ, CI='true')
        env.pop('SUB2API_TEST_ROLLUP_DISABLE_COMPAT_GUARD', None)
        # 任何capture命令都不能继承optional live开关/key或误入边界child模式；只记隔离事实，不记值。
        for name in ('OPENAI_INPUT_TOKENS_LIVE_TEST', 'OPENAI_API_KEY',
                     'SUB2API_OPENAI_INPUT_TOKENS_BOUNDARY_CHILD'):
            env.pop(name, None)
        record['isolated_live_environment'] = True
        with events.open('wb') as stdout, stderr.open('wb') as errors:
            result = subprocess.run(command, cwd=root / 'backend', env=env,
                                    stdout=stdout, stderr=errors, timeout=1200)
        record.update(exit_code=result.returncode, working_directory='backend',
                      events_sha256=evidence.sha(events.read_bytes()), stderr_sha256=evidence.sha(stderr.read_bytes()))
        # command 完成并不代表 test 验收通过；verified 留 false，交命名用例 gate 验证。
        record['command_completed'] = True
    except Exception as error:
        record['failure_class'] = type(error).__name__
        print('Native test command not completed; evidence is not verified', file=sys.stderr)
    evidence.save(args.execution, record)
    return result.returncode if result else 1


if __name__ == '__main__':
    sys.exit(main())
