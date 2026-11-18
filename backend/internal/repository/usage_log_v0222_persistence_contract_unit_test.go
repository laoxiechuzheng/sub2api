//go:build unit

package repository

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 独立固定的持久化协议：不得从prepare或产品列清单反推预期。
const v0222UsageInsertColumnContract = `user_id, api_key_id, account_id, request_id, model, requested_model, upstream_model, upstream_response_model, upstream_model_mismatch, group_id, subscription_id, input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens, cache_creation_5m_tokens, cache_creation_1h_tokens, image_output_tokens, image_output_cost, image_input_tokens, image_input_cost, input_cost, output_cost, cache_creation_cost, cache_read_cost, total_cost, actual_cost, rate_multiplier, account_rate_multiplier, billing_type, request_type, stream, openai_ws_mode, duration_ms, first_token_ms, user_agent, ip_address, image_count, image_size, image_input_size, image_output_size, image_size_source, image_size_breakdown, video_count, video_resolution, video_duration_seconds, service_tier, reasoning_effort, requested_reasoning_effort, inbound_endpoint, upstream_endpoint, cache_ttl_overridden, long_context_billing_applied, channel_id, model_mapping_chain, billing_tier, billing_mode, account_stats_cost, upstream_request_id, session_id, native_compaction_v2, composite_route_id, composite_route_kind, composite_fallback_chain, cache_creation_30m_tokens, cache_creation_12h_tokens, created_at`

func v0222UsagePointer[T any](value T) *T { return &value }

func v0222UsageFixture() (*service.UsageLog, []driver.Value) {
	created := time.Date(2026, 10, 1, 12, 34, 56, 123456000, time.UTC)
	log := &service.UsageLog{
		UserID: 101, APIKeyID: 102, AccountID: 103, RequestID: "v0222-client",
		Model: "stored-model", RequestedModel: "public-model", UpstreamModel: v0222UsagePointer("wire-model"),
		UpstreamResponseModel: v0222UsagePointer("observed-model"), UpstreamModelMismatch: v0222UsagePointer(true),
		GroupID: v0222UsagePointer(int64(110)), SubscriptionID: v0222UsagePointer(int64(111)),
		InputTokens: 112, OutputTokens: 113, CacheCreationTokens: 198,
		CacheReadTokens: 115, CacheCreation5mTokens: 17, CacheCreation1hTokens: 19,
		ImageOutputTokens: 118, ImageOutputCost: 1.19, ImageInputTokens: 120, ImageInputCost: 1.21,
		InputCost: 1.22, OutputCost: 1.23, CacheCreationCost: 1.24, CacheReadCost: 1.25,
		TotalCost: 12.5, ActualCost: 3.25, RateMultiplier: 1.28, AccountRateMultiplier: v0222UsagePointer(1.5),
		BillingType: service.BillingTypeSubscription, RequestType: service.RequestTypeWSV2, Stream: true, OpenAIWSMode: true,
		DurationMs: v0222UsagePointer(134), FirstTokenMs: v0222UsagePointer(135),
		UserAgent: v0222UsagePointer("synthetic-client"), IPAddress: v0222UsagePointer("192.0.2.1"), ImageCount: 138,
		ImageSize: v0222UsagePointer("size"), ImageInputSize: v0222UsagePointer("input-size"), ImageOutputSize: v0222UsagePointer("output-size"),
		ImageSizeSource: v0222UsagePointer("output"), ImageSizeBreakdown: map[string]int{"4K": 2},
		VideoCount: 144, VideoResolution: v0222UsagePointer("resolution"), VideoDurationSeconds: v0222UsagePointer(146),
		ServiceTier: v0222UsagePointer("priority"), ReasoningEffort: v0222UsagePointer("xhigh"), RequestedReasoningEffort: v0222UsagePointer("max"),
		InboundEndpoint: v0222UsagePointer("/v1/messages"), UpstreamEndpoint: v0222UsagePointer("/v1/responses"),
		CacheTTLOverridden: true, LongContextBillingApplied: true, ChannelID: v0222UsagePointer(int64(154)),
		ModelMappingChain: v0222UsagePointer("public→wire"), BillingTier: v0222UsagePointer("tier"), BillingMode: v0222UsagePointer("token"),
		AccountStatsCost: v0222UsagePointer(7.75), UpstreamRequestID: v0222UsagePointer("upstream-id"), SessionID: v0222UsagePointer("session-id"),
		NativeCompactionV2: true, CompositeRouteID: v0222UsagePointer(int64(162)),
		CompositeRouteKind:     v0222UsagePointer(service.CompositeRouteRequestKindCompaction),
		CompositeFallbackChain: v0222UsagePointer("primary (429) → backup"), CacheCreation30mTokens: 73, CacheCreation12hTokens: 89,
		CreatedAt: created,
	}
	// 全67值为独立字面量；只共享不可变created时间，不调用prepare生成预期。
	expected := []driver.Value{
		int64(101), int64(102), int64(103), "v0222-client", "stored-model", "public-model", "wire-model", "observed-model", true,
		int64(110), int64(111), int64(112), int64(113), int64(198), int64(115), int64(17), int64(19),
		int64(118), 1.19, int64(120), 1.21, 1.22, 1.23, 1.24, 1.25, 12.5, 3.25, 1.28, 1.5,
		int64(1), int64(3), true, true, int64(134), int64(135), "synthetic-client", "192.0.2.1", int64(138),
		"size", "input-size", "output-size", "output", `{"4K":2}`, int64(144), "resolution", int64(146),
		"priority", "xhigh", "max", "/v1/messages", "/v1/responses", true, true, int64(154), "public→wire", "tier", "token", 7.75,
		"upstream-id", "session-id", true, int64(162), "compaction", "primary (429) → backup", int64(73), int64(89), created,
	}
	return log, expected
}

