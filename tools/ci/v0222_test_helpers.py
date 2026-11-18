import importlib.util
import copy
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import MagicMock, patch


def load_helper(name):
    path = Path(__file__).with_name(name + '.py')
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


oci = load_helper('v0222_oci_identity')
gate = load_helper('v0222_test_receipt')
oldboot = load_helper('v0222_old_app_probe')


class OCIIdentityTests(unittest.TestCase):
    def setUp(self):
        self.config = 'sha256:' + 'a' * 64
        self.manifest = 'sha256:' + 'b' * 64
        self.expected = {'source': 'https://github.com/laoxiechuzheng/sub2api',
                         'revision': 'c' * 40, 'version': '0.2.22-custom-fixture'}
        self.local = {**self.expected, 'platform': 'linux/amd64', 'local_id': self.manifest}
        self.metadata = {'containerimage.config.digest': self.config,
                         'containerimage.digest': self.manifest,
                         'containerimage.descriptor': {'mediaType': 'application/vnd.oci.image.manifest.v1+json',
                             'digest': self.manifest, 'size': 501,
                             'annotations': {'config.digest': self.config}}}
        self.raw = json.dumps({'schemaVersion': 2,
            'mediaType': 'application/vnd.docker.distribution.manifest.v2+json',
            'config': {'mediaType': 'application/vnd.docker.container.image.v1+json',
                       'digest': self.config, 'size': 123},
            'layers': [{'digest': 'sha256:' + 'd' * 64, 'size': 456,
                        'mediaType': 'application/vnd.docker.image.rootfs.diff.tar.gzip'}]},
            separators=(',', ':')).encode()
        self.registry_digest = 'sha256:' + hashlib.sha256(self.raw).hexdigest()

    def test_containerd_local_manifest_id_is_not_canonical_config(self):
        self.assertEqual(self.config, oci.local_config_id(self.metadata, self.local, self.expected))

    def test_legacy_local_config_id_remains_canonical(self):
        self.local['local_id'] = self.config
        self.assertEqual(self.config, oci.local_config_id(self.metadata, self.local, self.expected))

    def test_each_required_metadata_key_is_required(self):
        for key in self.metadata:
            with self.subTest(key=key), self.assertRaises(ValueError):
                metadata = dict(self.metadata)
                del metadata[key]
                oci.local_config_id(metadata, self.local, self.expected)

    def test_local_source_platform_revision_version_and_id_reject(self):
        for field, wrong in [('source', 'https://github.com/other/source'),
                             ('revision', 'e' * 40), ('version', 'other'),
                             ('platform', 'linux/arm64'), ('local_id', 'not-a-digest')]:
            with self.subTest(field=field), self.assertRaises(ValueError):
                oci.local_config_id(self.metadata, {**self.local, field: wrong}, self.expected)

    def test_buildkit_index_mismatch_and_bad_annotation_reject(self):
        for field, wrong in [('mediaType', 'application/vnd.oci.image.index.v1+json'),
                             ('digest', 'sha256:' + 'f' * 64), ('size', 0),
                             ('annotations', {'config.digest': 'sha256:' + 'f' * 64})]:
            with self.subTest(field=field), self.assertRaises(ValueError):
                metadata = copy.deepcopy(self.metadata)
                metadata['containerimage.descriptor'][field] = wrong
                oci.local_config_id(metadata, self.local, self.expected)

    def test_hash_authenticated_registry_manifest_to_config(self):
        proof = oci.registry_link(self.raw, self.registry_digest, self.config)
        self.assertEqual(self.config, proof['config_digest'])
        self.assertEqual(len(self.raw), proof['manifest_size'])

    def test_wrong_config_and_wrong_registry_digest_reject(self):
        for manifest, config in [(self.registry_digest, self.manifest),
                                 (self.manifest, self.config)]:
            with self.subTest(manifest=manifest), self.assertRaises(ValueError):
                oci.registry_link(self.raw, manifest, config)

    def test_raw_bytes_must_not_be_reserialized_or_trimmed(self):
        for raw in [self.raw + b'\n', self.raw.replace(b':', b': ', 1), self.raw[:-1]]:
            with self.subTest(raw_length=len(raw)), self.assertRaises(ValueError):
                oci.registry_link(raw, self.registry_digest, self.config)

    def test_registry_index_and_malformed_descriptors_reject(self):
        for change in [{'mediaType': 'application/vnd.oci.image.index.v1+json', 'manifests': []},
                       {'schemaVersion': 1}, {'config': {}}, {'layers': []}]:
            with self.subTest(change=change), self.assertRaises(ValueError):
                parsed = json.loads(self.raw)
                parsed.update(change)
                raw = json.dumps(parsed).encode()
                oci.registry_link(raw, 'sha256:' + hashlib.sha256(raw).hexdigest(), self.config)

    def test_duplicate_json_members_reject(self):
        with self.assertRaises(ValueError):
            oci.strict_json('{"config":{},"config":{}}')

    def test_cli_error_never_echoes_metadata_environment(self):
        with tempfile.TemporaryDirectory() as directory:
            sentinel = 'SYNTHETIC_CONFIG_ENV_MUST_NOT_LEAK'
            env = dict(os.environ, FIXTURE_METADATA=json.dumps({'auth': sentinel}))
            result = subprocess.run([sys.executable, '-I', '-B',
                str(Path(__file__).with_name('v0222_oci_identity.py')), 'local',
                '--metadata-env', 'FIXTURE_METADATA', '--local-id', self.manifest,
                '--platform', 'linux/amd64', '--source', self.expected['source'],
                '--revision', self.expected['revision'], '--version', self.expected['version'],
                '--expected-source', self.expected['source'], '--expected-revision', self.expected['revision'],
                '--expected-version', self.expected['version'], '--binary-sha256', '1' * 64,
                '--receipt', str(Path(directory) / 'receipt.json')],
                env=env, capture_output=True, timeout=10)
            self.assertNotEqual(0, result.returncode)
            self.assertNotIn(sentinel.encode(), result.stdout + result.stderr)
            self.assertFalse((Path(directory) / 'receipt.json').exists())


def events_for(names, outcome='pass', package=gate.REPOSITORY, leaves=0):
    result = []
    for name in names:
        result.append({'Package': package, 'Test': name, 'Action': 'run'})
        for number in range(leaves):
            child = name + '/case' + str(number)
            result.extend([{'Package': package, 'Test': child, 'Action': 'run'},
                           {'Package': package, 'Test': child, 'Action': outcome}])
        if outcome == 'fail':
            result.append({'Package': package, 'Test': name, 'Action': 'output',
                'Output': 'NEGATIVE CONTROL: isolated schema uses official 251 invalidation without 252\n'
                    'Error: Not equal:\nMessages: d78 summary must match live synthetic detail\n'})
        result.append({'Package': package, 'Test': name, 'Action': outcome})
    result.append({'Package': package, 'Action': outcome})
    return result


def contract_events(tops, leaves):
    names = set(tops) | set(leaves)
    return [{'Package': gate.SERVICE, 'Test': name, 'Action': action}
            for name in sorted(names) for action in ('run', 'pass')] + [
            {'Package': gate.SERVICE, 'Action': 'pass'}]


class OCICommandTests(unittest.TestCase):
    setUp = OCIIdentityTests.setUp
    # 保留原全部 OCI 用例；额外通过真实 Python CLI 跑 local→registry。
    def cli_fixture(self, mutation=None, local_id=None, raw=None):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            asset = root / 'dist/assets'
            asset.mkdir(parents=True)
            (asset / 'index-fixture.js').write_bytes(b'fixture-script')
            manifest = root / 'assets.json'
            manifest.write_text(json.dumps({'files': [{'path': '/assets/index-fixture.js',
                'sha256': hashlib.sha256(b'fixture-script').hexdigest(), 'size': 14}]}), encoding='utf-8')
            build, proof, registry = root / 'build.json', root / 'proof.json', root / 'raw.json'
            env = dict(os.environ, FIXTURE_METADATA=json.dumps(self.metadata))
            base = [sys.executable, '-I', '-B', str(Path(__file__).with_name('v0222_oci_identity.py'))]
            result = subprocess.run(base + ['local', '--metadata-env', 'FIXTURE_METADATA', '--local-id', self.manifest,
                '--platform', 'linux/amd64', '--source', self.expected['source'], '--revision', self.expected['revision'],
                '--version', self.expected['version'], '--expected-source', self.expected['source'],
                '--expected-revision', self.expected['revision'], '--expected-version', self.expected['version'],
                '--binary-sha256', '1' * 64, '--receipt', str(build)], env=env, capture_output=True, timeout=10)
            self.assertEqual(0, result.returncode, result.stderr)
            value = json.loads(build.read_text())
            if mutation:
                mutation(value)
                build.write_text(json.dumps(value), encoding='utf-8')
            registry.write_bytes(raw if raw is not None else self.raw)
            result = subprocess.run(base + ['registry', '--metadata-env', 'FIXTURE_METADATA', '--manifest', str(registry),
                '--expected-digest', self.registry_digest, '--build-receipt', str(build), '--receipt', str(proof),
                '--expected-source', self.expected['source'], '--expected-revision', self.expected['revision'],
                '--expected-version', self.expected['version'], '--local-id', local_id or self.manifest,
                '--latest-id', self.manifest, '--binary-sha256', '1' * 64, '--asset-manifest', str(manifest),
                '--asset-manifest-sha256', hashlib.sha256(manifest.read_bytes()).hexdigest(), '--asset-count', '1',
                '--asset-directory', str(root / 'dist')], env=env, capture_output=True, timeout=10)
            if result.returncode:
                self.assertFalse(proof.exists())
            else:
                self.assertTrue(json.loads(proof.read_text())['verified'])
            return result

    def test_cli_local_to_registry_positive(self):
        self.assertEqual(0, self.cli_fixture().returncode)

    def test_cli_local_id_drift_reject(self):
        self.assertNotEqual(0, self.cli_fixture(local_id='sha256:' + 'e' * 64).returncode)

    def test_cli_registry_receipt_field_pollution_reject(self):
        for field, bad in [('source', 'wrong'), ('revision', 'd' * 40), ('version', 'wrong'),
                           ('platform', 'linux/arm64'), ('local_id', 'sha256:' + 'e' * 64),
                           ('config_digest', 'sha256:' + 'e' * 64), ('binary_sha256', '2' * 64),
                           ('registry', {'verified': True}), ('verified', True), ('schema_version', True),
                           ('buildkit_manifest_digest', 'sha256:' + 'e' * 64),
                           ('metadata_sha256', 'f' * 64), ('buildkit_manifest_size', 999),
                           ('buildkit_media_type', 'application/vnd.oci.image.index.v1+json')]:
            with self.subTest(field=field):
                self.assertNotEqual(0, self.cli_fixture(lambda value: value.update({field: bad})).returncode)

    def test_cli_raw_byte_drift_reject(self):
        self.assertNotEqual(0, self.cli_fixture(raw=self.raw + b'\n').returncode)

    def test_cli_missing_receipt_field_reject(self):
        for field in sorted(oci.BUILD_FIELDS):
            with self.subTest(field=field):
                self.assertNotEqual(0, self.cli_fixture(lambda value: value.pop(field)).returncode)

    def test_assets_require_exact_set_hash_count_and_bytes(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'assets').mkdir()
            (root / 'assets/index.js').write_bytes(b'asset')
            path = root / 'manifest.json'
            entry = {'path': '/assets/index.js', 'sha256': hashlib.sha256(b'asset').hexdigest(), 'size': 5}
            path.write_text(json.dumps({'files': [entry]}), encoding='utf-8')
            checksum = hashlib.sha256(path.read_bytes()).hexdigest()
            self.assertEqual(1, oci.assets_link(path, checksum, 1, root)['count'])
            for count, digest in [(0, checksum), (2, checksum), (1, '0' * 64)]:
                with self.subTest(count=count), self.assertRaises(ValueError):
                    oci.assets_link(path, digest, count, root)
            (root / 'assets/index.js').write_bytes(b'wrong')
            with self.assertRaises(ValueError):
                oci.assets_link(path, checksum, 1, root)
            (root / 'assets/index.js').write_bytes(b'asset')
            (root / 'assets/extra.js').write_bytes(b'extra')
            with self.assertRaises(ValueError):
                oci.assets_link(path, checksum, 1, root)
            path.write_text(json.dumps({'files': [entry, entry]}), encoding='utf-8')
            with self.assertRaises(ValueError):
                oci.assets_link(path, hashlib.sha256(path.read_bytes()).hexdigest(), 2, root)


class GoEvidenceTests(unittest.TestCase):
    def test_genuine_red_has_both_target_runs_failures_and_assertion(self):
        receipt = gate.verify(events_for(gate.RED_TESTS, 'fail'), 1, 'red')
        self.assertTrue(receipt['verified'])

    def test_arbitrary_nonzero_and_testmain_failure_are_not_red(self):
        for events in [[], [{'Package': gate.REPOSITORY, 'Action': 'fail'}],
                       [{'Action': 'build-fail'}], events_for(gate.RED_TESTS, 'pass')]:
            with self.subTest(events=events), self.assertRaises(ValueError):
                gate.verify(events, 1, 'red')

    def test_red_requires_unchanged_safety_assertion_not_connection_error(self):
        events = events_for(gate.RED_TESTS, 'fail')
        for event in events:
            if event['Action'] == 'output':
                event['Output'] = 'connect: connection refused'
        with self.assertRaises(ValueError):
            gate.verify(events, 1, 'red')

    def test_red_rejects_one_target_passing_or_missing(self):
        for names in [gate.RED_TESTS[:1], ()]:
            with self.subTest(names=names), self.assertRaises(ValueError):
                gate.verify(events_for(names, 'fail'), 1, 'red')
        events = events_for(gate.RED_TESTS, 'fail')
        events[2]['Action'] = 'pass'
        with self.assertRaises(ValueError):
            gate.verify(events, 1, 'red')

    def test_green_requires_all_ten_no_skip_and_no_negative_flag(self):
        self.assertTrue(gate.verify(events_for(gate.ROLLUP_TESTS), 0, 'green')['verified'])
        for outcome in ['skip', 'fail']:
            with self.subTest(outcome=outcome), self.assertRaises(ValueError):
                gate.verify(events_for(gate.ROLLUP_TESTS, outcome), 0, 'green')
        with self.assertRaises(ValueError):
            gate.verify(events_for(gate.ROLLUP_TESTS[:9]), 0, 'green')

    def test_sse_requires_eight_top_level_and_32_leaf_cases(self):
        good = contract_events(gate.SSE_TESTS, gate.SSE_LEAVES)
        self.assertEqual(32, gate.verify(good, 0, 'sse')['leaf_count'])
        for count in [0, 3, 5]:
            with self.subTest(count=count), self.assertRaises(ValueError):
                gate.verify(events_for(gate.SSE_TESTS, package=gate.SERVICE, leaves=count), 0, 'sse')

    def test_retry_requires_exact_one_top_and_eight_leaves_without_skip(self):
        good = contract_events(gate.RETRY_TESTS, gate.RETRY_LEAVES)
        self.assertEqual(8, gate.verify(good, 0, 'retry')['leaf_count'])
        for bad in (good[1:], contract_events(gate.RETRY_TESTS, gate.RETRY_LEAVES[:-1]),
                    events_for(gate.RETRY_TESTS, package=gate.SERVICE, leaves=8),
                    good + [{'Package': gate.SERVICE, 'Test': gate.RETRY_LEAVES[0], 'Action': 'skip'}],
                    good + [{'Action': 'build-fail'}]):
            with self.subTest(events=len(bad)), self.assertRaises(ValueError):
                gate.verify(bad, 0, 'retry')
        with self.assertRaises(ValueError):
            gate.verify(good, 1, 'retry')

    def test_sse_child_skip_missing_run_or_unrelated_failure_reject(self):
        good = contract_events(gate.SSE_TESTS, gate.SSE_LEAVES)
        for event in [{'Package': gate.SERVICE, 'Test': gate.SSE_TESTS[0] + '/case0', 'Action': 'skip'},
                      {'Package': 'other', 'Action': 'fail'}]:
            with self.subTest(event=event), self.assertRaises(ValueError):
                gate.verify(good + [event], 0, 'sse')
        with self.assertRaises(ValueError):
            gate.verify(good[1:], 0, 'sse')


