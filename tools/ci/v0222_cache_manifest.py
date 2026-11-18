"""#39 人工核过源码矩阵的固定测试合同；不从当前运行结果学习 expected。"""
import hashlib
from pathlib import Path
import re

SERVICE = 'github.com/Wei-Shaw/sub2api/internal/service'
APICOMPAT = 'github.com/Wei-Shaw/sub2api/internal/pkg/apicompat'
CACHE_SELECTOR = '^Test(V10Cache|V8OpenAICache)'
CACHE_COMMAND = ['go', 'test', '-json', '-mod=readonly', '-p=2', '-race', '-tags=unit',
                 './internal/service', './internal/pkg/apicompat', '-run', CACHE_SELECTOR,
                 '-count=1', '-timeout=180s']
BOOL = ('false', 'true')
ROUTES = ('messages_json', 'messages_sse', 'chat_json', 'chat_sse')
# 每项 tuple 是源码明确定义的叶后缀；空 tuple 表示顶层本身是唯一叶。
SERVICE_CASES = {
    'TestV8OpenAICacheUsageRealPath': tuple(f'{case}/oauth={oauth}/stream={stream}'
        for case in ('ordinary_uncached_is_not_write', 'canonical_mixed_ttl',
                     'single_variable_remove_ttl_details', 'minimal_vendor_read_only',
                     'minimal_read_nested_control', 'nested_zero_beats_read_alias',
                     'canonical_zero_beats_vendor_alias', 'chat_usage_shape',
                     'vendor_creation_alias', 'vendor_read_alias')
        for oauth in BOOL for stream in BOOL),
    'TestV8OpenAICachePrefixRealPath': tuple(f'{case}/oauth={oauth}'
        for case in ('metadata_stable_stateless_control', 'metadata_stable_tool_append',
                     'metadata_cold_local_state_long_history', 'single_variable_cold_short_history',
                     'single_variable_warm_continuation_long_history', 'single_variable_cold_disabled_continuation',
                     'no_metadata_rolling_anchor', 'single_variable_no_metadata_no_rolling_anchor',
                     'single_variable_stable_metadata_rolling_anchor', 'explicit_session_rolling_anchor') for oauth in BOOL),
    'TestV8OpenAICacheIsolationRealPath': (),
    'TestV10CacheAContextErrorRealPath': ('structured_context', 'ordinary_context_window', 'explicit_temp_rule_context'),
    'TestV10CacheARetryFullReplayRealPath': ('not_found', 'unsupported'),
    'TestV10CacheBCanonicalParity': ('canonical_zero', 'fieldwise_fallback_read', 'fieldwise_input_read_wins',
        'negative_canonical', 'ttl_valid', 'ttl_partial', 'ttl_fraction', 'ttl_string', 'ttl_bad_sum',
        'ttl_invalid_input_no_prompt_fallback', 'ttl_absent_input_prompt_fallback'),
    'TestV10CacheBActualJSONLibrarySemantics': ('duplicate_read', 'duplicate_ttl', 'fraction_read', 'string_read'),
    'TestV10CacheBNonCacheCandidateCallerDifference': tuple(f'{route}/zero={zero}' for route in ROUTES for zero in BOOL),
    'TestV10CacheC3RealBranchIsolation': ('same_request_retry', 'different_first_user_same_key',
        'different_system_same_history', 'different_tools_same_history', 'fork_wrong_assistant_extends_prior_request',
        'valid_linear_generated_echo', 'changed_tools_valid_echo_still_wrong_context'),
    'TestV10CacheC3InterleavedBranches': (),
    'TestV10CacheC3ToolBranchGeneratedEcho': ('same_args=true', 'same_args=false'),
    'TestV10CacheC3ConcurrentCompletionOverwrite': (),
    'TestV10CacheC1AnchorGateFacts': ('assistant_only', 'user_only', 'system_only', 'none'),
    'TestV10CacheProofMultiTurnActualEcho': BOOL,
    'TestV10CacheProofDeltaOnlyAndSplitText': BOOL,
    'TestV10CacheProofParallelToolSuffixAndCanonicalArgs': BOOL,
    'TestV10CacheProofBufferedWriteFailureDoesNotBind': ('error', 'short', 'canceled'),
    'TestV10CacheProofUnknownAndNonNormalTerminalNoBinding': tuple(f'{case}/{stream}'
        for case in ('incomplete_other', 'unknown_block') for stream in BOOL),
    'TestV10CacheProofBindingSnapshotCASAndBudget': (),
    'TestV10CacheStableAnchorRequiresRealMaterial': (),
    'TestV10CacheProofCASRetryUsesFullReplay': (),
    'TestV10CacheProofInvalidResponseRecoversNextValid': tuple(f'{case}/{stream}'
        for case in ('duplicate_args', 'server_block', 'oversize', 'max_tokens') for stream in BOOL),
    'TestV10CacheProofThinkingSignatureUsesSameConverter': ('synthetic-provider-signature',
        'gAAAA-synthetic-foreign', 'ccswitch-openai-reasoning-v1:synthetic'),
    'TestV10CacheProofStreamObserverSequenceAndLimits': ('unknown_delta', 'missing_stop', 'duplicate_args', 'limit'),
    'TestV10CacheEligibilityUnknownDeltaFinishAndRecovery': tuple(f'{case}/{stream}'
        for case in ('unknown_delta', 'malformed_ignored', 'unknown_finish', 'known_lifecycle') for stream in BOOL),
    'TestV10CacheEligibilityReasoningAndTextRemainUsable': BOOL,
    'TestV10CacheFourHandlersSameBuckets': tuple(f'{case}/{route}'
        for case in ('aliases', 'fieldwise', 'negative', 'ttl') for route in ROUTES),
    'TestV10CacheIdentityRollingThreeTurnsAndIsolation': (),
    'TestV10CacheDigestOnlyStripsKnownBlockControls': (),
    'TestV10CacheAnchorOnlyStableMaterial': (),
    'TestV10CacheProofUnknownProviderOutputCannotProve': BOOL,
    'TestV10CacheProofClientWriteBytesUnchanged': BOOL,
    'TestV10CacheGrokIdentityLegacyUnchanged': (),
    'TestV10CacheProofInterruptedStreamDoesNotBind': (),
}
APICOMPAT_CASES = {
    'TestV10CacheUsageCanonicalFieldsOnly': ('alias', 'fieldwise', 'zero', 'null', 'negative',
        'alias_priority', 'ttl', 'ttl_partial', 'duplicate_unsupported_cache_first'),
}
CACHE_CASES = {SERVICE: SERVICE_CASES, APICOMPAT: APICOMPAT_CASES}
SESSION_MEMORY_TESTS = tuple('TestSessionMemoryCache' + suffix for suffix in (
    'ExpiresWithoutReads', 'BoundsAndLRU', 'LongestPrefixKeepsRawKeySemantics',
    'ExpiredLongestPrefixFallsBack', 'DigestIndexMatchesLegacyLookup', 'ConcurrentClose'))

