"""Fail-closed go-test JSON gates; an arbitrary nonzero is never a genuine RED."""
import argparse
import importlib.util
import json
from pathlib import Path
import re
import sys

_spec = importlib.util.spec_from_file_location('v0222_evidence', Path(__file__).with_name('v0222_evidence.py'))
evidence = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(evidence)
cache_manifest = evidence.cache_manifest

ROLLUP_PREFIX = 'TestCompositeUsageV0222Rollback'
ROLLUP_TESTS = tuple(ROLLUP_PREFIX + suffix for suffix in (
    'OldSQLAndAttributionRoundTrip', 'OldBackgroundBeforeNewBegin',
    'OldBackgroundDuringNewCompute', 'OldBackgroundBetweenRegisterAndCompute',
    'FailedCandidateOldReadsAndNewRecovery', 'ExpiredFloorDeleteConsistency',
    'CleanupWindowWithOldBackground', 'TimezoneResetKeepsPartialRetainedDay',
    'MigrationPredicateAndMetadataTransitions', 'WriterTimezoneCoversStoredBucketDate'))
RED_TESTS = tuple(ROLLUP_PREFIX + suffix for suffix in (
    'ExpiredFloorDeleteConsistency', 'CleanupWindowWithOldBackground'))
SSE_PREFIX = 'TestV0222SSEFastPath'
SSE_TESTS = tuple(SSE_PREFIX + suffix for suffix in (
    'VisibleOutput', 'FirstVisibleBoundary', 'TerminalFixedTail', 'StopReasonIsNotTailStart',
    'EventNameCannotInventTerminal', 'DisconnectPreservesUsageWithoutMoreWrites',
    'FullReadersDisconnectToolTail', 'FullReadersFinalizeWriteFailure'))
SSE_LEAVES = (
    tuple(SSE_PREFIX + 'VisibleOutput/' + form + '/' + case
          for form in ('data_only', 'event_and_data')
          for case in ('text', 'tool_input', 'zero_arg_complete_tool', 'incomplete_tool',
                       'thinking_only', 'server_tool_not_client_tool'))
    + tuple(SSE_PREFIX + 'FirstVisibleBoundary/' + form + '/' + case
            for form in ('data_only', 'event_and_data')
            for case in ('at_8MiB', 'over_8MiB', 'visible_then_9MiB_line',
                         'unterminated_line_over_8MiB_without_EOF'))
    + tuple(SSE_PREFIX + 'TerminalFixedTail/' + case
            for case in ('named_message_stop', 'data_only_message_stop', 'DONE'))
    + tuple(SSE_PREFIX + case for case in ('StopReasonIsNotTailStart',
            'EventNameCannotInventTerminal', 'DisconnectPreservesUsageWithoutMoreWrites'))
    + tuple(SSE_PREFIX + 'FullReadersDisconnectToolTail/' + reader + '/' + ending
            for reader in ('responses', 'chat_completions')
            for ending in ('terminal', 'post_terminal_error'))
    + tuple(SSE_PREFIX + 'FullReadersFinalizeWriteFailure/' + reader
            for reader in ('responses', 'chat_completions'))
)
RETRY_TESTS = ('TestV0222GatewayRetryRectificationBeforeAccountPolicy',)
RETRY_LEAVES = tuple(RETRY_TESTS[0] + '/' + endpoint + '/' + case
    for endpoint in ('responses', 'chat/completions')
    for case in ('rectification_succeeds', 'final_nondefault_status_disables',
                 'repeated_thinking_400_is_final', 'rotation_then_rectification'))
BILLING_RACE_TESTS = ('TestCompositeUsageV0222BillingSameRequestConcurrentApplyOnce',)
BILLING_RACE_LEAVES = tuple(BILLING_RACE_TESTS[0] + '/' + path for path in ('repository', 'lane'))
BILLING_RACE_SELECTOR = '^' + BILLING_RACE_TESTS[0] + '$'
REPOSITORY = 'github.com/Wei-Shaw/sub2api/internal/repository'
SERVICE = 'github.com/Wei-Shaw/sub2api/internal/service'
LIFECYCLE_TESTS = ('TestSSEStreamReaderLifecycle',)
LIFECYCLE_LEAVES = tuple(LIFECYCLE_TESTS[0] + '/' + case for case in (
    'blocked_Read_join', 'send_blocked_cancel', 'budget_acquire_cancel',
    'first_visible_barrier_cancel', 'upstream_error', 'natural_EOF'))
