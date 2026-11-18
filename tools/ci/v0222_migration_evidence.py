"""从本次真实 native 收据汇总 R 的 executed-test-evidence；不是人工 bool 审批。"""
import argparse
import importlib.util
import json
from pathlib import Path
import sys

_spec = importlib.util.spec_from_file_location('v0222_evidence', Path(__file__).with_name('v0222_evidence.py'))
evidence = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(evidence)
NAMES = ('old_d78_new_schema', 'old_new_rollup_interleaving',
         'failed_candidate_old_restart', 'invalid_index_recovery')


def summarize(root, artifacts, context):
    source, references = context['source_commit'], {}
    def read(name):
        path = artifacts / name
        evidence.require(path.is_file() and not path.is_symlink(), 'missing/symlink native receipt')
        raw = path.read_bytes()
        references[name] = evidence.sha(raw)
        value = json.loads(raw)
        evidence.require(value.get('verified') is True and value.get('source_commit') == source and
            value.get('github_sha') == source and value.get('runner_version') == context['runner_version'],
            'native receipt failed or source/version drift')
        return value
    old = read('old-app-probe.json')
    index = read('invalid-index-recovery.json')
    rollup = read('rollup-execution.json')
    evidence.require(old.get('native_execution') is True and rollup.get('native_execution') is True and
        index.get('native_execution') is True and old.get('exit_code') == index.get('exit_code') == rollup.get('exit_code') == 0,
        'not a successful native execution')
    for native in (old, index, rollup):
        for name, digest in context['inputs'].items():
            evidence.require(native.get('inputs', {}).get(name) == digest, 'native tool/workflow input drift')
    evidence.require(set(old['stages']) == set('ABCDEF') and all(
        item.get('executed') is True and item.get('verified') is True for item in old['stages'].values()) and
        old.get('process_E_executed') is True and old.get('owned_disposable_cleanup_verified') is True,
        'full process A-F/cleanup incomplete')
    evidence.require(index.get('recovery_tested') is True and index['invalid_before_retry']['valid'] is False
        and index['valid_after_retry']['valid'] is True and index['invalid_before_retry']['oid'] != index['valid_after_retry']['oid']
        and index['cancellation_exit_code'] != 0 and index['old_ledger_preserved'] is True and index['atlas_preserved'] is True,
        'real invalid-index recovery absent')
    commands = rollup['commands']
    evidence.require([item['stage'] for item in commands] == ['red', 'green', 'rollup-race'] and
        all(item.get('verified') is True for item in commands), 'rollup three-stage gate incomplete')
    for run in commands:
        for path_key, hash_key in [('events_path', 'events_sha256'), ('stderr_path', 'stderr_sha256')]:
            path = artifacts / run[path_key]
            evidence.require(path.name == run[path_key] and path.is_file() and not path.is_symlink() and
                             evidence.sha(path.read_bytes()) == run[hash_key], 'raw Go evidence byte drift')
            references[path.name] = run[hash_key]
        checked = read('rollup-' + run['stage'] + '-receipt.json')
        evidence.require(checked['execution']['command'] == run['command'] and
                         checked['target_counts'] == run['target_counts'], 'rollup receipt/command drift')
        counts = checked['target_counts']
        expected_fail = run['stage'] == 'red'
        evidence.require(counts['skip'] == 0 and counts['run'] >= (2 if expected_fail else 10) and
                         counts['fail'] == (2 if expected_fail else 0) and
                         run['exit_code'] == (1 if expected_fail else 0), 'invalid RED/GREEN/race counters')
    cases = [
        {'name': 'full-d78-normal-bootstrap-new-schema-and-two-cycles', 'covers': ['old_d78_new_schema']},
        {'name': 'real-rollup-interleaving-green-and-race-with-genuine-red-control', 'covers': ['old_new_rollup_interleaving']},
        {'name': 'failed-candidate-persisted-registration-old-restart-two-cycles-new-recovery', 'covers': ['failed_candidate_old_restart']},
        {'name': 'official-249-original-runner-cancel-invalid-index-and-full-app-retry', 'covers': ['invalid_index_recovery']},
    ]
    for case in cases:
        case.update(status='passed', exit_code=0)
    return {**context, 'kind': 'executed-test-evidence', 'verified': True, 'exit_code': 0,
            'command': 'python3 -I -B tools/ci/v0222_migration_evidence.py --artifacts v0222-ci-evidence',
            'case_counters': {'executed': len(cases), 'passed': len(cases), 'failed': 0, 'skipped': 0,
                             **{name: 1 for name in NAMES}}, 'cases': cases,
            'source_evidence_sha256': references,
            'source_execution_commands': {'old_app': old['commands'], 'rollup': commands, 'invalid_index': index['commands']},
            'negative_control_note': 'RED has two intended safety-assertion failures; not counted as failed acceptance cases'}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--artifacts', required=True)
    args = parser.parse_args()
    root, artifacts = Path(__file__).resolve().parents[2], Path(args.artifacts).resolve()
    result = {'kind': 'executed-test-evidence', 'verified': False, 'exit_code': 1}
    try:
        context = evidence.context(root, [*Path(__file__).parent.glob('v0222_*'),
                                       root / '.github/workflows/backend-ci.yml'])
        result = summarize(root, artifacts, context)
    except Exception as error:
        result.update(verified=False, exit_code=1, failure_class=type(error).__name__)
        print('Migration evidence incomplete; no successful acceptance claim', file=sys.stderr)
    evidence.save(artifacts / 'migration-compatibility-evidence.json', result)
    return result['exit_code']


if __name__ == '__main__':
    sys.exit(main())