# 固定测试字节锁用于发现同名新case/漏case；改变时必须复核上述手工矩阵，不能自动接受。
CACHE_TEST_SOURCE_SHA256 = {
    'backend/internal/pkg/apicompat/cache_usage_test.go': '4b53948f99d7c54889f9a9316cac0725272ebf6b34b26bf2fa8cb3cf283a0403',
    'backend/internal/service/openai_gateway_messages_cache_contract_test.go': '7c440fd3d88dd11759d6d17525bdc48e446b9e241561beca35729ef3bbb84fc5',
    'backend/internal/service/openai_messages_continuation_contract_test.go': '592afab351ffe5b30cac2bddaf5027623c6139a46b6e1b96e07fd390cec6175f',
    'backend/internal/service/openai_messages_continuation_proof_test.go': '685fa1bb23d13fa78c95d94854b9678d30aa27786c256491fd9b97d1b23d3513',
    'backend/internal/service/openai_messages_continuation_recovery_test.go': '2b4abc2d7fcd9f078bcefe326aacfe8275c8bb2f70975e491a770e52a3609d24',
    'backend/internal/service/openai_messages_continuation_eligibility_test.go': 'c5c5c0c55aa86b34b130bbc9da0292643fd716cc437316896721ed57469dfd41',
    'backend/internal/service/openai_messages_cache_handlers_test.go': 'edff44f977b01a43abd97f927733f0f324dfe53b57a265592cdfc1e42152e328',
    'backend/internal/service/openai_messages_cache_identity_test.go': 'c5b6be334426be4526ade2064d3aceb6c040982703d7cff79b2d2d31c027ebda',
    'backend/internal/service/openai_messages_cache_final_contract_test.go': '3ae94c5fe0cbb565c9dd3a7ef68ad2acf5ac6c93ef3815b3f8d7bbdd5ae76c33',
    'backend/internal/service/openai_messages_continuation_test_helpers_test.go': '60f80e947a6c48b43178782ed61b52e4bec85e9aa523040784b30ec876f92740',
    'backend/internal/service/session_memory_cache_test.go': 'dd94788c0f3e9d519cfdd3178510132412f344efd578bcc78962c977ac4c6c50',
}
CACHE_PRODUCT_FILES = (
    'backend/internal/pkg/apicompat/cache_usage.go', 'backend/internal/pkg/apicompat/types.go',
    'backend/internal/pkg/apicompat/anthropic_to_responses.go',
    'backend/internal/service/openai_gateway_messages.go', 'backend/internal/service/openai_gateway_response_handling.go',
    'backend/internal/service/openai_messages_continuation.go', 'backend/internal/service/openai_messages_continuation_proof.go',
    'backend/internal/service/openai_messages_digest_session.go', 'backend/internal/service/openai_compat_prompt_cache_key.go',
    'backend/internal/service/session_memory_cache.go',
)
CACHE_EXISTING_TEST_FILES = ('backend/internal/service/openai_compat_model_test.go',
                             'backend/internal/service/openai_setup_token_compat_test.go')