SSE_LIFECYCLE_SELECTOR = '^Test(V0222(SSEFastPath|GatewayRetry)|SSEStreamReaderLifecycle)'
SSE_LIFECYCLE_COMMAND = ['go', 'test', '-json', '-mod=readonly', '-p=2', '-race', '-tags=unit',
                         './internal/service', '-run', SSE_LIFECYCLE_SELECTOR, '-count=1', '-timeout=2m']
LIVE_BOUNDARY_TESTS = ('TestOpenAIInputTokensLiveBoundary',)
LIVE_BOUNDARY_SELECTOR = '^' + LIVE_BOUNDARY_TESTS[0] + '$'
LIVE_BOUNDARY_COMMAND = ['go', 'test', '-json', '-mod=readonly', '-p=2', '-race', '-tags=unit',
                         './internal/service', '-run', LIVE_BOUNDARY_SELECTOR, '-count=1', '-timeout=180s']
# 固定源码合同：HTTP调用数、child原entry终态、三个原leaf终态、child退出码。
# outer leaf全部必须PASS；表内FAIL/SKIP仅是受隔离child的预期数据。
LIVE_SCENARIOS = {
    'without_opt_in': (0, 'SKIP', None, 0),
    'zero_opt_in': (0, 'SKIP', None, 0),
    'true_opt_in': (0, 'SKIP', None, 0),
    'zero_padded_opt_in': (0, 'SKIP', None, 0),
    'spaced_opt_in': (0, 'SKIP', None, 0),
    'without_key': (0, 'SKIP', None, 0),
    'short_placeholder_key': (0, 'SKIP', None, 0),
    'explicit_opt_in': (3, 'PASS', 'PASS', 0),
    'out_of_tolerance': (3, 'FAIL', 'FAIL', 1),
    'unauthorized': (3, 'PASS', 'SKIP', 0),
    'forbidden': (3, 'PASS', 'SKIP', 0),
    'network_error': (3, 'PASS', 'SKIP', 0),
    'server_error': (3, 'FAIL', 'FAIL', 1),
    'unexpected_redirect': (3, 'FAIL', 'FAIL', 1),
    'missing_count': (3, 'FAIL', 'FAIL', 1),
    'read_error': (3, 'FAIL', 'FAIL', 1),
}
LIVE_BOUNDARY_LEAVES = tuple(LIVE_BOUNDARY_TESTS[0] + '/' + name for name in LIVE_SCENARIOS)
LIVE_CHILD_LEAVES = ('simple_user_text', 'system_plus_tool', 'multi_turn_text')
LIVE_EVIDENCE_FIELDS = {'scenario', 'http_calls', 'entry_status', 'leaf_status', 'child_exit', 'sensitive_output'}
LIFECYCLE_SNAPSHOT = {'body_closed': True, 'reader_completed': True, 'active_reads': 0,
                       'budget_known': True, 'queued_bytes': 0}
BOUNDARY_SOURCE_FILES = tuple('backend/internal/service/' + name for name in (
    'gateway_upstream_response.go', 'gateway_v0222_fastpath_contract_test.go',
    'gateway_stream_reader_lifecycle_test.go', 'openai_gateway_count_tokens_test.go',
    'openai_input_tokens_live_boundary_test.go'))
PUBLISH_RACE_PACKAGES = (
    './internal/requestdiagnostic', './internal/service', './internal/repository',
    './internal/handler', './internal/handler/admin', './internal/server/middleware', './internal/server/routes',
    './internal/config', './internal/pkg/httputil', './internal/pkg/adminaccess',
    './internal/managementmcp', './internal/pkg/apicompat', './internal/server')
WS_TTFT_TESTS = (
    'TestOpenAIWSPassthroughFirstTokenFollowsTTFTMode',
    'TestOpenAIWSPassthroughTTFTOriginIncludesAdmission', 'TestOpenAIWSTTFTModeConn')
WS_TTFT_LEAVES = (
    WS_TTFT_TESTS[0] + '/semantic', WS_TTFT_TESTS[0] + '/visible', WS_TTFT_TESTS[1],
    *[WS_TTFT_TESTS[2] + '/' + name for name in (
        'waits_for_each_successful_create', 'context_cancel_releases_waiter',
        'close_releases_waiter', 'write_failure_releases_waiter')])