func v0222UsageDriverValues(t *testing.T, args []any) []driver.Value {
	t.Helper()
	result := make([]driver.Value, len(args))
	for i, arg := range args {
		converted, err := driver.DefaultParameterConverter.ConvertValue(arg)
		require.NoError(t, err, "parameter %d", i+1)
		result[i] = converted
	}
	return result
}

func v0222UsageColumns(value string) []string {
	values := strings.Split(value, ",")
	for i := range values {
		values[i] = strings.TrimSpace(values[i])
	}
	return values
}

func v0222RequireUsageInsertSQL(t *testing.T, query string, batch, indexed bool) {
	t.Helper()
	expected := v0222UsageColumns(v0222UsageInsertColumnContract)
	require.Len(t, expected, 67)
	insert := regexp.MustCompile(`(?s)INSERT INTO usage_logs\s*\((.*?)\)`).FindStringSubmatch(query)
	require.Len(t, insert, 2)
	require.Equal(t, expected, v0222UsageColumns(insert[1]))
	if batch {
		input := regexp.MustCompile(`(?s)WITH input\s*\((.*?)\) AS`).FindStringSubmatch(query)
		require.Len(t, input, 2)
		wanted := append([]string{}, expected...)
		if indexed {
			wanted = append([]string{"input_idx"}, wanted...)
		}
		require.Equal(t, wanted, v0222UsageColumns(input[1]))
		selected := regexp.MustCompile(`(?s)INSERT INTO usage_logs\s*\(.*?\)\s*SELECT\s*(.*?)\s*FROM input`).FindStringSubmatch(query)
		require.Len(t, selected, 2)
		require.Equal(t, expected, v0222UsageColumns(selected[1]))
	} else {
		values := regexp.MustCompile(`(?s)INSERT INTO usage_logs\s*\(.*?\) VALUES\s*\((.*?)\)`).FindStringSubmatch(query)
		require.Len(t, values, 2)
		placeholders := v0222UsageColumns(values[1])
		require.Len(t, placeholders, 67)
		for i, placeholder := range placeholders {
			require.Equal(t, fmt.Sprintf("$%d", i+1), placeholder)
		}
	}
}

func TestUsageLogV0222PersistenceIndependent67ParameterAnd68ColumnContract(t *testing.T) {
	log, expected := v0222UsageFixture()
	require.Len(t, expected, 67)
	require.Equal(t, expected, v0222UsageDriverValues(t, prepareUsageLogInsert(log).args))
	require.Equal(t, append([]string{"id"}, v0222UsageColumns(v0222UsageInsertColumnContract)...), v0222UsageColumns(usageLogSelectColumns))
	require.Equal(t, []string{
		"bigint", "bigint", "bigint", "text", "text", "text", "text", "text", "boolean", "bigint", "bigint",
		"integer", "integer", "integer", "integer", "integer", "integer", "integer", "numeric", "integer", "numeric",
		"numeric", "numeric", "numeric", "numeric", "numeric", "numeric", "numeric", "numeric", "smallint", "smallint", "boolean", "boolean",
		"integer", "integer", "text", "text", "integer", "text", "text", "text", "text", "jsonb", "integer", "text", "integer",
		"text", "text", "text", "text", "text", "boolean", "boolean", "bigint", "text", "text", "text", "numeric", "text", "text",
		"boolean", "bigint", "text", "text", "integer", "integer", "timestamptz",
	}, usageLogInsertArgTypes[:])
}