class BoundaryEvidenceTests(unittest.TestCase):
    # 独立fixture合同，不从gate的observed或expected反向生成场景数值。
    lifecycle_cases = ('blocked_Read_join', 'send_blocked_cancel', 'budget_acquire_cancel',
                       'first_visible_barrier_cancel', 'upstream_error', 'natural_EOF')
    live_cases = {
        'without_opt_in': (0, 'SKIP', None, 0), 'zero_opt_in': (0, 'SKIP', None, 0),
        'true_opt_in': (0, 'SKIP', None, 0), 'zero_padded_opt_in': (0, 'SKIP', None, 0),
        'spaced_opt_in': (0, 'SKIP', None, 0), 'without_key': (0, 'SKIP', None, 0),
        'short_placeholder_key': (0, 'SKIP', None, 0), 'explicit_opt_in': (3, 'PASS', 'PASS', 0),
        'out_of_tolerance': (3, 'FAIL', 'FAIL', 1), 'unauthorized': (3, 'PASS', 'SKIP', 0),
        'forbidden': (3, 'PASS', 'SKIP', 0), 'network_error': (3, 'PASS', 'SKIP', 0),
        'server_error': (3, 'FAIL', 'FAIL', 1), 'unexpected_redirect': (3, 'FAIL', 'FAIL', 1),
        'missing_count': (3, 'FAIL', 'FAIL', 1), 'read_error': (3, 'FAIL', 'FAIL', 1)}
    source_names = ('gateway_upstream_response.go', 'gateway_v0222_fastpath_contract_test.go',
                    'gateway_stream_reader_lifecycle_test.go', 'openai_gateway_count_tokens_test.go',
                    'openai_input_tokens_live_boundary_test.go')

    def events(self, mode):
        live = mode == 'live-boundary'
        top = 'TestOpenAIInputTokensLiveBoundary' if live else 'TestSSEStreamReaderLifecycle'
        filename = 'openai_input_tokens_live_boundary_test.go' if live else 'gateway_stream_reader_lifecycle_test.go'
        marker = 'live_boundary_evidence ' if live else 'SSE reader lifecycle snapshot: '
        events = [{'Package': gate.SERVICE, 'Action': 'start'},
                  {'Package': gate.SERVICE, 'Test': top, 'Action': 'run'}]
        for case in self.live_cases if live else self.lifecycle_cases:
            if live:
                calls, entry, leaf, child_exit = self.live_cases[case]
                value = {'scenario': case, 'http_calls': calls, 'entry_status': entry,
                         'leaf_status': {name: leaf for name in ('simple_user_text', 'system_plus_tool', 'multi_turn_text')} if leaf else {},
                         'child_exit': child_exit, 'sensitive_output': False}
            else:
                value = {'body_closed': True, 'reader_completed': True, 'active_reads': 0,
                         'budget_known': True, 'queued_bytes': 0}
            name = top + '/' + case
            events += [{'Package': gate.SERVICE, 'Test': name, 'Action': 'run'},
                       {'Package': gate.SERVICE, 'Test': name, 'Action': 'output',
                        'Output': '    ' + filename + ':105: ' + marker + json.dumps(value) + '\n'},
                       {'Package': gate.SERVICE, 'Test': name, 'Action': 'pass'}]
        return events + [{'Package': gate.SERVICE, 'Test': top, 'Action': 'pass'},
                         {'Package': gate.SERVICE, 'Action': 'pass'}]

    def execution(self, mode, events_sha='c' * 64, stderr=b''):
        context = {'source_commit': 'a' * 40, 'github_sha': 'a' * 40, 'runner_version': 'v0222-ci-v8.1',
                   'inputs': {'backend/internal/service/' + name: 'b' * 64 for name in self.source_names},
                   'native_execution': False}
        command = gate.LIVE_BOUNDARY_COMMAND if mode == 'live-boundary' else gate.SSE_LIFECYCLE_COMMAND
        return context, {**context, 'command': list(command), 'working_directory': 'backend', 'exit_code': 0,
            'command_completed': True, 'isolated_live_environment': True,
            'events_sha256': events_sha, 'stderr_sha256': hashlib.sha256(stderr).hexdigest()}

    def test_lifecycle_exact_one_top_six_released_leaf_snapshots(self):
        value = gate.verify(self.events('sse-lifecycle'), 0, 'sse-lifecycle')
        self.assertTrue(value['verified'])
        self.assertEqual((1, 6, 0), (value['top_level_count'], value['leaf_count'], value['skip_count']))
        self.assertEqual({'run': 7, 'pass': 7, 'fail': 0, 'skip': 0}, value['target_counts'])
        self.assertEqual(6, len(value['same_test_evidence']))
        self.assertEqual(32, len(gate.SSE_LEAVES))
        self.assertEqual(8, len(gate.RETRY_LEAVES))

    def test_live_exact_one_top_sixteen_outer_pass_child_fail_skip_are_only_data(self):
        value = gate.verify(self.events('live-boundary'), 0, 'live-boundary')
        self.assertEqual((1, 16, 0), (value['top_level_count'], value['leaf_count'], value['skip_count']))
        self.assertEqual({'run': 17, 'pass': 17, 'fail': 0, 'skip': 0}, value['target_counts'])
        self.assertEqual(set(self.live_cases), {name.split('/', 1)[1] for name in value['same_test_evidence']})
        self.assertEqual({'pass'}, set(value['leaf_outcomes'].values()))
        self.assertEqual(dict(self.live_cases), gate.LIVE_SCENARIOS)
        self.assertEqual(set(self.lifecycle_cases), {name.split('/', 1)[1] for name in gate.LIFECYCLE_LEAVES})

    def test_both_missing_any_run_terminal_or_same_test_output_reject(self):
        for mode in ('sse-lifecycle', 'live-boundary'):
            good = self.events(mode)
            for index, event in enumerate(good):
                if event['Action'] != 'start':
                    with self.subTest(mode=mode, index=index), self.assertRaises(ValueError):
                        gate.verify(good[:index] + good[index+1:], 0, mode)

    def test_both_duplicate_run_terminal_output_and_package_terminal_reject(self):
        for mode in ('sse-lifecycle', 'live-boundary'):
            good = self.events(mode)
            for index, event in enumerate(good):
                with self.subTest(mode=mode, action=event['Action'], index=index), self.assertRaises(ValueError):
                    gate.verify(good[:index] + [event] + good[index:], 0, mode)

    def test_both_wrong_package_wrong_test_and_unreviewed_descendant_reject(self):
        for mode in ('sse-lifecycle', 'live-boundary'):
            good = self.events(mode)
            for index, event in enumerate(good):
                if not event.get('Test'):
                    continue
                for field, wrong in (('Package', 'wrong-package'), ('Test', event['Test'] + '/unreviewed')):
                    bad = copy.deepcopy(good)
                    bad[index][field] = wrong
                    with self.subTest(mode=mode, index=index, field=field), self.assertRaises(ValueError):
                        gate.verify(bad, 0, mode)

    def test_both_zero_match_nonzero_skip_fail_buildfail_or_unrelated_failure_reject(self):
        for mode in ('sse-lifecycle', 'live-boundary'):
            good = self.events(mode)
            for sample in ([], [{'Package': gate.SERVICE, 'Action': 'pass'}], [None],
                           good + [{'Action': 'build-fail'}], good + [{'Package': 'another', 'Action': 'fail'}],
                           good + [{'Package': gate.SERVICE, 'Action': 'skip'}]):
                with self.subTest(mode=mode, sample_size=len(sample)), self.assertRaises(ValueError):
                    gate.verify(sample, 0, mode)
            for action in ('skip', 'fail'):
                for index, event in enumerate(good):
                    if event['Action'] == 'pass':
                        bad = copy.deepcopy(good)
                        bad[index]['Action'] = action
                        with self.subTest(mode=mode, index=index, action=action), self.assertRaises(ValueError):
                            gate.verify(bad, 0, mode)
            for code in (1, -1, False):
                with self.subTest(mode=mode, code=code), self.assertRaises(ValueError):
                    gate.verify(good, code, mode)

    def test_both_evidence_must_be_inside_its_leaf_run_and_before_terminal(self):
        for mode in ('sse-lifecycle', 'live-boundary'):
            good = self.events(mode)
            index = next(i for i, event in enumerate(good) if event['Action'] == 'output')
            for destination in (0, index - 1, index + 2, len(good)):
                bad = copy.deepcopy(good)
                event = bad.pop(index)
                bad.insert(destination, event)
                with self.subTest(mode=mode, destination=destination), self.assertRaises(ValueError):
                    gate.verify(bad, 0, mode)
            bad = copy.deepcopy(good)
            bad[1], bad[2] = bad[2], bad[1]
            with self.subTest(mode=mode, parent_order=True), self.assertRaises(ValueError):
                gate.verify(bad, 0, mode)

    def test_live_same_scenario_binding_rejects_sixteen_arbitrary_json_or_swapped_outputs(self):
        good = self.events('live-boundary')
        outputs = [index for index, event in enumerate(good) if event['Action'] == 'output']
        for index in outputs:
            for name in ('TestOpenAIInputTokensLiveBoundary', 'TestOther/synthetic',
                         'TestOpenAIInputTokensLiveBoundary/explicit_opt_in'):
                if name == good[index]['Test']:
                    continue
                bad = copy.deepcopy(good)
                bad[index]['Test'] = name
                with self.subTest(index=index, name=name), self.assertRaises(ValueError):
                    gate.verify(bad, 0, 'live-boundary')
        bad = copy.deepcopy(good)
        first, second = outputs[:2]
        bad[first]['Output'], bad[second]['Output'] = bad[second]['Output'], bad[first]['Output']
        with self.assertRaises(ValueError):
            gate.verify(bad, 0, 'live-boundary')

    def test_live_every_scenario_http_entry_original_leaf_childexit_and_sensitive_false_are_fixed(self):
        good = self.events('live-boundary')
        mutations = [('scenario', 'not_the_running_scenario'), ('http_calls', 99), ('entry_status', 'UNKNOWN'),
                     ('leaf_status', {'unexpected': 'PASS'}), ('child_exit', 7), ('sensitive_output', True)]
        for index, event in enumerate(good):
            if event['Action'] != 'output':
                continue
            prefix, payload = event['Output'].split('live_boundary_evidence ', 1)
            for field, wrong in mutations:
                value = json.loads(payload)
                value[field] = wrong
                bad = copy.deepcopy(good)
                bad[index]['Output'] = prefix + 'live_boundary_evidence ' + json.dumps(value) + '\n'
                with self.subTest(test=event['Test'], field=field), self.assertRaises(ValueError):
                    gate.verify(bad, 0, 'live-boundary')

    def test_live_schema_exact_keys_types_and_original_three_leaf_map_reject(self):
        good = self.events('live-boundary')
        for index, event in enumerate(good):
            if event['Action'] != 'output':
                continue
            prefix, payload = event['Output'].split('live_boundary_evidence ', 1)
            original = json.loads(payload)
            alternatives = [{**original, 'extra': 'pollution'}]
            alternatives += [{key: value for key, value in original.items() if key != missing} for missing in original]
            alternatives += [{**original, key: wrong} for key, wrong in (
                ('scenario', None), ('http_calls', False), ('http_calls', float(original['http_calls'])),
                ('child_exit', bool(original['child_exit'])), ('child_exit', str(original['child_exit'])),
                ('entry_status', []), ('sensitive_output', 0), ('sensitive_output', 'false'), ('leaf_status', []))]
            if original['leaf_status']:
                for leaf in original['leaf_status']:
                    leaves = dict(original['leaf_status'])
                    del leaves[leaf]
                    alternatives.append({**original, 'leaf_status': leaves})
            for alternative in alternatives:
                bad = copy.deepcopy(good)
                bad[index]['Output'] = prefix + 'live_boundary_evidence ' + json.dumps(alternative) + '\n'
                with self.subTest(test=event['Test'], alternative=alternative), self.assertRaises(ValueError):
                    gate.verify(bad, 0, 'live-boundary')

    def test_both_malformed_duplicate_member_multiline_or_wrong_source_evidence_reject(self):
        for mode in ('sse-lifecycle', 'live-boundary'):
            good = self.events(mode)
            index = next(i for i, event in enumerate(good) if event['Action'] == 'output')
            text = good[index]['Output']
            field = 'scenario' if mode == 'live-boundary' else 'body_closed'
            raw = json.loads(text[text.index('{'):])
            duplicate = text[:text.index('{') + 1] + json.dumps(field) + ':' + json.dumps(raw[field]) + ',' + text[text.index('{') + 1:]
            for wrong in (text[:-3], duplicate, text + text, text.replace('.go:', '.txt:'),
                          text.replace('    ', 'untrusted-prefix ', 1), text.replace('{', '[', 1)):
                bad = copy.deepcopy(good)
                bad[index]['Output'] = wrong
                with self.subTest(mode=mode, wrong=wrong[:60]), self.assertRaises(ValueError):
                    gate.verify(bad, 0, mode)
        with self.assertRaises(ValueError):
            gate.strict_json('{"leaf_status":{"x":"PASS","x":"PASS"}}')

    def test_lifecycle_every_snapshot_field_type_and_resource_leak_reject(self):
        good = self.events('sse-lifecycle')
        for index, event in enumerate(good):
            if event['Action'] != 'output':
                continue
            prefix, payload = event['Output'].split('SSE reader lifecycle snapshot: ', 1)
            original = json.loads(payload)
            alternatives = [{**original, 'extra': 0}]
            for field, value in original.items():
                alternatives += [{key: item for key, item in original.items() if key != field},
                                 {**original, field: (False if type(value) is bool else 168)},
                                 {**original, field: (1 if type(value) is bool else False)}]
            for alternative in alternatives:
                bad = copy.deepcopy(good)
                bad[index]['Output'] = prefix + 'SSE reader lifecycle snapshot: ' + json.dumps(alternative) + '\n'
                with self.subTest(test=event['Test'], alternative=alternative), self.assertRaises(ValueError):
                    gate.verify(bad, 0, 'sse-lifecycle')

    def test_live_sensitive_marker_credentials_or_child_raw_output_reject(self):
        good = self.events('live-boundary')
        for marker in ('input-tokens-boundary-sensitive-marker', 'INPUT_TOKENS_BOUNDARY_HTTP_CALLS=3',
                       'Authorization: synthetic', 'Bearer synthetic', 'OPENAI_API_KEY=synthetic'):
            bad = good[:-1] + [{'Package': gate.SERVICE, 'Action': 'output', 'Output': marker}] + good[-1:]
            with self.subTest(marker=marker), self.assertRaises(ValueError):
                gate.verify(bad, 0, 'live-boundary')

    def test_live_rejects_real_optional_entry_and_lifecycle_keeps_other_contracts_separate(self):
        good = self.events('live-boundary')
        name = 'TestEstimateOpenAIInputTokens_CompareWithOpenAIAPI'
        extra = [{'Package': gate.SERVICE, 'Test': name, 'Action': action} for action in ('run', 'pass')]
        with self.assertRaises(ValueError):
            gate.verify(good[:-1] + extra + good[-1:], 0, 'live-boundary')
        lifecycle = self.events('sse-lifecycle')
        original = contract_events(gate.SSE_TESTS, gate.SSE_LEAVES)[:-1]
        original += contract_events(gate.RETRY_TESTS, gate.RETRY_LEAVES)[:-1]
        mixed = lifecycle[:-1] + original + lifecycle[-1:]
        self.assertEqual(6, gate.verify(mixed, 0, 'sse-lifecycle')['leaf_count'])
        self.assertEqual(32, gate.verify(mixed, 0, 'sse')['leaf_count'])
        self.assertEqual(8, gate.verify(mixed, 0, 'retry')['leaf_count'])

    def test_lifecycle_wide_raw_preserves_unrelated_optional_skip_but_not_target_skip(self):
        good = self.events('sse-lifecycle')
        extra = [{'Package': gate.SERVICE, 'Test': 'TestUnrelatedOptional', 'Action': action}
                 for action in ('run', 'skip')]
        self.assertEqual(6, gate.verify(good[:-1] + extra + good[-1:], 0, 'sse-lifecycle')['leaf_count'])
        live = self.events('live-boundary')
        with self.assertRaises(ValueError):
            gate.verify(live[:-1] + extra + live[-1:], 0, 'live-boundary')
        bad = copy.deepcopy(good)
        index = next(i for i, event in enumerate(bad) if event.get('Test', '').endswith('/natural_EOF')
                     and event['Action'] == 'pass')
        bad[index]['Action'] = 'skip'
        with self.assertRaises(ValueError):
            gate.verify(bad, 0, 'sse-lifecycle')

    def test_both_execution_exact_argv_source_inputs_raw_directexit_cwd_and_isolation(self):
        for mode in ('sse-lifecycle', 'live-boundary'):
            context, execution = self.execution(mode)
            gate.validate_execution(execution, context, 0, 'c' * 64, mode)
            edits = [lambda value: value.update(source_commit='d' * 40),
                     lambda value: value.update(github_sha='d' * 40), lambda value: value.update(inputs={}),
                     lambda value: value.update(events_sha256='d' * 64), lambda value: value.update(exit_code=1),
                     lambda value: value.update(exit_code=False), lambda value: value.update(working_directory='.'),
                     lambda value: value.update(command_completed=False), lambda value: value.update(isolated_live_environment=False),
                     lambda value: value['command'].remove('-race'), lambda value: value['command'].remove('-json'),
                     lambda value: value['command'].__setitem__(6, '-tags=integration'),
                     lambda value: value['command'].__setitem__(7, './internal/repository'),
                     lambda value: value['command'].__setitem__(-2, '-count=2'),
                     lambda value: value['command'].__setitem__(-1, '-timeout=1h'),
                     lambda value: value['command'].__setitem__(-3, '^TestEstimateOpenAIInputTokens_CompareWithOpenAIAPI$')]
            for index, edit in enumerate(edits):
                bad = copy.deepcopy(execution)
                edit(bad)
                with self.subTest(mode=mode, index=index), self.assertRaises(ValueError):
                    gate.validate_execution(bad, context, 0, 'c' * 64, mode)
            for source in self.source_names:
                wrong_context = copy.deepcopy(context)
                wrong_context['inputs'].pop('backend/internal/service/' + source)
                bad = {**execution, 'inputs': wrong_context['inputs']}
                with self.subTest(mode=mode, missing=source), self.assertRaises(ValueError):
                    gate.validate_execution(bad, wrong_context, 0, 'c' * 64, mode)

    def test_lifecycle_publish_exact_thirteen_package_command_and_no_missing_old_branch(self):
        context, execution = self.execution('sse-lifecycle')
        execution['command'] = list(gate.PUBLISH_LIFECYCLE_COMMAND)
        gate.validate_execution(execution, context, 0, 'c' * 64, 'sse-lifecycle')
        self.assertEqual(13, len(gate.PUBLISH_RACE_PACKAGES))
        for edit in (lambda value: value['command'].remove('./internal/handler/admin'),
                     lambda value: value['command'].__setitem__(-3, gate.PUBLISH_RACE_SELECTOR.replace('Composite|', '', 1))):
            bad = copy.deepcopy(execution)
            edit(bad)
            with self.assertRaises(ValueError):
                gate.validate_execution(bad, context, 0, 'c' * 64, 'sse-lifecycle')

    def test_boundary_stderr_hash_and_no_misplaced_or_sensitive_live_evidence(self):
        for mode in ('sse-lifecycle', 'live-boundary'):
            _, execution = self.execution(mode)
            gate.validate_boundary_stderr(execution, b'', mode)
            with self.assertRaises(ValueError):
                gate.validate_boundary_stderr(execution, b'raw-drift', mode)
        for raw in (b'input-tokens-boundary-sensitive-marker', b'live_boundary_evidence {}', b'Authorization: synthetic'):
            _, execution = self.execution('live-boundary', stderr=raw)
            with self.assertRaises(ValueError):
                gate.validate_boundary_stderr(execution, raw, 'live-boundary')

    def test_both_main_write_fixed_named_receipts_and_reject_raw_stderr_directexit_drift(self):
        for mode in ('sse-lifecycle', 'live-boundary'):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                events, errors = root / 'events.jsonl', root / 'errors.log'
                record, receipt = root / 'execution.json', root / 'receipt.json'
                raw = ('\n'.join(json.dumps(event) for event in self.events(mode)) + '\n').encode()
                events.write_bytes(raw)
                errors.write_bytes(b'')
                context, execution = self.execution(mode, hashlib.sha256(raw).hexdigest())
                record.write_text(json.dumps(execution), encoding='utf-8')
                argv = ['v0222_test_receipt.py', '--mode', mode, '--events', str(events), '--stderr', str(errors),
                        '--exit-code', '0', '--execution', str(record), '--receipt', str(receipt)]
                with patch.object(sys, 'argv', argv), patch.object(gate.evidence, 'context', return_value=context):
                    gate.main()
                value = json.loads(receipt.read_text())
                self.assertTrue(value['verified'])
                self.assertFalse(value['native_execution'])
                self.assertEqual(16 if mode == 'live-boundary' else 6, value['leaf_count'])
                self.assertEqual(hashlib.sha256(record.read_bytes()).hexdigest(), value['execution_sha256'])
                for mutation in ('raw', 'stderr', 'directexit', 'missing-stderr', 'duplicate-json'):
                    events.write_bytes(raw)
                    errors.write_bytes(b'')
                    arguments = list(argv)
                    current = copy.deepcopy(execution)
                    if mutation == 'raw':
                        events.write_bytes(raw + b'\n')
                    elif mutation == 'stderr':
                        errors.write_bytes(b'changed')
                    elif mutation == 'directexit':
                        current['exit_code'] = 1
                    elif mutation == 'missing-stderr':
                        index = arguments.index('--stderr')
                        del arguments[index:index+2]
                    else:
                        changed = raw.replace(b'"Action": "start"', b'"Action": "start", "Action": "start"', 1)
                        events.write_bytes(changed)
                        current['events_sha256'] = hashlib.sha256(changed).hexdigest()
                    record.write_text(json.dumps(current), encoding='utf-8')
                    with patch.object(sys, 'argv', arguments), patch.object(gate.evidence, 'context', return_value=context), self.assertRaises(SystemExit) as rejected:
                        gate.main()
                    self.assertEqual(1, rejected.exception.code)
                    self.assertFalse(json.loads(receipt.read_text())['verified'])

    def test_capture_sanitizes_live_optin_key_child_marker_and_binds_stderr_without_running_go(self):
        capture = load_helper('v0222_capture')
        context, _ = self.execution('live-boundary')
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            argv = ['v0222_capture.py', '--events', str(root / 'events.jsonl'), '--stderr', str(root / 'stderr.log'),
                    '--execution', str(root / 'execution.json'), '--', *gate.LIVE_BOUNDARY_COMMAND]
            observed = {}
            def fake_run(command, **kwargs):
                observed.update(kwargs)
                return subprocess.CompletedProcess(command, 0)
            environment = {name: 'synthetic-fixture-only' for name in (
                'OPENAI_INPUT_TOKENS_LIVE_TEST', 'OPENAI_API_KEY', 'SUB2API_OPENAI_INPUT_TOKENS_BOUNDARY_CHILD',
                'SUB2API_TEST_ROLLUP_DISABLE_COMPAT_GUARD')}
            with patch.object(sys, 'argv', argv), patch.dict(os.environ, environment), \
                    patch.object(capture.evidence, 'context', return_value=context), \
                    patch.object(capture.subprocess, 'run', side_effect=fake_run) as run:
                self.assertEqual(0, capture.main())
            run.assert_called_once()
            self.assertTrue(all(name not in observed['env'] for name in environment))
            self.assertEqual('true', observed['env']['CI'])
            value = json.loads((root / 'execution.json').read_text())
            self.assertTrue(value['isolated_live_environment'])
            self.assertFalse(value['verified'])
            self.assertEqual(0, value['exit_code'])
            self.assertEqual(hashlib.sha256(b'').hexdigest(), value['stderr_sha256'])
            self.assertNotIn('synthetic-fixture-only', json.dumps(value))