# Origin top自身也是leaf：3top与7leaf的并集为9，不是10。
WS_TTFT_NODES = frozenset(WS_TTFT_TESTS + WS_TTFT_LEAVES)
WS_TTFT_SELECTOR = '^(' + '|'.join(WS_TTFT_TESTS) + ')$'
WS_TTFT_COMMAND = ['go', 'test', '-json', '-mod=readonly', '-p=2', '-race', '-tags=unit',
                   './internal/service', '-run', WS_TTFT_SELECTOR, '-count=1', '-timeout=180s']
WS_TTFT_SOURCE_FILES = tuple('backend/internal/service/' + name for name in (
    'openai_ws_v2_passthrough_ttft_mode_test.go', 'openai_ws_v2_passthrough_adapter.go',
    'openai_ws_v2/passthrough_relay.go', 'openai_ws_v2/entry.go', 'openai_ws_v2/caddy_adapter.go',
    'openai_gateway_passthrough.go', 'openai_ws_forwarder.go'))
WS_TTFT_WIDE_PREFIXES = (
    'OpenAIWSPassthroughFirstTokenFollowsTTFTMode', 'OpenAIWSPassthroughTTFTOriginIncludesAdmission',
    'OpenAIWSTTFTModeConn')
# 保留原宽选择器每个分支和顺序，只尾增已审核前缀；不能从当前workflow自动学习expected。
PUBLISH_RACE_SELECTOR = (
    'Composite|NativeCompaction|DeepSeek|OpenRouter|CacheTTL|CacheControl|UpstreamProtocol|'
    'ResponsesChatFallback|ResponsesNativeGuard|Diagnostic|Diagnostics|Capture|AnthropicVisibleOutput|'
    'AnthropicCompat|ForwardAsCompat|GatewayForwardStreamDrain|CommentOnlySSEOverload|V0218|Gzip|'
    'Compressed|PartialUsage|ServerTiming|OpenAIUsage|OpenAICyber|RecordCyberPolicy|ScopedAdmin|ImageCount|'
    'MCP|MCPOAuth|ToolCatalog|ToolResult|BodyMemory|RequestBody|ReadTimeout|WriteTimeout|UpstreamDrain|'
    'HTTPUpstreamDrain|SSEQueue|SSETransport|BoundedSSE|AnthropicNativePump|AnthropicNativeLinePump|'
    'GatewayStreamingUndelimited|GatewayRequestBodyReuse|ThinkingBlocksNeedFiltering|FilterThinkingBlocks|'
    'OpenAIRequestPatches|OpenAICompatBufferedReasoning|StreamAccumulation|AccountTestService|'
    'AntigravityBuild|AnthropicMessagesURL|MessageStop|TailUsage|PartialCachedUsage|CyberSession|'
    'UsageRecordAsync|UsageRecordContext|CCBufferedFromNativeAnthropic|CCStreamingFromNativeAnthropic|'
    'ResponsesBufferedFromNativeAnthropic|ResponsesStreamingFromNativeAnthropic|V0222SSEFastPath|'
    'V0222GatewayRetry|V8OpenAI|UsageLogV0222Persistence|UsageBillingV0222Apply|V10Cache|SessionMemory|'
    'SSEStreamReaderLifecycle|OpenAIInputTokensLiveBoundary|'
    'OpenAIWSPassthroughFirstTokenFollowsTTFTMode|OpenAIWSPassthroughTTFTOriginIncludesAdmission|'
    'OpenAIWSTTFTModeConn')
PUBLISH_LIFECYCLE_COMMAND = ['go', 'test', '-json', '-mod=readonly', '-p', '2', '-race', '-tags=unit',
    *PUBLISH_RACE_PACKAGES, '-run', PUBLISH_RACE_SELECTOR, '-count=1', '-timeout=10m']
LIVE_FORBIDDEN_OUTPUT = ('input-tokens-boundary-sensitive-marker', 'INPUT_TOKENS_BOUNDARY_HTTP_CALLS=',
                         'Authorization:', 'Bearer ', 'OPENAI_API_KEY=')


def require(condition, reason):
    if not condition:
        raise ValueError(reason)


# 存储 owner 若添加其它入口，必须在这里单独列名，不可由 selector 静默漏测。
ROLLUP_ADDITIONAL_TESTS = ()