CACHE_DELETED_FILES = ('backend/internal/service/openai_messages_replay_guard.go',
                       'backend/internal/service/openai_messages_replay_guard_test.go')


def leaves(package):
    return tuple(top + '/' + suffix if suffix else top for top, suffixes in CACHE_CASES[package].items()
                 for suffix in (suffixes or ('',)))


def nodes(package):
    # 多级slash是一次t.Run产生的完整名字，不虚构中间run节点。
    return set(CACHE_CASES[package]) | set(leaves(package))


def source_paths(root):
    return [root / name for name in (*CACHE_TEST_SOURCE_SHA256, *CACHE_PRODUCT_FILES, *CACHE_EXISTING_TEST_FILES)]


def check_sources(root):
    for name, expected in CACHE_TEST_SOURCE_SHA256.items():
        path = root / name
        if not path.is_file() or path.is_symlink() or hashlib.sha256(path.read_bytes()).hexdigest() != expected:
            raise ValueError('cache test source changed/missing; review fixed manifest: ' + name)
    for name in CACHE_DELETED_FILES:
        if (root / name).exists():
            raise ValueError('deleted cache source reappeared; review contract: ' + name)
    for package, directory in ((SERVICE, 'backend/internal/service'), (APICOMPAT, 'backend/internal/pkg/apicompat')):
        declared = set()
        for path in (root / directory).glob('*test.go'):
            declared.update(re.findall(r'^func (Test(?:V10Cache|V8OpenAICache)\w+)\(', path.read_text(encoding='utf-8'), re.M))
        if declared != set(CACHE_CASES[package]):
            raise ValueError('cache top-level source manifest drift; explicit review required')
    text = (root / 'backend/internal/service/session_memory_cache_test.go').read_text(encoding='utf-8')
    if set(re.findall(r'^func (TestSessionMemory\w+)\(', text, re.M)) != set(SESSION_MEMORY_TESTS):
        raise ValueError('SessionMemory source manifest drift')