class WSTTFTEvidenceTests(unittest.TestCase):
    # 主固定manifest的独立合同，不从observed或正在修改的W测试生成expected。
    cases = {
        'TestOpenAIWSPassthroughFirstTokenFollowsTTFTMode': ('semantic', 'visible'),
        'TestOpenAIWSPassthroughTTFTOriginIncludesAdmission': (),
        'TestOpenAIWSTTFTModeConn': ('waits_for_each_successful_create', 'context_cancel_releases_waiter',
                                  'close_releases_waiter', 'write_failure_releases_waiter')}
    source_files = tuple('backend/internal/service/' + name for name in (
        'openai_ws_v2_passthrough_ttft_mode_test.go', 'openai_ws_v2_passthrough_adapter.go',
        'openai_ws_v2/passthrough_relay.go', 'openai_ws_v2/entry.go', 'openai_ws_v2/caddy_adapter.go',
        'openai_gateway_passthrough.go', 'openai_ws_forwarder.go'))
    selector = ('^(TestOpenAIWSPassthroughFirstTokenFollowsTTFTMode|'
                'TestOpenAIWSPassthroughTTFTOriginIncludesAdmission|TestOpenAIWSTTFTModeConn)$')
    command = ['go', 'test', '-json', '-mod=readonly', '-p=2', '-race', '-tags=unit',
               './internal/service', '-run', selector, '-count=1', '-timeout=180s']

    def events(self):
        events = [{'Package': gate.SERVICE, 'Action': 'start'}]
        for top, leaves in self.cases.items():
            events.append({'Package': gate.SERVICE, 'Test': top, 'Action': 'run'})
            for leaf in leaves:
                name = top + '/' + leaf
                events += [{'Package': gate.SERVICE, 'Test': name, 'Action': action} for action in ('run', 'pass')]
            events.append({'Package': gate.SERVICE, 'Test': top, 'Action': 'pass'})
        return events + [{'Package': gate.SERVICE, 'Action': 'pass'}]

    def execution(self, events_sha='c' * 64, stderr=b''):
        context = {'source_commit': 'a' * 40, 'github_sha': 'a' * 40, 'runner_version': 'v0222-ci-v8.1',
                   'inputs': {path: 'b' * 64 for path in self.source_files}, 'native_execution': False}
        return context, {**context, 'command': list(self.command), 'working_directory': 'backend', 'exit_code': 0,
                         'command_completed': True, 'isolated_live_environment': True,
                         'events_sha256': events_sha, 'stderr_sha256': hashlib.sha256(stderr).hexdigest()}

    def test_fixed_three_top_seven_leaf_nine_nodes_origin_is_also_leaf(self):
        value = gate.verify(self.events(), 0, 'ws-ttft')
        expected_nodes = set(self.cases) | {top + '/' + leaf for top, leaves in self.cases.items() for leaf in leaves}
        expected_leaves = {top + '/' + leaf for top, leaves in self.cases.items() for leaf in leaves} | {
            'TestOpenAIWSPassthroughTTFTOriginIncludesAdmission'}
        self.assertEqual(list(self.cases), list(gate.WS_TTFT_TESTS))
        self.assertEqual(expected_nodes, set(gate.WS_TTFT_NODES))
        self.assertEqual(expected_leaves, set(gate.WS_TTFT_LEAVES))
        self.assertEqual((3, 7, 9, 6, 0), (value['top_level_count'], value['leaf_count'],
                         value['observed_tests'], value['subtest_count'], value['skip_count']))
        self.assertEqual({'run': 9, 'pass': 9, 'fail': 0, 'skip': 0}, value['target_counts'])
        self.assertEqual({name: 'pass' for name in expected_leaves}, value['leaf_outcomes'])
        self.assertNotIn('same_test_evidence', value, 'WS relies on permanent Go assertions, not new traces')
        self.assertEqual(self.command, gate.WS_TTFT_COMMAND)

    def test_missing_each_run_pass_package_start_or_terminal_reject(self):
        good = self.events()
        for index, event in enumerate(good):
            with self.subTest(index=index, test=event.get('Test'), action=event['Action']), self.assertRaises(ValueError):
                gate.verify(good[:index] + good[index+1:], 0, 'ws-ttft')

    def test_duplicate_each_run_terminal_and_package_boundary_reject(self):
        good = self.events()
        for index, event in enumerate(good):
            with self.subTest(index=index, action=event['Action']), self.assertRaises(ValueError):
                gate.verify(good[:index] + [event] + good[index:], 0, 'ws-ttft')

    def test_wrong_package_test_extra_test_and_descendant_reject(self):
        good = self.events()
        for index, event in enumerate(good):
            bad = copy.deepcopy(good)
            bad[index]['Package'] = 'wrong-package'
            with self.subTest(index=index, field='Package'), self.assertRaises(ValueError):
                gate.verify(bad, 0, 'ws-ttft')
            if event.get('Test'):
                for wrong in ('TestWrongTTFT', event['Test'] + '/unknown'):
                    bad = copy.deepcopy(good)
                    bad[index]['Test'] = wrong
                    with self.subTest(index=index, wrong=wrong), self.assertRaises(ValueError):
                        gate.verify(bad, 0, 'ws-ttft')
        for name in ('TestOtherPassingTest', 'TestEstimateOpenAIInputTokens_CompareWithOpenAIAPI'):
            extra = [{'Package': gate.SERVICE, 'Test': name, 'Action': action} for action in ('run', 'pass')]
            with self.subTest(extra=name), self.assertRaises(ValueError):
                gate.verify(good[:-1] + extra + good[-1:], 0, 'ws-ttft')

    def test_skip_fail_buildfail_zero_nonzero_and_parent_only_pass_reject(self):
        good = self.events()
        for sample in ([], [{'Package': gate.SERVICE, 'Action': 'start'}, {'Package': gate.SERVICE, 'Action': 'pass'}],
                       [None], good + [{'Action': 'build-fail'}], good + [{'Package': 'other', 'Action': 'fail'}],
                       events_for(tuple(self.cases), package=gate.SERVICE)):
            with self.subTest(size=len(sample)), self.assertRaises(ValueError):
                gate.verify(sample, 0, 'ws-ttft')
        for index, event in enumerate(good):
            if event['Action'] == 'pass':
                for action in ('skip', 'fail', 'build-fail'):
                    bad = copy.deepcopy(good)
                    bad[index]['Action'] = action
                    with self.subTest(index=index, action=action), self.assertRaises(ValueError):
                        gate.verify(bad, 0, 'ws-ttft')
        for code in (1, -1, False, 0.0):
            with self.subTest(exit=code), self.assertRaises(ValueError):
                gate.verify(good, code, 'ws-ttft')

    def test_terminal_before_run_parent_before_children_or_post_package_event_reject(self):
        good = self.events()
        for index, event in enumerate(good):
            if event['Action'] == 'pass' and event.get('Test'):
                bad = copy.deepcopy(good)
                terminal = bad.pop(index)
                bad.insert(1, terminal)
                with self.subTest(terminal=event['Test']), self.assertRaises(ValueError):
                    gate.verify(bad, 0, 'ws-ttft')
        bad = copy.deepcopy(good)
        bad[1], bad[2] = bad[2], bad[1]
        with self.assertRaises(ValueError):
            gate.verify(bad, 0, 'ws-ttft')
        with self.assertRaises(ValueError):
            gate.verify(good + [{'Package': gate.SERVICE, 'Action': 'output', 'Output': 'late'}], 0, 'ws-ttft')
        bad = copy.deepcopy(good)
        index = next(i for i, event in enumerate(bad) if event.get('Test') == list(self.cases)[0] and event['Action'] == 'pass')
        terminal = bad.pop(index)
        bad.insert(2, terminal)
        with self.assertRaises(ValueError):
            gate.verify(bad, 0, 'ws-ttft')

    def test_output_pause_cont_does_not_replace_required_nodes_or_require_trace_marker(self):
        good = self.events()
        top = list(self.cases)[0]
        extra = [{'Package': gate.SERVICE, 'Test': top, 'Action': action, 'Output': 'synthetic fixture output'}
                 for action in ('output', 'pause', 'cont')]
        self.assertTrue(gate.verify(good[:2] + extra + good[2:], 0, 'ws-ttft')['verified'])
        for event in ({'Package': gate.SERVICE, 'Test': 'TestOther', 'Action': 'output'},
                      {'Package': gate.SERVICE, 'Test': top, 'Action': 'output', 'Output': {}},
                      {'Package': gate.SERVICE, 'Test': top, 'Action': 'invented'}):
            with self.subTest(event=event), self.assertRaises(ValueError):
                gate.verify(good[:2] + [event] + good[2:], 0, 'ws-ttft')

    def test_execution_binds_exact_command_count_tags_race_json_selector_and_no_overlay(self):
        context, execution = self.execution()
        gate.validate_execution(execution, context, 0, 'c' * 64, 'ws-ttft')
        mutations = [lambda value: value['command'].remove('-race'), lambda value: value['command'].remove('-json'),
                     lambda value: value['command'].__setitem__(3, '-mod=mod'),
                     lambda value: value['command'].__setitem__(4, '-p=1'),
                     lambda value: value['command'].__setitem__(6, '-tags=integration'),
                     lambda value: value['command'].__setitem__(7, './internal/repository'),
                     lambda value: value['command'].__setitem__(-3, '^TestOpenAIWS'),
                     lambda value: value['command'].__setitem__(-2, '-count=5'),
                     lambda value: value['command'].__setitem__(-1, '-timeout=1h'),
                     lambda value: value['command'].extend(['-overlay', 'synthetic-P-mutant.json']),
                     lambda value: value['command'].append('-shuffle=on'),
                     lambda value: value['command'].append('./internal/handler'),
                     lambda value: value.update(command=list(gate.PUBLISH_LIFECYCLE_COMMAND))]
        for index, mutate in enumerate(mutations):
            bad = copy.deepcopy(execution)
            mutate(bad)
            with self.subTest(index=index), self.assertRaises(ValueError):
                gate.validate_execution(bad, context, 0, 'c' * 64, 'ws-ttft')

    def test_execution_rejects_source_github_sha_inputs_raw_directexit_cwd_runner_isolation(self):
        context, execution = self.execution()
        for field, wrong in (('source_commit', 'd' * 40), ('github_sha', 'd' * 40), ('inputs', {}),
                             ('events_sha256', 'd' * 64), ('exit_code', 1), ('exit_code', False),
                             ('working_directory', '.'), ('runner_version', 'wrong'), ('command_completed', False),
                             ('isolated_live_environment', False)):
            bad = copy.deepcopy(execution)
            bad[field] = wrong
            with self.subTest(field=field, wrong=wrong), self.assertRaises(ValueError):
                gate.validate_execution(bad, context, 0, 'c' * 64, 'ws-ttft')
        for wrong in ('d' * 40, None):
            bad_context = {**context, 'github_sha': wrong}
            bad = {**execution, 'github_sha': wrong}
            with self.subTest(same_sha=wrong), self.assertRaises(ValueError):
                gate.validate_execution(bad, bad_context, 0, 'c' * 64, 'ws-ttft')

    def test_execution_requires_each_fixed_sourceinput_and_valid_hash_even_when_receipts_agree(self):
        self.assertEqual(self.source_files, gate.WS_TTFT_SOURCE_FILES)
        context, execution = self.execution()
        for path in self.source_files:
            for wrong in (None, '', 'invalid-hash', True):
                bad_context = copy.deepcopy(context)
                if wrong is None:
                    bad_context['inputs'].pop(path)
                else:
                    bad_context['inputs'][path] = wrong
                bad = {**execution, 'inputs': bad_context['inputs']}
                with self.subTest(path=path, wrong=wrong), self.assertRaises(ValueError):
                    gate.validate_execution(bad, bad_context, 0, 'c' * 64, 'ws-ttft')

    def test_stderr_hash_is_required_and_bytes_cannot_drift(self):
        _, execution = self.execution(stderr=b'synthetic stderr')
        gate.validate_boundary_stderr(execution, b'synthetic stderr', 'ws-ttft')
        for bad in ({key: value for key, value in execution.items() if key != 'stderr_sha256'},
                    {**execution, 'stderr_sha256': 'f' * 64}):
            with self.assertRaises(ValueError):
                gate.validate_boundary_stderr(bad, b'synthetic stderr', 'ws-ttft')
        with self.assertRaises(ValueError):
            gate.validate_boundary_stderr(execution, b'changed', 'ws-ttft')

    def test_main_names_all_nodes_and_rejects_raw_stderr_exit_source_context_or_duplicate_json(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            events, stderr, execution_path, receipt = [root / name for name in
                ('events.jsonl', 'stderr.log', 'execution.json', 'receipt.json')]
            raw = ('\n'.join(json.dumps(event) for event in self.events()) + '\n').encode()
            context, execution = self.execution(hashlib.sha256(raw).hexdigest())
            argv = ['v0222_test_receipt.py', '--mode', 'ws-ttft', '--events', str(events), '--stderr', str(stderr),
                    '--exit-code', '0', '--execution', str(execution_path), '--receipt', str(receipt)]
            events.write_bytes(raw)
            stderr.write_bytes(b'')
            execution_path.write_text(json.dumps(execution), encoding='utf-8')
            with patch.object(sys, 'argv', argv), patch.object(gate.evidence, 'context', return_value=context):
                gate.main()
            value = json.loads(receipt.read_text())
            self.assertTrue(value['verified'])
            self.assertFalse(value['native_execution'])
            self.assertEqual((3, 7, 9), (value['top_level_count'], value['leaf_count'], value['observed_tests']))
            self.assertEqual(hashlib.sha256(execution_path.read_bytes()).hexdigest(), value['execution_sha256'])
            for mutation in ('raw', 'stderr', 'directexit', 'exit-argument', 'source', 'same-sha',
                             'missing-stderr', 'duplicate-event-json', 'duplicate-execution-json'):
                arguments = list(argv)
                current, current_context = copy.deepcopy(execution), copy.deepcopy(context)
                events.write_bytes(raw)
                stderr.write_bytes(b'')
                if mutation == 'raw':
                    events.write_bytes(raw + b'\n')
                elif mutation == 'stderr':
                    stderr.write_bytes(b'changed')
                elif mutation == 'directexit':
                    current['exit_code'] = 1
                elif mutation == 'exit-argument':
                    arguments[arguments.index('--exit-code') + 1] = '1'
                elif mutation == 'source':
                    current_context['inputs'].pop(self.source_files[0])
                    current['inputs'] = current_context['inputs']
                elif mutation == 'same-sha':
                    current_context['github_sha'] = current['github_sha'] = 'd' * 40
                elif mutation == 'missing-stderr':
                    index = arguments.index('--stderr')
                    del arguments[index:index+2]
                elif mutation == 'duplicate-event-json':
                    changed = raw.replace(b'"Action": "start"', b'"Action": "start", "Action": "start"', 1)
                    events.write_bytes(changed)
                    current['events_sha256'] = hashlib.sha256(changed).hexdigest()
                encoded = json.dumps(current)
                if mutation == 'duplicate-execution-json':
                    encoded = '{"exit_code":0,' + encoded[1:]
                execution_path.write_text(encoded, encoding='utf-8')
                with self.subTest(mutation=mutation), patch.object(sys, 'argv', arguments), \
                        patch.object(gate.evidence, 'context', return_value=current_context), self.assertRaises(SystemExit) as rejected:
                    gate.main()
                self.assertEqual(1, rejected.exception.code)
                self.assertFalse(json.loads(receipt.read_text())['verified'])


class CacheRaceEvidenceTests(unittest.TestCase):
    def events(self):
        manifest = gate.cache_manifest
        events = []
        for package in manifest.CACHE_CASES:
            for name in sorted(manifest.nodes(package)):
                events += [{'Package': package, 'Test': name, 'Action': action} for action in ('run', 'pass')]
            events.append({'Package': package, 'Action': 'pass'})
        return events

    def test_fixed_two_packages_35_top_179_leaves_not_expanded_419(self):
        receipt = gate.verify(self.events(), 0, 'cache-race')
        self.assertEqual(35, receipt['top_level_count'])
        self.assertEqual(179, receipt['leaf_count'])
        self.assertEqual({'run': 203, 'pass': 203, 'fail': 0, 'skip': 0}, receipt['target_counts'])
        self.assertEqual(34, receipt['packages'][gate.SERVICE]['top_level_count'])
        self.assertEqual(170, receipt['packages'][gate.SERVICE]['leaf_count'])
        self.assertEqual(9, receipt['packages'][gate.cache_manifest.APICOMPAT]['leaf_count'])

    def test_missing_any_top_leaf_run_or_pass_rejects(self):
        good = self.events()
        for index, event in enumerate(good):
            if event.get('Test') and event['Action'] in ('run', 'pass'):
                with self.subTest(test=event['Test'], action=event['Action']), self.assertRaises(ValueError):
                    gate.verify(good[:index] + good[index+1:], 0, 'cache-race')

    def test_skip_fail_buildfail_or_other_package_failure_rejects(self):
        good = self.events()
        for package in gate.cache_manifest.CACHE_CASES:
            target = gate.cache_manifest.leaves(package)[0]
            for action in ('skip', 'fail'):
                bad = [{**event, 'Action': action} if event.get('Package') == package and
                       event.get('Test') == target and event['Action'] == 'pass' else event for event in good]
                with self.subTest(package=package, action=action), self.assertRaises(ValueError):
                    gate.verify(bad, 0, 'cache-race')
        for event in ({'Action': 'build-fail'}, {'Package': 'unexpected-package', 'Action': 'fail'},
                      {'Package': gate.SERVICE, 'Action': 'skip'}):
            with self.subTest(event=event), self.assertRaises(ValueError):
                gate.verify(good + [event], 0, 'cache-race')

    def test_zero_match_wrong_package_extra_case_and_duplicate_reject(self):
        good = self.events()
        samples = [[], [{'Package': package, 'Action': 'pass'} for package in gate.cache_manifest.CACHE_CASES],
                   [event for event in good if event.get('Package') != gate.cache_manifest.APICOMPAT],
                   good + [good[0]], good + [good[1]],
                   good + [{'Package': 'wrong-package', 'Test': 'TestV10CacheUnknown', 'Action': 'run'}],
                   good + [{'Package': gate.SERVICE, 'Test': 'TestV10CacheNewUnreviewedCase', 'Action': 'run'}]]
        for sample in samples:
            with self.subTest(events=len(sample)), self.assertRaises(ValueError):
                gate.verify(sample, 0, 'cache-race')
        with self.assertRaises(ValueError):
            gate.verify(good, 1, 'cache-race')

    def test_cache_command_execution_exact_tags_packages_selector_and_hash(self):
        manifest = gate.cache_manifest
        context = {'source_commit': 'a' * 40, 'github_sha': 'a' * 40,
                   'runner_version': 'v0222-ci-v8.1', 'inputs': {'test.go': 'b' * 64}}
        execution = {**context, 'exit_code': 0, 'command_completed': True, 'events_sha256': 'c' * 64,
                     'command': list(manifest.CACHE_COMMAND), 'working_directory': 'backend'}
        gate.validate_execution(execution, context, 0, 'c' * 64, 'cache-race')
        edits = [lambda value: value.update(github_sha='d' * 40), lambda value: value.update(inputs={}),
                 lambda value: value.update(events_sha256='d' * 64), lambda value: value.update(exit_code=1),
                 lambda value: value['command'].remove('./internal/pkg/apicompat'),
                 lambda value: value['command'].__setitem__(6, '-tags=integration'),
                 lambda value: value['command'].__setitem__(10, '^TestV10Cache'),
                 lambda value: value['command'].remove('-race')]
        for index, edit in enumerate(edits):
            value = copy.deepcopy(execution)
            edit(value)
            with self.subTest(index=index), self.assertRaises(ValueError):
                gate.validate_execution(value, context, 0, 'c' * 64, 'cache-race')

    def test_cache_main_writes_both_named_packages_and_raw_drift_false(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            events, execution, receipt = root / 'events.jsonl', root / 'execution.json', root / 'receipt.json'
            events.write_text('\n'.join(json.dumps(event) for event in self.events()) + '\n', encoding='utf-8')
            context = {'source_commit': 'a' * 40, 'github_sha': 'a' * 40,
                       'runner_version': 'v0222-ci-v8.1', 'inputs': {}, 'native_execution': False}
            execution.write_text(json.dumps({**context, 'command': gate.cache_manifest.CACHE_COMMAND,
                'exit_code': 0, 'working_directory': 'backend', 'command_completed': True,
                'events_sha256': hashlib.sha256(events.read_bytes()).hexdigest()}), encoding='utf-8')
            argv = ['v0222_test_receipt.py', '--mode', 'cache-race', '--events', str(events), '--exit-code', '0',
                    '--execution', str(execution), '--receipt', str(receipt)]
            with patch.object(sys, 'argv', argv), patch.object(gate.evidence, 'context', return_value=context):
                gate.main()
            value = json.loads(receipt.read_text())
            self.assertTrue(value['verified'])
            self.assertFalse(value['native_execution'])
            self.assertEqual(179, value['leaf_count'])
            self.assertEqual(set(gate.cache_manifest.CACHE_CASES), set(value['packages']))
            events.write_bytes(events.read_bytes() + b'\n')
            with patch.object(sys, 'argv', argv), patch.object(gate.evidence, 'context', return_value=context), self.assertRaises(SystemExit) as error:
                gate.main()
            self.assertEqual(1, error.exception.code)
            self.assertFalse(json.loads(receipt.read_text())['verified'])

    def test_sessionmemory_named_gate_remains_outside_cache_matrix(self):
        names = gate.cache_manifest.SESSION_MEMORY_TESTS
        good = events_for(names, package=gate.SERVICE)
        value = gate.verify(good, 0, 'session-memory')
        self.assertEqual(6, value['top_level_count'])
        self.assertEqual(6, value['leaf_count'])
        self.assertTrue(set(names).isdisjoint(gate.cache_manifest.SERVICE_CASES))
        for events, code in ((events_for(names[:-1], package=gate.SERVICE), 0),
                             (events_for(names, 'skip', package=gate.SERVICE), 0), (good, 1),
                             (good + [{'Package': 'another', 'Action': 'fail'}], 0)):
            with self.subTest(events=len(events), code=code), self.assertRaises(ValueError):
                gate.verify(events, code, 'session-memory')

    def test_fixed_source_manifest_rejects_same_name_new_case_or_missing_file(self):
        manifest = gate.cache_manifest
        root = Path(__file__).resolve().parents[2]
        manifest.check_sources(root)
        file = root / next(iter(manifest.CACHE_TEST_SOURCE_SHA256))
        original = Path.read_bytes
        def changed(path):
            return original(path) + b'\n// synthetic extra case\n' if path == file else original(path)
        with patch.object(Path, 'read_bytes', changed), self.assertRaises(ValueError):
            manifest.check_sources(root)
        with tempfile.TemporaryDirectory() as directory, self.assertRaises(ValueError):
            manifest.check_sources(Path(directory))


class BillingRaceEvidenceTests(unittest.TestCase):
    def events(self):
        names = gate.BILLING_RACE_TESTS + gate.BILLING_RACE_LEAVES
        return [{'Package': gate.REPOSITORY, 'Test': name, 'Action': action}
                for name in names for action in ('run', 'pass')] + [{'Package': gate.REPOSITORY, 'Action': 'pass'}]

    def test_exact_top_and_repository_lane_leaves_pass_without_skips(self):
        receipt = gate.verify(self.events(), 0, 'billing-race')
        self.assertTrue(receipt['verified'])
        self.assertEqual(list(gate.BILLING_RACE_TESTS), receipt['required_top_level_tests'])
        self.assertEqual(1, receipt['top_level_count'])
        self.assertEqual(2, receipt['leaf_count'])
        self.assertEqual({name: 'pass' for name in gate.BILLING_RACE_LEAVES}, receipt['leaf_outcomes'])
        self.assertEqual({'run': 3, 'pass': 3, 'fail': 0, 'skip': 0}, receipt['target_counts'])
        self.assertFalse(set(gate.BILLING_RACE_TESTS) & set(gate.ROLLUP_TESTS + gate.ROLLUP_ADDITIONAL_TESTS))
        self.assertEqual(10, len(gate.ROLLUP_TESTS))
        self.assertEqual((), gate.ROLLUP_ADDITIONAL_TESTS)

    def test_missing_top_or_named_leaf_run_outcome_reject(self):
        good = self.events()
        for name in gate.BILLING_RACE_TESTS + gate.BILLING_RACE_LEAVES:
            for action in ('run', 'pass'):
                bad = [event for event in good if not (event.get('Test') == name and event['Action'] == action)]
                with self.subTest(name=name, action=action), self.assertRaises(ValueError):
                    gate.verify(bad, 0, 'billing-race')
        with self.assertRaises(ValueError):
            gate.verify(events_for(gate.BILLING_RACE_TESTS), 0, 'billing-race')

    def test_wrong_leaf_extra_subcase_or_other_top_reject(self):
        good = self.events()
        bad_names = (gate.BILLING_RACE_TESTS[0] + '/repository-copy',
                     gate.BILLING_RACE_LEAVES[0] + '/extra-case', gate.ROLLUP_TESTS[0])
        for name in bad_names:
            bad = good + [{'Package': gate.REPOSITORY, 'Test': name, 'Action': action} for action in ('run', 'pass')]
            with self.subTest(name=name), self.assertRaises(ValueError):
                gate.verify(bad, 0, 'billing-race')

    def test_skip_fail_nonzero_compile_dockerfail_and_zero_match_reject(self):
        good = self.events()
        samples = [(good, 1), ([], 0), ([{'Package': gate.REPOSITORY, 'Action': 'pass'}], 0),
                   ([{'Package': gate.REPOSITORY, 'Action': 'fail', 'Output': 'Docker unavailable'}], 1),
                   (good + [{'Action': 'build-fail'}], 0), (good + [{'Package': 'another', 'Action': 'fail'}], 0)]
        for action in ('skip', 'fail'):
            samples.append(([{**event, 'Action': action} if event.get('Test') == gate.BILLING_RACE_LEAVES[0]
                             and event['Action'] == 'pass' else event for event in good], 0))
        for events, code in samples:
            with self.subTest(events=len(events), code=code), self.assertRaises(ValueError):
                gate.verify(events, code, 'billing-race')

    def test_billing_receipt_main_writes_named_leaves_and_rejects_raw_drift(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            events, execution, receipt = root / 'events.jsonl', root / 'execution.json', root / 'receipt.json'
            events.write_text('\n'.join(json.dumps(value) for value in self.events()) + '\n', encoding='utf-8')
            context = {'source_commit': 'a' * 40, 'github_sha': 'a' * 40,
                       'runner_version': 'v0222-ci-v8.1', 'inputs': {}, 'native_execution': False}
            command = ['go', 'test', '-json', '-mod=readonly', '-p=2', '-race', '-tags=integration',
                       './internal/repository', '-run', gate.BILLING_RACE_SELECTOR, '-count=1', '-timeout=5m']
            execution.write_text(json.dumps({**context, 'command': command, 'exit_code': 0,
                'working_directory': 'backend', 'command_completed': True,
                'events_sha256': hashlib.sha256(events.read_bytes()).hexdigest()}), encoding='utf-8')
            argv = ['v0222_test_receipt.py', '--mode', 'billing-race', '--events', str(events), '--exit-code', '0',
                    '--execution', str(execution), '--receipt', str(receipt)]
            with patch.object(sys, 'argv', argv), patch.object(gate.evidence, 'context', return_value=context):
                gate.main()
            value = json.loads(receipt.read_text())
            self.assertTrue(value['verified'])
            self.assertFalse(value['native_execution'])
            self.assertEqual(1, value['top_level_count'])
            self.assertEqual(2, value['leaf_count'])
            self.assertEqual({name: 'pass' for name in gate.BILLING_RACE_LEAVES}, value['leaf_outcomes'])
            self.assertEqual(hashlib.sha256(execution.read_bytes()).hexdigest(), value['execution_sha256'])
            events.write_text(events.read_text() + '\n', encoding='utf-8')
            with patch.object(sys, 'argv', argv), patch.object(gate.evidence, 'context', return_value=context), self.assertRaises(SystemExit) as rejected:
                gate.main()
            self.assertEqual(1, rejected.exception.code)
            self.assertFalse(json.loads(receipt.read_text())['verified'])

    def test_billing_execution_binds_exact_command_sha_inputs_and_raw_hash(self):
        context = {'source_commit': 'a' * 40, 'github_sha': 'a' * 40,
                   'runner_version': 'v0222-ci-v8.1', 'inputs': {'test.go': 'b' * 64}}
        command = ['go', 'test', '-json', '-mod=readonly', '-p=2', '-race', '-tags=integration',
                   './internal/repository', '-run', gate.BILLING_RACE_SELECTOR, '-count=1', '-timeout=5m']
        execution = {**context, 'exit_code': 0, 'command_completed': True, 'events_sha256': 'c' * 64,
                     'command': command, 'working_directory': 'backend'}
        gate.validate_execution(execution, context, 0, 'c' * 64, 'billing-race')
        mutations = [lambda value: value.update(source_commit='d' * 40),
                     lambda value: value.update(inputs={}), lambda value: value.update(events_sha256='d' * 64),
                     lambda value: value.update(command_completed=False), lambda value: value.update(exit_code=1),
                     lambda value: value.update(working_directory='.'),
                     lambda value: value['command'].remove('-race'),
                     lambda value: value['command'].__setitem__(6, '-tags=unit'),
                     lambda value: value['command'].__setitem__(9, '^TestCompositeUsage')]
        for index, mutate in enumerate(mutations):
            bad = copy.deepcopy(execution)
            mutate(bad)
            with self.subTest(index=index), self.assertRaises(ValueError):
                gate.validate_execution(bad, context, 0, 'c' * 64, 'billing-race')


class OldAppProbeContractTests(unittest.TestCase):
    def test_health_or_setup_only_is_not_normal_boot(self):
        for sample in [{'health': {'status': 'ok'}},
                       {'health': {'status': 'ok'}, 'setup': {'needs_setup': True}},
                       {'health': {'status': 'ok'}, 'public_version': oldboot.OLD_VERSION}]:
            with self.subTest(sample=sample), self.assertRaises(ValueError):
                oldboot.check_normal(sample, oldboot.OLD_VERSION)

    def test_normal_boot_requires_full_route_and_embedded_frontend_evidence(self):
        sample = {'health': {'status': 'ok'}, 'setup': {'needs_setup': False, 'step': 'completed'},
                  'public_version': oldboot.OLD_VERSION, 'profile_status': 401,
                  'entry_path': '/assets/index-synthetic.js', 'entry_sha256': 'c' * 64}
        oldboot.check_normal(sample, oldboot.OLD_VERSION)
        for key, bad in [('public_version', 'other'), ('profile_status', 404), ('entry_sha256', '')]:
            with self.subTest(key=key), self.assertRaises(ValueError):
                oldboot.check_normal({**sample, key: bad}, oldboot.OLD_VERSION)

    def test_two_background_cycles_need_new_watermark_bucket_and_times(self):
        first = {'closed_before': '2026-11-17', 'bucket_total': 1.25, 'detail_total': 1.25,
                 'computed_at': '2026-11-17T00:00:01Z', 'updated_at': '2026-11-17T00:00:01Z'}
        self.assertTrue(oldboot.background_complete(first, '2026-11-17', 1.25))
        self.assertFalse(oldboot.background_complete(first, '2026-11-17', 1.25, first))
        second = {**first, 'bucket_total': 2.75, 'detail_total': 2.75,
                  'computed_at': '2026-11-17T00:00:03Z', 'updated_at': '2026-11-17T00:00:03Z'}
        self.assertTrue(oldboot.background_complete(second, '2026-11-17', 2.75, first))
        self.assertFalse(oldboot.background_complete({**second, 'bucket_total': 1.25}, '2026-11-17', 2.75, first))

    def test_ledger_requires_exact_source_delta_and_checksums(self):
        old = {'old.sql': {'raw_sha256': 'a', 'checksum': 'b'}}
        new = {**old, **{name: {'raw_sha256': name, 'checksum': name} for name in oldboot.DELTA}}
        ledger = [{'filename': name, 'checksum': values['checksum'], 'applied_at': 'synthetic-time'}
                  for name, values in new.items()]
        oldboot.check_migrations(old, new, ledger)
        for bad_ledger in [ledger[:-1], ledger + [ledger[0]],
                           [{**row, 'checksum': 'wrong'} for row in ledger]]:
            with self.subTest(rows=len(bad_ledger)), self.assertRaises(ValueError):
                oldboot.check_migrations(old, new, bad_ledger)
        with self.assertRaises(ValueError):
            oldboot.check_migrations(old, {**new, 'unreviewed.sql': {}}, ledger)

    def test_go_trim_space_does_not_use_python_only_control_whitespace(self):
        self.assertEqual('SELECT 1;', '\u3000SELECT 1;\u3000'.strip(oldboot.GO_SPACE))
        self.assertEqual('\x1cSELECT 1;\x1c', '\x1cSELECT 1;\x1c'.strip(oldboot.GO_SPACE))

    def test_native_probe_refuses_outside_actions_without_running_docker(self):
        with tempfile.TemporaryDirectory() as directory:
            env = dict(os.environ)
            env.pop('CI', None)
            env.pop('GITHUB_ACTIONS', None)
            result = subprocess.run([sys.executable, '-I', '-B',
                str(Path(__file__).with_name('v0222_old_app_probe.py')), '--artifacts', directory,
                '--execute-isolated-ci-probe'], env=env, capture_output=True, timeout=10)
            self.assertNotEqual(0, result.returncode)
            self.assertIn(b'refusing old-app probe', result.stderr)
            self.assertFalse((Path(directory) / 'old-app-probe.json').exists())


class OwnedNetworkProbeTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        root = Path(self.temporary.name)
        artifacts = root / 'artifacts'
        artifacts.mkdir()
        self.probe = oldboot.Probe(root, artifacts)
        self.probe.network_id = 'a' * 64
        self.probe.containers.append(self.probe.redis)
        self.cid, self.endpoint_id = 'b' * 64, 'c' * 64
        self.container = {'Id': self.cid, 'Name': '/' + self.probe.redis,
            'Config': {'Labels': {'sub2api.ci.owner': self.probe.owner}},
            'State': {'Running': True}, 'RestartCount': 0,
            'HostConfig': {'NetworkMode': self.probe.network, 'PortBindings': {}, 'PublishAllPorts': False},
            'NetworkSettings': {'Ports': {'6379/tcp': None}, 'Networks': {self.probe.network: {
                'NetworkID': self.probe.network_id, 'EndpointID': self.endpoint_id,
                'IPAddress': '172.28.1.3', 'IPPrefixLen': 24, 'Gateway': '172.28.1.1'}}}}
        self.network = {'Id': self.probe.network_id, 'Name': self.probe.network, 'Driver': 'bridge',
            'Scope': 'local', 'Internal': True, 'Labels': {'sub2api.ci.owner': self.probe.owner},
            'IPAM': {'Config': [{'Subnet': '172.28.1.0/24', 'Gateway': '172.28.1.1'}]},
            'Containers': {self.cid: {'Name': self.probe.redis, 'EndpointID': self.endpoint_id,
                                     'IPv4Address': '172.28.1.3/24'}}}

    def docker_inspect(self, *arguments, **kwargs):
        if arguments == ('inspect', self.probe.redis):
            return json.dumps([self.container])
        if arguments == ('network', 'inspect', self.probe.network_id):
            return json.dumps([self.network])
        raise AssertionError(arguments)

    def test_owned_endpoint_accepts_no_published_ports_and_exact_private_ip(self):
        with patch.object(self.probe, 'docker', side_effect=self.docker_inspect):
            endpoint = self.probe.endpoint(self.probe.redis)
        self.assertEqual('172.28.1.3', endpoint['ipv4'])
        self.assertEqual(self.cid, endpoint['container_id'])
        self.assertEqual(self.endpoint_id, endpoint['endpoint_id'])

    def test_endpoint_rejects_wrong_owner_network_and_missing_ip(self):
        original_container, original_network = copy.deepcopy(self.container), copy.deepcopy(self.network)
        changes = [
            lambda: self.container['Config']['Labels'].update({'sub2api.ci.owner': 'other'}),
            lambda: self.network['Labels'].update({'sub2api.ci.owner': 'other'}),
            lambda: self.network.update({'Internal': False}),
            lambda: self.network.update({'Id': 'd' * 64}),
            lambda: self.network.update({'Driver': 'host'}),
            lambda: self.container['HostConfig'].update({'NetworkMode': 'bridge'}),
            lambda: self.container['HostConfig'].update({'PortBindings': {'8080/tcp': [{'HostPort': '8080'}]}}),
            lambda: self.container['NetworkSettings']['Networks'].update({'outside': {}}),
            lambda: self.container['NetworkSettings']['Networks'][self.probe.network].update({'NetworkID': 'd' * 64}),
            lambda: self.container['NetworkSettings']['Networks'][self.probe.network].pop('IPAddress'),
            lambda: self.container['NetworkSettings']['Networks'][self.probe.network].update({'EndpointID': 'd' * 64}),
            lambda: self.network.update({'Containers': {}}),
            lambda: self.network['Containers'][self.cid].update({'IPv4Address': '172.28.1.4/24'}),
        ]
        for index, mutate in enumerate(changes):
            self.container, self.network = copy.deepcopy(original_container), copy.deepcopy(original_network)
            mutate()
            with self.subTest(case=index), patch.object(self.probe, 'docker', side_effect=self.docker_inspect), self.assertRaises(ValueError):
                self.probe.endpoint(self.probe.redis)
        with patch.object(self.probe, 'docker') as docker, self.assertRaises(ValueError):
            self.probe.endpoint('not-a-registered-container')
        docker.assert_not_called()

    def test_endpoint_rejects_public_loopback_ipv6_empty_and_outside_subnet(self):
        original = copy.deepcopy(self.container)
        for value in ('8.8.8.8', '127.0.0.1', '169.254.1.2', '0.0.0.0', '::1', '', None, '10.1.1.2'):
            self.container = copy.deepcopy(original)
            self.container['NetworkSettings']['Networks'][self.probe.network]['IPAddress'] = value
            with self.subTest(address=value), patch.object(self.probe, 'docker', side_effect=self.docker_inspect), self.assertRaises(ValueError):
                self.probe.endpoint(self.probe.redis)

    def test_http_direct_owned_ip_ignores_proxies_and_keeps_token_in_memory(self):
        response = MagicMock()
        response.status, response.headers = 200, {'Content-Type': 'application/json'}
        response.read.return_value = b'{"status":"ok"}'
        opener = MagicMock()
        opener.open.return_value.__enter__.return_value = response
        token = 'SYNTHETIC_AUTH_MUST_NOT_BE_RECORDED'
        with patch.object(self.probe, 'docker', side_effect=self.docker_inspect), \
                patch.object(oldboot.urllib.request, 'build_opener', return_value=opener) as builder:
            result = self.probe.http(self.probe.redis, '/health', token=token)
        request = opener.open.call_args.args[0]
        self.assertEqual('http://172.28.1.3:8080/health', request.full_url)
        self.assertEqual('Bearer ' + token, request.get_header('Authorization'))
        self.assertEqual({}, builder.call_args.args[0].proxies)
        self.assertEqual(200, result[0])
        self.assertNotIn(token, json.dumps(self.probe.receipt))

    def test_app_has_no_publish_or_extra_network_and_index_reuses_endpoint(self):
        name, image = self.probe.prefix + '-app', 'fixture:image'
        self.probe.data_proofs[str(self.probe.data)] = 'd' * 64
        with patch.object(self.probe, 'docker') as docker:
            self.probe.app(image, name)
        arguments = docker.call_args.args
        self.assertNotIn('-p', arguments)
        self.assertNotIn('--publish', arguments)
        self.assertEqual(1, arguments.count('--network'))
        self.assertEqual(self.probe.network, arguments[arguments.index('--network') + 1])
        import inspect
        source = inspect.getsource(oldboot.Probe.invalid_index)
        self.assertIn('candidate_endpoint = self.endpoint(failing_app)', source)
        self.assertNotIn("['NetworkSettings']['Networks']", source)

    def test_redis_pong_smoke_succeeds_before_both_image_builds(self):
        sequence = []
        connection = MagicMock()
        connection.recv.side_effect = [b'+PO', b'NG\r\n']
        opened = MagicMock()
        opened.__enter__.return_value = connection
        def connect(address, timeout):
            self.assertEqual(('172.28.1.3', 6379), address)
            self.assertGreater(timeout, 0)
            sequence.append('host-ping')
            return opened
        def docker(*arguments, **kwargs):
            if arguments[:2] == ('network', 'create'):
                self.assertIn('--internal', arguments)
                return 'a' * 64
            if arguments[:1] == ('run',):
                self.assertEqual(oldboot.REDIS_IMAGE, arguments[-1])
                self.assertNotIn('-p', arguments)
            if arguments[:1] == ('inspect',) or arguments[:2] == ('network', 'inspect'):
                return self.docker_inspect(*arguments, **kwargs)
            return ''
        # prepare_runtime_network 本身登记资源；还原 setUp 的预登记避免重复计数。
        self.probe.containers.clear()
        def build(*arguments):
            self.assertTrue(self.probe.receipt['network_smoke']['verified'])
            sequence.append('build-' + arguments[-1])
            return arguments[-1]
        with patch.object(self.probe, 'docker', side_effect=docker), patch.object(self.probe, 'identity', return_value=['fixture']), \
                patch.object(self.probe, 'image', side_effect=build), patch.object(oldboot.socket, 'create_connection', side_effect=connect):
            result = self.probe.build_release_images(Path('old'), Path('new'), 'd' * 40, 'fixture-version')
        self.assertEqual(('old', 'new'), result)
        self.assertEqual(['host-ping', 'build-old', 'build-new'], sequence)
        self.assertEqual(1, self.probe.containers.count(self.probe.redis))
        self.assertTrue(self.probe.receipt['no_egress_runtime_network'])
        connection.sendall.assert_called_once_with(b'*1\r\n$4\r\nPING\r\n')

    def test_smoke_failure_never_builds_and_cleanup_checks_owners(self):
        self.probe.containers.clear()
        def docker(*arguments, **kwargs):
            if arguments[:2] == ('network', 'create'):
                return 'a' * 64
            if arguments[:1] == ('inspect',) or arguments[:2] == ('network', 'inspect'):
                return self.docker_inspect(*arguments, **kwargs)
            return ''
        clock = [0.0]
        with patch.object(self.probe, 'docker', side_effect=docker), patch.object(self.probe, 'image') as build, \
                patch.object(oldboot.socket, 'create_connection', side_effect=ConnectionRefusedError), \
                patch.object(oldboot.time, 'monotonic', side_effect=lambda: clock[0]), \
                patch.object(oldboot.time, 'sleep', side_effect=lambda amount: clock.__setitem__(0, clock[0] + amount)):
            with self.assertRaises(ValueError):
                self.probe.build_release_images(Path('old'), Path('new'), 'd' * 40, 'fixture-version')
        build.assert_not_called()
        smoke = self.probe.receipt['network_smoke']
        self.assertTrue(smoke['executed'])
        self.assertFalse(smoke['verified'])
        self.assertEqual(1, smoke['exit_code'])
        self.assertLessEqual(smoke['elapsed_seconds'], 30)
        calls = []
        def cleanup_command(command, **kwargs):
            calls.append(command)
            if 'inspect' in command:
                return subprocess.CompletedProcess(command, 0, (self.probe.owner + '\n').encode(), b'')
            return subprocess.CompletedProcess(command, 0, b'', b'')
        with patch.object(oldboot.subprocess, 'run', side_effect=cleanup_command):
            self.probe.cleanup()
        self.assertIn(['docker', 'rm', '-f', self.probe.redis], calls)
        self.assertIn(['docker', 'network', 'rm', self.probe.network], calls)
        self.assertIn(['docker', 'volume', 'rm', self.probe.prefix + '-redis-data'], calls)
        self.assertTrue(self.probe.receipt['owned_disposable_cleanup_verified'])
        self.assertFalse(self.probe.work.exists())
        self.assertFalse(any('prune' in command for command in calls))

    def test_synthetic_postgres_password_never_enters_argv_or_receipt(self):
        self.probe.password = 'SYNTHETIC_PASSWORD_ONLY_IN_CHILD_ENV'
        commands = []
        def run(command, **kwargs):
            commands.append((command, kwargs))
            return subprocess.CompletedProcess(command, 0, b'fixture-created\n', b'')
        with patch.object(oldboot.subprocess, 'run', side_effect=run):
            self.probe.start_postgres()
        command, options = commands[-1]
        self.assertNotIn(self.probe.password, ' '.join(command))
        self.assertNotIn('POSTGRES_PASSWORD=' + self.probe.password, command)
        self.assertIn('POSTGRES_PASSWORD', command)
        self.assertEqual(self.probe.password, options['env']['POSTGRES_PASSWORD'])
        self.probe.persist()
        self.assertNotIn(self.probe.password, json.dumps(self.probe.receipt))
        self.assertNotIn(self.probe.password, (self.probe.artifacts / 'old-app-probe.json').read_text())

    def test_image_refuses_without_smoke_before_invoking_docker(self):
        with patch.object(self.probe, 'run') as run, self.assertRaises(ValueError):
            self.probe.image(Path('old'), oldboot.OLD_SHA, oldboot.OLD_VERSION, 'old')
        run.assert_not_called()
        self.assertEqual([], self.probe.images)

    def test_smoke_does_not_contact_wrong_owner_or_network(self):
        self.network['Labels']['sub2api.ci.owner'] = 'not-this-runner'
        with patch.object(self.probe, 'docker', side_effect=self.docker_inspect), \
                patch.object(oldboot.socket, 'create_connection') as connect, self.assertRaises(ValueError):
            self.probe.host_network_smoke()
        connect.assert_not_called()
        self.assertEqual(1, self.probe.receipt['network_smoke']['exit_code'])
        self.assertFalse(self.probe.receipt['network_smoke']['verified'])

    def test_owned_cleanup_does_not_remove_foreign_labels(self):
        commands = []
        def foreign(command, **kwargs):
            commands.append(command)
            return subprocess.CompletedProcess(command, 0, b'another-owner\n', b'')
        with patch.object(oldboot.subprocess, 'run', side_effect=foreign), self.assertRaises(ValueError):
            self.probe.cleanup()
        self.assertTrue(all('inspect' in command for command in commands))
        self.assertFalse(self.probe.receipt.get('owned_disposable_cleanup_verified', False))

    def test_wrong_redis_response_is_not_connectivity_success(self):
        opened, connection = MagicMock(), MagicMock()
        opened.__enter__.return_value = connection
        connection.recv.return_value = b'+NOT-PONG\r\n'
        with patch.object(self.probe, 'docker', side_effect=self.docker_inspect), \
                patch.object(oldboot.socket, 'create_connection', return_value=opened), self.assertRaises(ValueError):
            self.probe.host_network_smoke()
        self.assertFalse(self.probe.receipt['network_smoke']['verified'])

    def test_stage_counts_are_derived_and_not_go_events(self):
        self.probe.persist()
        self.assertEqual({'executed': 0, 'verified': 0, 'unverified': 0, 'not_executed': 6}, self.probe.receipt['stage_counts'])
        self.probe.receipt['stages']['A'].update(executed=True, verified=True)
        self.probe.receipt['stages']['B']['executed'] = True
        self.probe.persist()
        self.assertEqual({'executed': 2, 'verified': 1, 'unverified': 1, 'not_executed': 4}, self.probe.receipt['stage_counts'])
        self.assertNotIn('target_counts', self.probe.receipt)
        self.assertNotIn('target_counts', self.probe.index_receipt)
        self.assertEqual({'executed': 0, 'verified': 0}, self.probe.index_receipt['case_counts'])
        for step in self.probe.index_receipt['steps'].values():
            step.update(executed=True, verified=True)
        self.probe.index_receipt['recovery_tested'] = True
        self.probe.persist()
        self.assertEqual({'executed': 1, 'verified': 1}, self.probe.index_receipt['case_counts'])


class NativePreparationTests(unittest.TestCase):
    def test_rollup_race_requires_all_ten_and_no_skip(self):
        receipt = gate.verify(events_for(gate.ROLLUP_TESTS), 0, 'rollup-race')
        self.assertEqual({'run': 10, 'pass': 10, 'fail': 0, 'skip': 0}, receipt['target_counts'])
        for events in [events_for(gate.ROLLUP_TESTS[:-1]), events_for(gate.ROLLUP_TESTS, 'skip'),
                       events_for(gate.ROLLUP_TESTS) + [{'Action': 'build-fail'}]]:
            with self.subTest(count=len(events)), self.assertRaises(ValueError):
                gate.verify(events, 0, 'rollup-race')

    def test_old_sql_contract_comes_from_exact_d78_source_object(self):
        root = Path(__file__).resolve().parents[2]
        with tempfile.TemporaryDirectory() as directory:
            source = Path(directory)
            repository = source / 'backend/internal/repository'
            repository.mkdir(parents=True)
            for name in ('usage_log_repo_insert.go', 'usage_log_repo_query.go'):
                data = subprocess.check_output(['git', 'show', oldboot.OLD_SHA + ':backend/internal/repository/' + name], cwd=root)
                (repository / name).write_bytes(data)
            sql, projection, names, proof = oldboot.old_usage_contract(source)
            self.assertEqual(65, len(names))
            self.assertEqual(['id'] + names, projection)
            self.assertEqual(66, proof['select_columns'])
            self.assertNotIn('cache_creation_30m_tokens', sql)
            self.assertNotIn('cache_creation_12h_tokens', sql)

    def test_unexecuted_stages_and_invalid_recovery_stay_false(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            artifacts = root / 'artifacts'
            artifacts.mkdir()
            probe = oldboot.Probe(root, artifacts)
            try:
                self.assertFalse(probe.receipt['verified'])
                self.assertFalse(probe.receipt['process_E_executed'])
                self.assertFalse(probe.index_receipt['recovery_tested'])
                for value in probe.receipt['stages'].values():
                    self.assertEqual({'executed': False, 'verified': False}, value)
            finally:
                import shutil
                shutil.rmtree(probe.work)

    def test_tracked_gate_rejects_untracked_staged_and_dirty_inputs(self):
        from unittest.mock import patch
        helper = load_helper('v0222_evidence')
        with tempfile.TemporaryDirectory() as directory:
            root, name = Path(directory), 'input.py'
            target = root / name
            target.write_bytes(b'committed')
            def fake_git(_root, *arguments):
                if arguments[:2] == ('ls-files', '--error-unmatch'):
                    return b'input.py\n'
                if arguments[:2] == ('ls-files', '--stage'):
                    return b'100644 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa 0\tinput.py\n'
                if arguments[:1] == ('show',):
                    return b'committed'
                raise AssertionError(arguments)
            with patch.object(helper, 'git', side_effect=fake_git):
                self.assertEqual({name: hashlib.sha256(b'committed').hexdigest()}, helper.tracked_inputs(root, [target]))
                target.write_bytes(b'dirty')
                with self.assertRaises(ValueError):
                    helper.tracked_inputs(root, [target])
                target.write_bytes(b'committed')
            with patch.object(helper, 'git', side_effect=subprocess.CalledProcessError(1, ['git'])):
                with self.assertRaises(subprocess.CalledProcessError):
                    helper.tracked_inputs(root, [target])
            def staged_git(_root, *arguments):
                return b'staged-change' if arguments == ('show', ':' + name) else fake_git(_root, *arguments)
            with patch.object(helper, 'git', side_effect=staged_git), self.assertRaises(ValueError):
                helper.tracked_inputs(root, [target])

    def test_capture_refuses_local_without_native_command(self):
        with tempfile.TemporaryDirectory() as directory:
            env = dict(os.environ)
            env.pop('CI', None)
            env.pop('GITHUB_ACTIONS', None)
            target = Path(directory) / 'execution.json'
            result = subprocess.run([sys.executable, '-I', '-B',
                str(Path(__file__).with_name('v0222_capture.py')), '--events', str(Path(directory) / 'events'),
                '--stderr', str(Path(directory) / 'stderr'), '--execution', str(target), '--',
                'go', 'test', '-json', '-race'], env=env, capture_output=True, timeout=10)
            self.assertNotEqual(0, result.returncode)
            self.assertFalse(json.loads(target.read_text())['verified'])
            self.assertFalse((Path(directory) / 'events').exists())

    def test_migration_summary_binds_counts_source_and_raw_hashes(self):
        summary = load_helper('v0222_migration_evidence')
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            context = {'source_commit': 'a' * 40, 'github_sha': 'a' * 40,
                       'runner_version': 'v0222-ci-v8.1', 'inputs': {}}
            common = {**context, 'verified': True, 'native_execution': True, 'exit_code': 0, 'commands': []}
            old = {**common, 'stages': {name: {'executed': True, 'verified': True} for name in 'ABCDEF'},
                   'process_E_executed': True, 'owned_disposable_cleanup_verified': True}
            index = {**common, 'recovery_tested': True, 'invalid_before_retry': {'valid': False, 'oid': '1'},
                     'valid_after_retry': {'valid': True, 'oid': '2'}, 'cancellation_exit_code': 1,
                     'old_ledger_preserved': True, 'atlas_preserved': True}
            commands = []
            for stage in ('red', 'green', 'rollup-race'):
                events, errors = root / (stage + '.jsonl'), root / (stage + '.stderr')
                events.write_bytes(b'fixture events')
                errors.write_bytes(b'')
                counts = {'run': 2 if stage == 'red' else 10, 'pass': 0 if stage == 'red' else 10,
                          'fail': 2 if stage == 'red' else 0, 'skip': 0}
                run = {'stage': stage, 'verified': True, 'events_path': events.name,
                       'stderr_path': errors.name, 'events_sha256': hashlib.sha256(events.read_bytes()).hexdigest(),
                       'stderr_sha256': hashlib.sha256(b'').hexdigest(), 'command': ['go', 'test'],
                       'exit_code': 1 if stage == 'red' else 0, 'target_counts': counts}
                commands.append(run)
                (root / ('rollup-' + stage + '-receipt.json')).write_text(json.dumps(
                    {**context, 'verified': True, 'execution': run, 'target_counts': counts}), encoding='utf-8')
            for name, value in [('old-app-probe.json', old), ('invalid-index-recovery.json', index),
                                ('rollup-execution.json', {**common, 'commands': commands})]:
                (root / name).write_text(json.dumps(value), encoding='utf-8')
            result = summary.summarize(root, root, context)
            self.assertEqual(4, result['case_counters']['executed'])
            self.assertEqual(1, result['case_counters']['invalid_index_recovery'])
            (root / 'red.jsonl').write_bytes(b'corrupted')
            with self.assertRaises(ValueError):
                summary.summarize(root, root, context)

    def test_migration_summary_rejects_missing_native_evidence(self):
        summary = load_helper('v0222_migration_evidence')
        with tempfile.TemporaryDirectory() as directory, self.assertRaises(ValueError):
            summary.summarize(Path(directory), Path(directory), {'source_commit': 'a' * 40})

    def test_single_multiline_and_dash_run_are_all_extracted(self):
        syntax = load_helper('v0222_check_syntax')
        text = 'steps:\n  - run: printf fixture\n  - name: block\n    run: |\n      true\n      false\n  - run: >-\n      true\n      false\n'
        result = syntax.run_blocks(text)
        self.assertEqual(3, len(result))
        self.assertEqual('printf fixture\n', result[0][1])
        self.assertEqual('true\nfalse\n', result[1][1])
        self.assertEqual('true false\n', result[2][1])


class WorkflowWiringTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.root = Path(__file__).resolve().parents[2]
        cls.publish = (cls.root / '.github/workflows/publish-personal-ghcr.yml').read_text(encoding='utf-8')
        cls.ci = (cls.root / '.github/workflows/backend-ci.yml').read_text(encoding='utf-8')

    def test_canonical_config_and_store_local_id_are_separate(self):
        self.assertIn('id: runtime_build', self.publish)
        self.assertIn('BUILD_METADATA: ${{ steps.runtime_build.outputs.metadata }}', self.publish)
        self.assertIn('OCI_LOCAL_IMAGE_ID=${canonical_config_digest}', self.publish)
        self.assertIn('OCI_DOCKER_LOCAL_IMAGE_ID=${runtime_image_id}', self.publish)
        self.assertNotIn('OCI_LOCAL_IMAGE_ID=${runtime_image_id}', self.publish)
        self.assertIn('"$current_image_id" == "$OCI_DOCKER_LOCAL_IMAGE_ID"', self.publish)
        self.assertIn('"$current_latest_image_id" == "$OCI_DOCKER_LOCAL_IMAGE_ID"', self.publish)
        self.assertIn('"${IMAGE_NAME}@${version_digest}" > "${identity_dir}/registry-manifest.json"', self.publish)
        self.assertIn('--build-receipt "${PROVENANCE_WORK_DIR}/build-identity.json"', self.publish)
        self.assertIn('--arg oci_local_image_id "$OCI_LOCAL_IMAGE_ID"', self.publish)

    def test_sse_race_json_gate_is_wired_in_both_workflows(self):
        self.assertIn('|V0222SSEFastPath|V0222GatewayRetry|V8OpenAI|UsageLogV0222Persistence|UsageBillingV0222Apply|V10Cache|SessionMemory|SSEStreamReaderLifecycle|OpenAIInputTokensLiveBoundary|OpenAIWSPassthroughFirstTokenFollowsTTFTMode|OpenAIWSPassthroughTTFTOriginIncludesAdmission|OpenAIWSTTFTModeConn\'', self.publish)
        self.assertIn("-run '^Test(V0222(SSEFastPath|GatewayRetry)|SSEStreamReaderLifecycle)'", self.ci)
        for workflow in (self.publish, self.ci):
            self.assertIn('v0222_test_receipt.py --mode retry', workflow)
            self.assertNotIn('continue-on-error', workflow)
        for workflow in [self.publish, self.ci]:
            self.assertIn('v0222_test_receipt.py --mode sse', workflow)
            self.assertIn('--exit-code "$race_exit"', workflow)
            self.assertIn('go test -json -mod=readonly', workflow)

    def test_identity_artifact_does_not_change_legacy_two_entries(self):
        entries = self.publish.split('- name: Upload release provenance', 1)[1].split('- name:', 1)[0]
        self.assertIn('release-provenance/release-manifest.json', entries)
        self.assertIn('release-provenance/asset-manifest.json', entries)
        self.assertNotIn('registry-manifest.json', entries)
        self.assertNotIn('image-identity.json', entries)
        self.assertIn('name: release-image-identity', self.publish)

    def test_preserve_original_race_packages_and_every_regex_branch(self):
        original = subprocess.check_output(['git', 'show', 'HEAD:.github/workflows/publish-personal-ghcr.yml'],
                                           cwd=self.root, text=True)
        import re
        old_regex = re.search(r"-run '([^']+)'", original)[1]
        new_regex = re.search(r"-run '([^']+)'", self.publish)[1]
        self.assertTrue(set(old_regex.split('|')) <= set(new_regex.split('|')))
        for package in re.findall(r'\./internal/[a-zA-Z0-9_/]+', original.split('Race regression tests', 1)[1].split('- name:', 1)[0]):
            self.assertIn(package, self.publish)

    def test_openai_v8_seven_top_level_contracts_are_in_wide_race(self):
        import re
        source = (self.root / 'backend/internal/service/account_test_service_openai_v8_contract_test.go').read_text(encoding='utf-8')
        tests = re.findall(r'^func (TestV8OpenAI\w+)\(', source, re.M)
        self.assertEqual(7, len(tests))
        selector = re.search(r"-run '([^']+)'", self.publish)[1]
        self.assertIn('V8OpenAI', selector.split('|'))
        self.assertTrue(all(re.search(selector, name) for name in tests))
        self.assertIn('V0222SSEFastPath', selector.split('|'))
        self.assertIn('V0222GatewayRetry', selector.split('|'))

    def test_persistence_all_five_exact_unit_contracts_match_original_wide_race(self):
        import re
        repository = self.root / 'backend/internal/repository'
        expected = {
            'TestUsageLogV0222PersistenceIndependent67ParameterAnd68ColumnContract',
            'TestUsageLogV0222PersistenceMultirowArgumentsKeepFullStride',
            'TestUsageLogV0222PersistenceRealInsertEntrypointsAndFallback',
            'TestUsageLogV0222PersistenceScan68KeepsCostsTTLAndComposite',
            'TestUsageBillingV0222ApplyTransactionAndIdempotencyContract',
        }
        declared = set()
        for filename in ('usage_log_v0222_persistence_contract_unit_test.go',
                         'usage_billing_v0222_apply_contract_unit_test.go'):
            declared.update(re.findall(r'^func (Test\w+)\(', (repository / filename).read_text(encoding='utf-8'), re.M))
        self.assertEqual(expected, declared)
        selector = re.search(r"-run '([^']+)'", self.publish)[1]
        self.assertTrue(all(re.search(selector, name) for name in declared))
        self.assertIn('UsageLogV0222Persistence', selector.split('|'))
        self.assertIn('UsageBillingV0222Apply', selector.split('|'))
        self.assertIn('V8OpenAI', selector.split('|'))

    def test_same_request_pg_race_is_separate_exact_command_in_both_workflows(self):
        import re
        path = self.root / 'backend/internal/repository/usage_billing_v0222_same_request_integration_test.go'
        source = path.read_text(encoding='utf-8')
        self.assertEqual(list(gate.BILLING_RACE_TESTS), re.findall(r'^func (Test\w+)\(', source, re.M))
        self.assertRegex(source, r'\[\]string\{"repository", "lane"\}')
        self.assertIn('const count = 32', source)
        for workflow in (self.ci, self.publish):
            self.assertIn("-run '" + gate.BILLING_RACE_SELECTOR + "' -count=1 -timeout=5m", workflow)
            block = workflow.split('- name: Same-request billing repository and lane integration race', 1)[1].split('- name:', 1)[0]
            self.assertIn('go test -json -mod=readonly -p=2 -race -tags=integration ./internal/repository', block)
            self.assertIn('v0222_capture.py', block)
            self.assertIn('v0222_test_receipt.py --mode billing-race', block)
            self.assertIn('billing-race.jsonl', block)
            self.assertIn('billing-race.stderr', block)
            self.assertIn('billing-race-execution.json', block)
            self.assertIn('billing-race-receipt.json', block)
            self.assertIn('race_exit=$?', block)
            self.assertIn('--exit-code "$race_exit"', block)
        rollup = (self.root / 'tools/ci/v0222_rollup_red_green.py').read_text(encoding='utf-8')
        self.assertNotIn(gate.BILLING_RACE_TESTS[0], rollup)
        self.assertEqual(10, len(gate.ROLLUP_TESTS))
        self.assertEqual((), gate.ROLLUP_ADDITIONAL_TESTS)

    def test_race_input_manifest_explicitly_contains_new_storage_sources(self):
        helper = load_helper('v0222_evidence')
        names = {path.relative_to(self.root).as_posix() for path in helper.race_inputs(self.root)}
        for name in ('usage_log_v0222_persistence_contract_unit_test.go',
                     'usage_billing_v0222_apply_contract_unit_test.go',
                     'usage_billing_v0222_same_request_integration_test.go',
                     'integration_harness_test.go', 'usage_billing_batch_repo_integration_test.go'):
            self.assertIn('backend/internal/repository/' + name, names)
        self.assertIn('backend/go.mod', names)
        self.assertIn('backend/go.sum', names)

    def test_cache_race_exact_two_package_commands_and_raw_receipts_in_both(self):
        for workflow in (self.ci, self.publish):
            block = workflow.split('- name: Cache proof and accounting two-package race', 1)[1].split('- name:', 1)[0]
            self.assertIn('go test -json -mod=readonly -p=2 -race -tags=unit ./internal/service ./internal/pkg/apicompat', block)
            self.assertIn("-run '^Test(V10Cache|V8OpenAICache)' -count=1 -timeout=180s", block)
            self.assertIn('v0222_capture.py', block)
            self.assertIn('v0222_test_receipt.py --mode cache-race', block)
            self.assertIn('race_exit=$?', block)
            self.assertIn('--exit-code "$race_exit"', block)
            for name in ('cache-race.jsonl', 'cache-race.stderr', 'cache-race-execution.json', 'cache-race-receipt.json'):
                self.assertIn(name, block)

    def test_cache_and_sessionmemory_wide_intersections_minimal_prefix_addition(self):
        import re
        manifest = gate.cache_manifest
        manifest.check_sources(self.root)
        selector = re.search(r"-run '([^']+)'", self.publish)[1]
        branches = selector.split('|')
        self.assertIn('V10Cache', branches)
        self.assertIn('SessionMemory', branches)
        self.assertIn('V8OpenAI', branches)
        self.assertNotIn('V8OpenAICache', branches, 'already covered by V8OpenAI; do not duplicate')
        for package, cases in manifest.CACHE_CASES.items():
            for name in cases:
                self.assertRegex(name, selector)
        for name in manifest.SESSION_MEMORY_TESTS:
            self.assertRegex(name, selector)
        self.assertEqual(6, len(manifest.SESSION_MEMORY_TESTS))
        self.assertIn('v0222_test_receipt.py --mode session-memory', self.publish)
        self.assertIn('session-memory-race-receipt.json', self.publish)

    def test_all_cache_products_tests_and_helpers_are_explicit_race_inputs(self):
        helper = load_helper('v0222_evidence')
        names = {path.relative_to(self.root).as_posix() for path in helper.race_inputs(self.root)}
        expected = set(gate.cache_manifest.CACHE_TEST_SOURCE_SHA256) | set(gate.cache_manifest.CACHE_PRODUCT_FILES)
        expected |= set(gate.cache_manifest.CACHE_EXISTING_TEST_FILES)
        self.assertTrue(expected <= names)
        self.assertFalse(set(gate.cache_manifest.CACHE_DELETED_FILES) & names)
        self.assertIn('backend/internal/service/account_test_service_openai_v8_contract_test.go', names)
        self.assertIn('backend/internal/repository/usage_billing_v0222_same_request_integration_test.go', names)
        self.assertTrue(all((self.root / name).is_file() for name in expected))

    def test_boundary_live_exact_isolated_commands_receipts_and_always_upload_in_both(self):
        import re
        import shlex
        for workflow, directory in ((self.ci, 'v0222-ci-evidence'), (self.publish, 'v0222-publish-race-evidence')):
            block = workflow.split('- name: Isolated live-boundary race,', 1)[1].split('- name:', 1)[0]
            flattened = block.replace('\\\n', ' ')
            command = shlex.split(re.search(r'go test [^\n]+', flattened)[0])
            self.assertEqual(gate.LIVE_BOUNDARY_COMMAND, command)
            self.assertIn('working-directory: backend', block)
            self.assertIn('v0222_capture.py', block)
            self.assertIn('v0222_test_receipt.py --mode live-boundary', block)
            self.assertIn('race_exit=$?', block)
            self.assertIn('--exit-code "$race_exit"', block)
            for name in ('live-boundary-race.jsonl', 'live-boundary-race.stderr',
                         'live-boundary-race-execution.json', 'live-boundary-race-receipt.json'):
                self.assertIn('../' + directory + '/' + name, block)
            self.assertNotIn('TestEstimateOpenAIInputTokens_CompareWithOpenAIAPI', workflow)
            self.assertNotIn('OPENAI_INPUT_TOKENS_LIVE_TEST', workflow)
            self.assertNotIn('OPENAI_API_KEY', workflow)
            self.assertIn('name: ' + ('v0222-ci-compatibility-evidence' if workflow == self.ci else directory), workflow)
            upload = workflow.split('uses: actions/upload-artifact@v4', 1)[0]
            self.assertIn('if: always()', upload)

    def test_lifecycle_both_exact_command_sources_and_original_sse_retry_counts_unchanged(self):
        import re
        import shlex
        for workflow, label, expected in (
                (self.ci, 'Named-event SSE, retry and reader lifecycle race contracts', gate.SSE_LIFECYCLE_COMMAND),
                (self.publish, 'Race regression tests', gate.PUBLISH_LIFECYCLE_COMMAND)):
            block = workflow.split('- name: ' + label, 1)[1].split('- name:', 1)[0]
            command = shlex.split(re.search(r'go test [^\n]+', block.replace('\\\n', ' '))[0])
            self.assertEqual(expected, command)
            self.assertIn('v0222_test_receipt.py --mode sse-lifecycle', block)
            self.assertIn('sse-lifecycle-race-receipt.json', block)
            self.assertIn('--stderr ', block)
            self.assertIn('v0222_test_receipt.py --mode sse ', block)
            self.assertIn('v0222_test_receipt.py --mode retry ', block)
        self.assertEqual(32, len(gate.SSE_LEAVES))
        self.assertEqual(8, len(gate.RETRY_LEAVES))
        self.assertTrue(all(re.search(gate.SSE_LIFECYCLE_SELECTOR, name)
                            for name in gate.SSE_TESTS + gate.RETRY_TESTS + gate.LIFECYCLE_TESTS))
        self.assertFalse(gate.SSE_LIFECYCLE_SELECTOR.endswith('$'))
        selector = re.search(r"-run '([^']+)'", self.publish)[1]
        self.assertEqual(gate.PUBLISH_RACE_SELECTOR, selector)
        self.assertEqual(['SSEStreamReaderLifecycle', 'OpenAIInputTokensLiveBoundary'], selector.split('|')[-5:-3])
        self.assertEqual(['OpenAIWSPassthroughFirstTokenFollowsTTFTMode', 'OpenAIWSPassthroughTTFTOriginIncludesAdmission',
                          'OpenAIWSTTFTModeConn'], selector.split('|')[-3:])

    def test_five_boundary_sources_are_explicit_tracked_input_contract_and_actual_declared_tops(self):
        import re
        helper = load_helper('v0222_evidence')
        names = {path.relative_to(self.root).as_posix() for path in helper.race_inputs(self.root)}
        for name in BoundaryEvidenceTests.source_names:
            path = 'backend/internal/service/' + name
            self.assertIn(path, names)
            self.assertIn(path, gate.BOUNDARY_SOURCE_FILES)
        for filename, top in (('gateway_stream_reader_lifecycle_test.go', gate.LIFECYCLE_TESTS[0]),
                              ('openai_input_tokens_live_boundary_test.go', gate.LIVE_BOUNDARY_TESTS[0])):
            source = (self.root / 'backend/internal/service' / filename).read_text(encoding='utf-8')
            self.assertIn(top, re.findall(r'^func (Test\w+)\(', source, re.M))
        live = (self.root / 'backend/internal/service/openai_input_tokens_live_boundary_test.go').read_text(encoding='utf-8')
        scenarios = re.findall(r'\{name: "([^"]+)"', live.split('if childScenario', 1)[0])
        self.assertEqual(list(BoundaryEvidenceTests.live_cases), scenarios)
        for snippet in ("'ls-files', '--error-unmatch'", "'ls-files', '--stage'", "'HEAD:' + relative", "':' + relative",
                        'path.read_bytes() == committed', 'staged == committed'):
            self.assertIn(snippet, Path(helper.__file__).read_text(encoding='utf-8'))

    def test_ws_ttft_exact_independent_race_steps_and_named_raw_receipts_in_both(self):
        import re
        import shlex
        for workflow, directory in ((self.ci, 'v0222-ci-evidence'), (self.publish, 'v0222-publish-race-evidence')):
            block = workflow.split('- name: WS TTFT mode and admission origin race,', 1)[1].split('- name:', 1)[0]
            command = shlex.split(re.search(r'go test [^\n]+', block.replace('\\\n', ' '))[0])
            self.assertEqual(WSTTFTEvidenceTests.command, command)
            self.assertIn('working-directory: backend', block)
            self.assertIn('v0222_capture.py', block)
            self.assertIn('v0222_test_receipt.py --mode ws-ttft', block)
            self.assertIn('race_exit=$?', block)
            self.assertIn('--exit-code "$race_exit"', block)
            self.assertIn('set +e', block)
            self.assertIn('set -e', block)
            self.assertIn('--stderr ../' + directory + '/ws-ttft-race.stderr', block)
            for name in ('ws-ttft-race.jsonl', 'ws-ttft-race.stderr', 'ws-ttft-race-execution.json', 'ws-ttft-race-receipt.json'):
                self.assertIn('../' + directory + '/' + name, block)
            self.assertNotIn('-overlay', block)
            self.assertNotIn('mutant', block)
            self.assertNotIn('continue-on-error', workflow)
        wide = self.publish.split('- name: Race regression tests', 1)[1].split('- name:', 1)[0]
        self.assertNotIn('--mode ws-ttft', wide, 'wide raw cannot substitute the exact dedicated receipt')

    def test_ws_ttft_wide_tail_three_prefixes_preserve_exact_thirteen_packages_and_sse_selector(self):
        import re
        import shlex
        block = self.publish.split('- name: Race regression tests', 1)[1].split('- name:', 1)[0]
        command = shlex.split(re.search(r'go test [^\n]+', block.replace('\\\n', ' '))[0])
        self.assertEqual(gate.PUBLISH_LIFECYCLE_COMMAND, command)
        self.assertEqual(list(gate.PUBLISH_RACE_PACKAGES), [arg for arg in command if arg.startswith('./internal/')])
        self.assertEqual(13, len(gate.PUBLISH_RACE_PACKAGES))
        branches = command[command.index('-run') + 1].split('|')
        prefixes = ['OpenAIWSPassthroughFirstTokenFollowsTTFTMode', 'OpenAIWSPassthroughTTFTOriginIncludesAdmission', 'OpenAIWSTTFTModeConn']
        self.assertEqual(prefixes, branches[-3:])
        self.assertEqual(tuple(prefixes), gate.WS_TTFT_WIDE_PREFIXES)
        self.assertEqual(['SSEStreamReaderLifecycle', 'OpenAIInputTokensLiveBoundary'], branches[-5:-3])
        for top in WSTTFTEvidenceTests.cases:
            self.assertRegex(top, gate.PUBLISH_RACE_SELECTOR)
        self.assertIn("-run '^Test(V0222(SSEFastPath|GatewayRetry)|SSEStreamReaderLifecycle)' -count=1 -timeout=2m", self.ci)
        self.assertEqual((32, 8, 6, 16, 10), (len(gate.SSE_LEAVES), len(gate.RETRY_LEAVES),
                         len(gate.LIFECYCLE_LEAVES), len(gate.LIVE_BOUNDARY_LEAVES), len(gate.ROLLUP_TESTS)))
        self.assertEqual((), gate.ROLLUP_ADDITIONAL_TESTS)

    def test_ws_ttft_all_reviewed_sourceinput_paths_are_explicit_without_reading_active_test(self):
        helper = load_helper('v0222_evidence')
        names = {path.relative_to(self.root).as_posix() for path in helper.race_inputs(self.root)}
        self.assertEqual(WSTTFTEvidenceTests.source_files, gate.WS_TTFT_SOURCE_FILES)
        self.assertTrue(set(WSTTFTEvidenceTests.source_files) <= names)
        self.assertIn('backend/go.mod', names)
        self.assertIn('backend/go.sum', names)
        # 此fixture不读活动WS test或拿其半写内容做hash/freeze；真实inputs仍由native tracked gate逐byte核验。
        for path in WSTTFTEvidenceTests.source_files[1:]:
            self.assertTrue((self.root / path).is_file())
        source = Path(helper.__file__).read_text(encoding='utf-8')
        for snippet in ("'ls-files', '--error-unmatch'", "'ls-files', '--stage'", "'HEAD:' + relative", "':' + relative",
                        'path.read_bytes() == committed', 'staged == committed'):
            self.assertIn(snippet, source)
        capture = (self.root / 'tools/ci/v0222_capture.py').read_text(encoding='utf-8')
        for name in ('OPENAI_INPUT_TOKENS_LIVE_TEST', 'OPENAI_API_KEY', 'SUB2API_OPENAI_INPUT_TOKENS_BOUNDARY_CHILD'):
            self.assertIn(name, capture)
        self.assertIn("env.pop(name, None)", capture)

    def test_native_ci_keeps_normal_suites_and_uploads_actual_attempts(self):
        self.assertIn('run: make test-unit', self.ci)
        self.assertIn('run: make test-integration', self.ci)
        self.assertIn('v0222_rollup_red_green.py --artifacts v0222-ci-evidence', self.ci)
        self.assertIn('v0222_old_app_probe.py --artifacts v0222-ci-evidence --execute-isolated-ci-probe', self.ci)
        self.assertIn("CI: 'true'", self.ci)
        self.assertIn('if: always()', self.ci)
        self.assertIn('if-no-files-found: error', self.ci)
        self.assertIn("^TestCompositeUsage", self.publish)


if __name__ == '__main__':
    unittest.main()