def verify_cache(events, exit_code):
    require(exit_code == 0 and bool(events), 'cache race requires successful process with JSON events')
    require(all(isinstance(event, dict) for event in events), 'cache JSON event must be an object')
    require(not any(event.get('Action') in ('fail', 'skip', 'build-fail') for event in events),
            'cache race cannot contain failure, skip or build failure in either package')
    observed_packages = {event.get('Package') for event in events if event.get('Action') in
                         ('start', 'run', 'pass', 'output') and event.get('Package')}
    require(observed_packages == set(cache_manifest.CACHE_CASES), 'cache race requires the exact two packages')
    packages = {}
    for package, top_cases in cache_manifest.CACHE_CASES.items():
        runs, outcomes, package_outcomes = set(), {}, []
        expected = cache_manifest.nodes(package)
        for event in events:
            if event.get('Package') != package:
                continue
            name, action = event.get('Test'), event.get('Action')
            if name is None:
                if action in ('pass', 'fail', 'skip'):
                    package_outcomes.append(action)
                continue
            if action in ('run', 'pass', 'fail', 'skip'):
                require(name in expected, 'unexpected cache test/case; review the fixed manifest')
            if action == 'run':
                require(name not in runs, 'cache duplicate run')
                runs.add(name)
            elif action in ('pass', 'fail', 'skip'):
                require(name in runs and name not in outcomes, 'cache outcome missing prior run or duplicate terminal')
                outcomes[name] = action
        require(package_outcomes == ['pass'], 'cache package did not finish exactly once with pass')
        require(runs == expected and set(outcomes) == expected and set(outcomes.values()) == {'pass'},
                'cache fixed test matrix missing run/pass; zero selection is not success')
        leaves = {name for name in runs if not any(other.startswith(name + '/') for other in runs)}
        require(leaves == set(cache_manifest.leaves(package)), 'cache fixed leaf matrix mismatch')
        packages[package] = {'top_level_count': len(top_cases), 'leaf_count': len(leaves),
            'required_top_level_tests': list(top_cases), 'leaf_outcomes': {name: outcomes[name] for name in sorted(leaves)},
            'target_counts': {'run': len(runs), 'pass': len(outcomes), 'fail': 0, 'skip': 0},
            'observed_node_tests': sorted(runs)}
    return {'mode': 'cache-race', 'verified': True, 'process_exit': exit_code, 'packages': packages,
        'top_level_count': sum(value['top_level_count'] for value in packages.values()),
        'leaf_count': sum(value['leaf_count'] for value in packages.values()), 'skip_count': 0,
        'target_counts': {key: sum(value['target_counts'][key] for value in packages.values())
                          for key in ('run', 'pass', 'fail', 'skip')},
        'count_semantics': 'exact cache selector only; not expanded 419-leaf compatibility suite'}


def strict_json(text):
    def pairs(items):
        result = {}
        for key, value in items:
            require(key not in result, 'duplicate JSON member')
            result[key] = value
        return result
    return json.loads(text, object_pairs_hook=pairs)


