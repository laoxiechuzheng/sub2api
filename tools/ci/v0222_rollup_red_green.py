"""只在准确 Actions checkout 跑现有 testcontainers RED/GREEN/integration race。"""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import re
import sys


def load(name):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(name + '.py'))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


gate, evidence = load('v0222_test_receipt'), load('v0222_evidence')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--artifacts', required=True)
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[2]
    artifacts = Path(args.artifacts).resolve()
    artifacts.mkdir(parents=True, exist_ok=True)
    receipt = {'verified': False, 'commands': [], 'native_execution': False}
    try:
        paths = [*Path(__file__).parent.glob('v0222_*'),
                 root / '.github/workflows/backend-ci.yml',
                 *root.glob('backend/internal/repository/*rollup*test.go'),
                 root / 'backend/internal/repository/integration_harness_test.go',
                 *root.glob('backend/migrations/*.sql')]
        receipt.update(evidence.context(root, paths))
        declared = set()
        for path in root.glob('backend/internal/repository/*test.go'):
            declared.update(re.findall(r'^func (TestCompositeUsageV0222Rollback\w+)\(',
                                       path.read_text(encoding='utf-8'), re.M))
        expected = gate.ROLLUP_TESTS + gate.ROLLUP_ADDITIONAL_TESTS
        gate.require(declared == set(expected), 'review explicit rollup test manifest before running')
        receipt['required_tests'] = list(expected)
        evidence.save(artifacts / 'rollup-execution.json', receipt)
        env = dict(os.environ, CI='true', GOMAXPROCS='2')
        env.pop('SUB2API_TEST_ROLLUP_DISABLE_COMPAT_GUARD', None)
        env.pop('SUB2API_TEST_POSTGRES_IMAGE', None)
        for mode, names in [('red', gate.RED_TESTS), ('green', expected), ('rollup-race', expected)]:
            child_env = dict(env)
            if mode == 'red':
                child_env['SUB2API_TEST_ROLLUP_DISABLE_COMPAT_GUARD'] = '1'
            command = ['go', 'test', '-json', '-mod=readonly', '-p=2', '-tags=integration']
            if mode == 'rollup-race':
                command.append('-race')
            command += ['./internal/repository', '-run', '^(' + '|'.join(names) + ')$',
                        '-count=1', '-timeout=5m']
            run = evidence.run_go(root, command, child_env, artifacts / ('rollup-' + mode), mode)
            receipt['commands'].append(run)
            evidence.save(artifacts / 'rollup-execution.json', receipt)
            events = [json.loads(line) for line in (artifacts / run['events_path']).read_text(
                encoding='utf-8').splitlines() if line.strip()]
            checked = gate.verify(events, run['exit_code'], mode)
            checked.update({key: receipt[key] for key in ('source_commit', 'github_sha', 'runner_version', 'inputs')})
            checked['execution'] = run
            evidence.save(artifacts / ('rollup-' + mode + '-receipt.json'), checked)
            run['target_counts'] = checked['target_counts']
            run['verified'] = True
        receipt['verified'] = True
    except Exception as error:
        receipt.update(verified=False, failure_class=type(error).__name__)
        if receipt['commands']:
            receipt['commands'][-1]['verified'] = False
        print('Rollup evidence rejected; inspect sanitized stage receipts', file=sys.stderr)
    receipt['exit_code'] = 0 if receipt['verified'] else 1
    evidence.save(artifacts / 'rollup-execution.json', receipt)
    return receipt['exit_code']


if __name__ == '__main__':
    sys.exit(main())