func TestUsageLogV0222PersistenceMultirowArgumentsKeepFullStride(t *testing.T) {
	first, firstExpected := v0222UsageFixture()
	second, secondExpected := v0222UsageFixture()
	second.RequestID = "v0222-second"
	second.CacheCreation30mTokens, second.CacheCreation12hTokens = 97, 101
	second.TotalCost, second.ActualCost, second.AccountStatsCost = 22.5, 4.5, v0222UsagePointer(8.75)
	secondExpected[3], secondExpected[64], secondExpected[65] = "v0222-second", int64(97), int64(101)
	secondExpected[25], secondExpected[26], secondExpected[57] = 22.5, 4.5, 8.75
	firstPrepared, secondPrepared := prepareUsageLogInsert(first), prepareUsageLogInsert(second)
	firstKey, secondKey := usageLogBatchKey(first.RequestID, first.APIKeyID), usageLogBatchKey(second.RequestID, second.APIKeyID)
	query, args := buildUsageLogBatchInsertQuery([]string{firstKey, secondKey}, map[string]usageLogInsertPrepared{firstKey: firstPrepared, secondKey: secondPrepared})
	expected := append([]driver.Value{int64(0)}, firstExpected...)
	expected = append(expected, int64(1))
	expected = append(expected, secondExpected...)
	require.Len(t, args, 136)
	require.Equal(t, expected, v0222UsageDriverValues(t, args))
	v0222RequireUsageInsertSQL(t, query, true, true)
	placeholders := usageLogPlaceholderRe.FindAllString(query, -1)
	require.Len(t, placeholders, 136)
	for i, placeholder := range placeholders {
		require.True(t, placeholder == fmt.Sprintf("$%d", i+1), "placeholder %d=%s", i+1, placeholder)
	}
	query, args = buildUsageLogBestEffortInsertQuery([]usageLogInsertPrepared{firstPrepared, secondPrepared})
	expected = append(append([]driver.Value{}, firstExpected...), secondExpected...)
	require.Len(t, args, 134)
	require.Equal(t, expected, v0222UsageDriverValues(t, args))
	v0222RequireUsageInsertSQL(t, query, true, false)
	placeholders = usageLogPlaceholderRe.FindAllString(query, -1)
	require.Len(t, placeholders, 134)
	for i, placeholder := range placeholders {
		require.Equal(t, fmt.Sprintf("$%d", i+1), placeholder)
	}
}