def verify_boundary(events, exit_code, mode):
    live = mode == 'live-boundary'
    tops = LIVE_BOUNDARY_TESTS if live else LIFECYCLE_TESTS
    leaves = LIVE_BOUNDARY_LEAVES if live else LIFECYCLE_LEAVES
    expected = set(tops + leaves)
    marker = 'live_boundary_evidence ' if live else 'SSE reader lifecycle snapshot: '
    filename = 'openai_input_tokens_live_boundary_test.go' if live else 'gateway_stream_reader_lifecycle_test.go'
    pattern = re.compile(r'\s*' + re.escape(filename) + r':[1-9][0-9]*: ' + re.escape(marker) + r'(\{[^\n]*\})\n?')
    require(type(exit_code) is int and exit_code == 0 and bool(events), 'boundary requires successful process and JSON events')
    require(all(isinstance(event, dict) for event in events), 'boundary JSON event must be an object')
    require(not any(event.get('Action') in ('fail', 'build-fail') for event in events),
            'boundary raw cannot contain test/package failure or build failure')
    if live:
        require(not any(event.get('Action') == 'skip' for event in events), 'dedicated live boundary cannot skip')
    runs, outcomes, structured = set(), {}, {}
    package_started, package_finished = False, False
    for event in events:
        package, name, action = event.get('Package'), event.get('Test'), event.get('Action')
        text = event.get('Output', '')
        require(isinstance(text, str), 'boundary Output must be text')
        if live:
            require(package == SERVICE, 'live boundary requires only the exact service package')
            require(not any(value in text for value in LIVE_FORBIDDEN_OUTPUT), 'forbidden live boundary output')
        relevant = isinstance(name, str) and name.split('/', 1)[0] in tops
        # 同名目标放在错包、错Test或伪造的任意JSON，均不能补齐required集合。
        if relevant:
            require(package == SERVICE and name in expected, 'boundary wrong package or unreviewed test/leaf')
        if live and name is not None:
            require(name in expected, 'live boundary selected an unexpected test, including optional live entry')
        if package == SERVICE:
            if name is None:
                if action == 'start':
                    require(not package_started and not package_finished, 'duplicate boundary package start')
                    package_started = True
                if action in ('pass', 'fail', 'skip'):
                    require(not package_finished and set(outcomes) == expected and
                            set(outcomes.values()) == {'pass'} and action == 'pass',
                            'boundary package terminal missing tests, duplicate or not pass')
                    package_finished = True
            elif relevant:
                require(not package_finished, 'boundary test event after package terminal')
                if action == 'run':
                    require(name not in runs and (name in tops or (tops[0] in runs and tops[0] not in outcomes)),
                            'boundary duplicate run or leaf outside parent run')
                    runs.add(name)
                elif action in ('pass', 'fail', 'skip'):
                    require(name in runs and name not in outcomes and action == 'pass',
                            'boundary terminal without run, duplicate, failure or skip')
                    if name in tops:
                        require(set(structured) == set(leaves) and set(leaves) <= set(outcomes),
                                'boundary parent passed before all leaf evidence and terminals')
                    else:
                        require(name in structured, 'boundary leaf passed without same-test evidence')
                    outcomes[name] = action
        marked = marker in text
        # 原SSE叶也会打印snapshot；生命周期只消费新六叶，不能把原32混进新合同。
        if marked and (live or relevant or filename + ':' in text):
            require(action == 'output' and package == SERVICE and name in leaves and
                    name in runs and name not in outcomes and not package_finished,
                    'boundary evidence not emitted by its running exact leaf')
            require(name not in structured, 'duplicate boundary scenario evidence')
            match = pattern.fullmatch(text)
            require(match is not None, 'boundary evidence must be a single source-attributed JSON output line')
            value = strict_json(match[1])
            require(type(value) is dict, 'boundary evidence must be an object')
            if live:
                scenario = name.split('/', 1)[1]
                calls, entry, leaf, child_exit = LIVE_SCENARIOS[scenario]
                expected_leaves = {key: leaf for key in LIVE_CHILD_LEAVES} if leaf else {}
                require(set(value) == LIVE_EVIDENCE_FIELDS and value['scenario'] == scenario and
                        type(value['http_calls']) is int and value['http_calls'] == calls and
                        type(value['entry_status']) is str and value['entry_status'] == entry and
                        type(value['leaf_status']) is dict and value['leaf_status'] == expected_leaves and
                        all(type(status) is str for status in value['leaf_status'].values()) and
                        type(value['child_exit']) is int and value['child_exit'] == child_exit and
                        value['sensitive_output'] is False, 'live boundary scenario schema or fixed evidence mismatch')
            else:
                require(set(value) == set(LIFECYCLE_SNAPSHOT) and
                        all(type(value[key]) is type(want) and value[key] == want
                            for key, want in LIFECYCLE_SNAPSHOT.items()),
                        'lifecycle exit snapshot does not prove joined reader and released resources')
            structured[name] = value
    require(package_finished and runs == expected and set(outcomes) == expected and
            set(structured) == set(leaves), 'boundary fixed matrix missing run, terminal or evidence')
    return {'mode': mode, 'verified': True, 'process_exit': exit_code, 'package': SERVICE,
            'required_top_level_tests': list(tops), 'observed_top_level_tests': list(tops),
            'observed_tests': len(expected), 'top_level_count': 1, 'subtest_count': len(leaves),
            'leaf_count': len(leaves), 'skip_count': 0,
            'leaf_outcomes': {name: outcomes[name] for name in sorted(leaves)},
            'target_counts': {'run': len(runs), 'pass': len(outcomes), 'fail': 0, 'skip': 0},
            'same_test_evidence': {name: structured[name] for name in sorted(leaves)},
            'count_semantics': 'exact outer lifecycle/live leaves; child expected FAIL/SKIP are structured data only'}


def verify_ws_ttft(events, exit_code):
    require(type(exit_code) is int and exit_code == 0 and bool(events),
            'WS TTFT requires successful direct process exit and JSON events')
    require(all(type(event) is dict for event in events), 'WS TTFT event must be an object')
    require(not any(event.get('Action') in ('fail', 'skip', 'build-fail') for event in events),
            'WS TTFT cannot contain skip, failure or build failure')
    runs, outcomes = set(), {}
    started, finished = False, False
    for event in events:
        name, action = event.get('Test'), event.get('Action')
        require(event.get('Package') == SERVICE and not finished,
                'WS TTFT wrong package or event after package terminal')
        require(type(event.get('Output', '')) is str, 'WS TTFT Output must be text')
        if name is None:
            if action == 'start':
                require(not started and not runs, 'WS TTFT duplicate or late package start')
                started = True
            elif action == 'pass':
                require(started and runs == WS_TTFT_NODES and set(outcomes) == WS_TTFT_NODES,
                        'WS TTFT package passed before the complete exact matrix')
                finished = True
            else:
                require(started and action == 'output', 'WS TTFT missing package start or invalid package event')
            continue
        require(started and isinstance(name, str) and name in WS_TTFT_NODES,
                'WS TTFT unexpected test/leaf or missing package start')
        if action == 'run':
            require(name not in runs, 'WS TTFT duplicate run')
            if '/' in name:
                parent = name.rsplit('/', 1)[0]
                require(parent in runs and parent not in outcomes, 'WS TTFT leaf run outside its parent')
            runs.add(name)
        elif action == 'pass':
            require(name in runs and name not in outcomes, 'WS TTFT terminal without prior run or duplicate terminal')
            children = {node for node in WS_TTFT_NODES if node.startswith(name + '/')}
            require(children <= set(outcomes), 'WS TTFT parent passed before required children')
            outcomes[name] = action
        else:
            require(name in runs and action in ('output', 'pause', 'cont'),
                    'WS TTFT unsupported action or output before run')
    require(started and finished and runs == WS_TTFT_NODES and set(outcomes) == WS_TTFT_NODES,
            'WS TTFT missing exact run, terminal or package boundary')
    actual_leaves = {name for name in runs if not any(other.startswith(name + '/') for other in runs)}
    require(actual_leaves == set(WS_TTFT_LEAVES), 'WS TTFT fixed seven-leaf matrix mismatch')
    return {'mode': 'ws-ttft', 'verified': True, 'process_exit': exit_code, 'package': SERVICE,
            'required_top_level_tests': list(WS_TTFT_TESTS), 'observed_top_level_tests': list(WS_TTFT_TESTS),
            'observed_node_tests': sorted(runs), 'observed_tests': len(runs), 'top_level_count': 3,
            'subtest_count': 6, 'leaf_count': len(actual_leaves), 'skip_count': 0,
            'leaf_outcomes': {name: outcomes[name] for name in sorted(actual_leaves)},
            'target_counts': {'run': len(runs), 'pass': len(outcomes), 'fail': 0, 'skip': 0},
            'count_semantics': 'three tops union seven leaves is nine nodes; Origin top is also a leaf; no trace or mutants required'}