func TestUsageLogV0222PersistenceRealInsertEntrypointsAndFallback(t *testing.T) {
	for _, path := range []string{"single", "no_result", "batch", "best_effort", "batch_sql_fallback", "best_effort_sql_fallback", "batch_invalid_result_no_replay"} {
		t.Run(path, func(t *testing.T) {
			log, expected := v0222UsageFixture()
			var captured []string
			db, mock := newSQLCapturingMock(t, &captured)
			repo := &usageLogRepository{sql: db}
			ctx := context.Background()
			batch := path != "single" && path != "no_result"
			bestEffort := strings.HasPrefix(path, "best_effort")
			fallback := strings.HasSuffix(path, "sql_fallback")
			sentinel := errors.New("synthetic batch statement failure")
			if batch {
				if bestEffort {
					expectation := mock.ExpectExec("WITH input").WithArgs(expected...)
					if fallback {
						expectation.WillReturnError(sentinel)
					} else {
						expectation.WillReturnResult(sqlmock.NewResult(0, 1))
					}
				} else {
					args := append([]driver.Value{int64(0)}, expected...)
					expectation := mock.ExpectQuery("WITH input").WithArgs(args...)
					switch {
					case fallback:
						expectation.WillReturnError(sentinel)
					case path == "batch_invalid_result_no_replay":
						expectation.WillReturnRows(sqlmock.NewRows([]string{"payload"}).AddRow([]byte(`not-json`)))
					default:
						expectation.WillReturnRows(sqlmock.NewRows([]string{"payload"}).AddRow([]byte(`[{"request_id":"v0222-client","api_key_id":102,"id":700,"created_at":"2026-10-01T12:34:56.123456Z","inserted":true}]`)))
					}
				}
			}
			if !batch || fallback {
				if bestEffort || path == "no_result" {
					mock.ExpectExec("INSERT INTO usage_logs").WithArgs(expected...).WillReturnResult(sqlmock.NewResult(0, 1))
				} else {
					mock.ExpectQuery("INSERT INTO usage_logs").WithArgs(expected...).WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(700), log.CreatedAt))
				}
			}
			switch {
			case path == "single":
				inserted, err := repo.Create(ctx, log)
				require.NoError(t, err)
				require.True(t, inserted)
				require.EqualValues(t, 700, log.ID)
			case path == "no_result":
				require.NoError(t, execUsageLogInsertNoResult(ctx, db, prepareUsageLogInsert(log)))
			case bestEffort:
				done := make(chan error, 1)
				repo.flushBestEffortBatch(db, []usageLogBestEffortRequest{{prepared: prepareUsageLogInsert(log), apiKeyID: log.APIKeyID, resultCh: done}})
				require.NoError(t, <-done)
			default:
				done := make(chan usageLogCreateResult, 1)
				repo.flushCreateBatch(db, []usageLogCreateRequest{{log: log, prepared: prepareUsageLogInsert(log), resultCh: done}})
				result := <-done
				if path == "batch_invalid_result_no_replay" {
					require.Error(t, result.err)
					require.False(t, result.inserted)
				} else {
					require.NoError(t, result.err)
					require.True(t, result.inserted)
					require.EqualValues(t, 700, log.ID)
				}
			}
			require.NoError(t, mock.ExpectationsWereMet())
			wantedQueries := 1
			if fallback {
				wantedQueries = 2
			}
			require.Len(t, captured, wantedQueries)
			v0222RequireUsageInsertSQL(t, captured[0], batch, batch && !bestEffort)
			if fallback {
				v0222RequireUsageInsertSQL(t, captured[1], false, false)
			}
		})
	}
}

func TestUsageLogV0222PersistenceScan68KeepsCostsTTLAndComposite(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%v", legacy), func(t *testing.T) {
			_, expected := v0222UsageFixture()
			if legacy {
				// 以下是独立协议位置，不从prepare或产品字段反推。
				for _, i := range []int{15, 16, 64, 65} {
					expected[i] = int64(0)
				}
				for _, i := range []int{5, 28, 57, 61, 62, 63} {
					expected[i] = nil
				}
			}
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			values := append([]driver.Value{int64(700)}, expected...)
			rows := sqlmock.NewRows(append([]string{"id"}, v0222UsageColumns(v0222UsageInsertColumnContract)...)).AddRow(values...)
			mock.ExpectQuery("SELECT ").WithArgs(int64(700)).WillReturnRows(rows)
			got, err := (&usageLogRepository{sql: db}).GetByID(context.Background(), 700)
			require.NoError(t, err)
			require.Equal(t, 12.5, got.TotalCost)
			require.Equal(t, 3.25, got.ActualCost)
			require.True(t, got.CreatedAt.Equal(time.Date(2026, 10, 1, 12, 34, 56, 123456000, time.UTC)))
			if legacy {
				require.Equal(t, "stored-model", got.RequestedModel)
				require.Nil(t, got.AccountRateMultiplier)
				require.Nil(t, got.AccountStatsCost)
				require.Nil(t, got.CompositeRouteID)
				require.Nil(t, got.CompositeRouteKind)
				require.Nil(t, got.CompositeFallbackChain)
				require.Zero(t, got.CacheCreation5mTokens)
				require.Zero(t, got.CacheCreation1hTokens)
				require.Zero(t, got.CacheCreation30mTokens)
				require.Zero(t, got.CacheCreation12hTokens)
			} else {
				require.Equal(t, "public-model", got.RequestedModel)
				require.Equal(t, 7.75, *got.AccountStatsCost)
				require.Equal(t, 1.5, *got.AccountRateMultiplier)
				require.EqualValues(t, 162, *got.CompositeRouteID)
				require.Equal(t, "compaction", *got.CompositeRouteKind)
				require.Equal(t, "primary (429) → backup", *got.CompositeFallbackChain)
				require.Equal(t, 17, got.CacheCreation5mTokens)
				require.Equal(t, 19, got.CacheCreation1hTokens)
				require.Equal(t, 73, got.CacheCreation30mTokens)
				require.Equal(t, 89, got.CacheCreation12hTokens)
				require.Equal(t, "upstream-id", *got.UpstreamRequestID)
				require.Equal(t, "session-id", *got.SessionID)
				require.True(t, got.NativeCompactionV2)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