def verify(events, exit_code, mode):
    if mode == 'ws-ttft':
        return verify_ws_ttft(events, exit_code)
    if mode == 'cache-race':
        return verify_cache(events, exit_code)
    if mode in ('sse-lifecycle', 'live-boundary'):
        return verify_boundary(events, exit_code, mode)
    require(mode in ('red', 'green', 'rollup-race', 'sse', 'retry', 'billing-race', 'session-memory'), 'invalid gate mode')
    expected = {'red': RED_TESTS, 'green': ROLLUP_TESTS + ROLLUP_ADDITIONAL_TESTS,
                'rollup-race': ROLLUP_TESTS + ROLLUP_ADDITIONAL_TESTS,
                'sse': SSE_TESTS, 'retry': RETRY_TESTS, 'billing-race': BILLING_RACE_TESTS,
                'session-memory': cache_manifest.SESSION_MEMORY_TESTS}[mode]
    package = SERVICE if mode in ('sse', 'retry', 'session-memory') else REPOSITORY
    require(bool(events), 'no go-test JSON events')
    require(all(isinstance(event, dict) for event in events), 'non-object JSON event')
    require(not any(event.get('Action') == 'build-fail' for event in events), 'build failure is not RED')
    all_fail = [event for event in events if event.get('Action') == 'fail']
    runs, outcomes, output, package_outcomes = {}, {}, {}, []
    for event in events:
        if event.get('Package') != package:
            continue
        test, action = event.get('Test'), event.get('Action')
        if not test:
            if action in ('pass', 'fail', 'skip'):
                package_outcomes.append(action)
            continue
        top = test.split('/', 1)[0]
        if top not in expected:
            if mode in ('red', 'green', 'rollup-race', 'billing-race') and action == 'run':
                raise ValueError('unexpected dedicated integration test selected')
            continue
        if action == 'run':
            require(test not in runs, 'duplicate test run event')
            runs[test] = True
        if action in ('pass', 'fail', 'skip'):
            require(test not in outcomes, 'duplicate test outcome event')
            outcomes[test] = action
        if action == 'output':
            output.setdefault(top, []).append(event.get('Output', ''))
    require(all(test in runs for test in expected), 'missing required test run')
    require(set(runs) == set(outcomes), 'unfinished/mismatched test events')
    require(not any(result == 'skip' for result in outcomes.values()), 'required test skipped')
    if mode == 'red':
        require(exit_code != 0 and package_outcomes == ['fail'], 'RED process/package must fail')
        require(all(outcomes.get(test) == 'fail' for test in expected), 'both RED targets must fail')
        require(all(event.get('Package') == package and
                    (not event.get('Test') or event['Test'] in expected) for event in all_fail),
                'unrelated failure is not the RED counterexample')
        for test in expected:
            text = ''.join(output.get(test, []))
            require('NEGATIVE CONTROL: isolated schema uses official 251 invalidation without 252' in text,
                    'negative control was not observed in target test')
            require('d78 summary must match live synthetic detail' in text and 'Not equal:' in text,
                    'failure did not reach the unchanged safety consistency assertion')
    else:
        require(exit_code == 0 and package_outcomes == ['pass'] and not all_fail,
                'GREEN process/package must pass with no failures')
        require(all(result == 'pass' for result in outcomes.values()), 'required tests not all PASS')
        require(not any('NEGATIVE CONTROL:' in ''.join(parts) for parts in output.values()),
                'negative flag leaked into normal GREEN')
    leaves = [test for test in runs if not any(other.startswith(test + '/') for other in runs)]
    if mode == 'sse':
        require(len(SSE_LEAVES) == 32 and set(leaves) == set(SSE_LEAVES),
                'SSE gate requires all 32 exact contract leaves across eight top-level tests')
    if mode == 'retry':
        require(set(leaves) == set(RETRY_LEAVES), 'retry gate requires all eight exact contract leaves')
    if mode == 'session-memory':
        require(set(runs) == set(cache_manifest.SESSION_MEMORY_TESTS) and set(leaves) == set(runs),
                'SessionMemory requires all six exact top-level tests with no unexpected subtests')
    if mode == 'billing-race':
        require(set(leaves) == set(BILLING_RACE_LEAVES) and
                set(runs) == set(BILLING_RACE_TESTS + BILLING_RACE_LEAVES),
                'billing race requires the exact top-level and repository/lane leaves')
    return {'mode': mode, 'verified': True, 'process_exit': exit_code,
            'package': package, 'required_top_level_tests': list(expected),
            'observed_tests': len(runs), 'top_level_count': sum('/' not in name for name in runs),
            'subtest_count': sum('/' in name for name in runs), 'leaf_count': len(leaves), 'skip_count': 0,
            'observed_top_level_tests': sorted(name for name in runs if '/' not in name),
            'leaf_outcomes': {name: outcomes[name] for name in sorted(leaves)},
            'target_counts': {'run': len(runs), 'pass': sum(v == 'pass' for v in outcomes.values()),
                              'fail': sum(v == 'fail' for v in outcomes.values()), 'skip': 0}}


def validate_execution(execution, context, exit_code, events_sha256, mode):
    require(execution['source_commit'] == context['source_commit'] and execution['github_sha'] == context['github_sha']
            and execution['runner_version'] == context['runner_version'] and execution['inputs'] == context['inputs']
            and execution['exit_code'] == exit_code and execution.get('command_completed') is True
            and execution['events_sha256'] == events_sha256
            and execution['command'][:2] == ['go', 'test'] and '-race' in execution['command']
            and '-json' in execution['command'], 'execution receipt mismatch')
    if mode == 'cache-race':
        require(execution['command'] == cache_manifest.CACHE_COMMAND and execution.get('working_directory') == 'backend',
                'cache race must use the exact two-package unit command')
    if mode == 'billing-race':
        expected_command = ['go', 'test', '-json', '-mod=readonly', '-p=2', '-race', '-tags=integration',
                            './internal/repository', '-run', BILLING_RACE_SELECTOR, '-count=1', '-timeout=5m']
        require(execution['command'] == expected_command and execution.get('working_directory') == 'backend',
                'billing race must use the exact isolated integration command')
    if mode in ('sse-lifecycle', 'live-boundary', 'ws-ttft'):
        commands = {'live-boundary': [LIVE_BOUNDARY_COMMAND], 'ws-ttft': [WS_TTFT_COMMAND],
                    'sse-lifecycle': [SSE_LIFECYCLE_COMMAND, PUBLISH_LIFECYCLE_COMMAND]}[mode]
        sources = WS_TTFT_SOURCE_FILES if mode == 'ws-ttft' else BOUNDARY_SOURCE_FILES
        require(execution['command'] in commands and execution.get('working_directory') == 'backend',
                'boundary gate requires the exact reviewed selector, packages, tags, count and timeout')
        require(type(exit_code) is int and type(execution['exit_code']) is int and
                context['source_commit'] == context['github_sha'] and
                re.fullmatch(r'[0-9a-f]{40}', context['source_commit']) is not None and
                execution.get('isolated_live_environment') is True and
                all(path in context['inputs'] and isinstance(context['inputs'][path], str) and
                    re.fullmatch(r'[0-9a-f]{64}', context['inputs'][path]) is not None
                    for path in sources),
                'boundary execution requires final same-SHA inputs and sanitized live environment')


def validate_boundary_stderr(execution, raw, mode):
    require(execution.get('stderr_sha256') == evidence.sha(raw), 'boundary stderr hash mismatch')
    text = raw.decode('utf-8', errors='strict')
    if mode == 'live-boundary':
        require(not any(value in text for value in LIVE_FORBIDDEN_OUTPUT) and
                'live_boundary_evidence ' not in text, 'forbidden or misplaced live boundary stderr evidence')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--mode', choices=('red', 'green', 'rollup-race', 'sse', 'retry', 'billing-race', 'cache-race', 'session-memory', 'sse-lifecycle', 'live-boundary', 'ws-ttft'), required=True)
    parser.add_argument('--events', required=True)
    parser.add_argument('--stderr')
    parser.add_argument('--exit-code', type=int, required=True)
    parser.add_argument('--receipt', required=True)
    parser.add_argument('--execution', required=True)
    args = parser.parse_args()
    receipt = {'mode': args.mode, 'verified': False, 'process_exit': args.exit_code}
    try:
        root = Path(__file__).resolve().parents[2]
        receipt.update(evidence.context(root, evidence.race_inputs(root)))
        if args.mode in ('cache-race', 'session-memory'):
            cache_manifest.check_sources(root)
        execution_raw = Path(args.execution).read_bytes()
        boundary = args.mode in ('sse-lifecycle', 'live-boundary', 'ws-ttft')
        execution = strict_json(execution_raw) if boundary else json.loads(execution_raw)
        events_raw = Path(args.events).read_bytes()
        validate_execution(execution, receipt, args.exit_code, evidence.sha(events_raw), args.mode)
        if boundary:
            require(args.stderr is not None, 'boundary gate requires captured raw stderr')
            validate_boundary_stderr(execution, Path(args.stderr).read_bytes(), args.mode)
        parse = strict_json if boundary else json.loads
        events = [parse(line) for line in events_raw.decode('utf-8').splitlines() if line.strip()]
        receipt.update(verify(events, args.exit_code, args.mode))
        receipt['execution'] = execution
        receipt['execution_sha256'] = evidence.sha(execution_raw)
    except (ValueError, OSError, TypeError, KeyError) as error:
        receipt['reason'] = str(error)
        Path(args.receipt).write_text(json.dumps(receipt, indent=2) + '\n', encoding='utf-8')
        parser.exit(1, 'go-test evidence gate rejected: ' + str(error) + '\n')
    Path(args.receipt).write_text(json.dumps(receipt, indent=2) + '\n', encoding='utf-8')
    top_count = receipt['top_level_count']
    print(f"Verified {args.mode}: {top_count} top-level tests, {receipt['leaf_count']} leaves, no skips")


if __name__ == '__main__':
    main()
