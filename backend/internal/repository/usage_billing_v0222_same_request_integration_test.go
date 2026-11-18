//go:build integration

package repository

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// 复用完整PG夹具，区分同request并发幂等与既有300条不同request的lane测试。
func TestCompositeUsageV0222BillingSameRequestConcurrentApplyOnce(t *testing.T) {
	for _, path := range []string{"repository", "lane"} {
		t.Run(path, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			f := newBatchBillingFixture(t, testEntClient(t), 100)
			repo := NewUsageBillingRepository(nil, integrationDB)
			apply := repo.Apply
			if path == "lane" {
				lanes := service.NewUsageBillingLaneCombiner(repo)
				defer lanes.Stop()
				apply = lanes.Apply
			}
			base := *f.commands("same-request-"+uuid.NewString(), []batchBillingStep{{
				key: 'A', account: 'P', balance: 1.25, keyQuota: 0.75, keyRate: 0.5, accountQuota: 0.25,
			}})[0]
			const count = 32
			results := make([]*service.UsageBillingApplyResult, count)
			errs := make([]error, count)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i := range count {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					// Normalize会写命令；每个调用独立副本，不共享可变指针。
					cmd := base
					<-start
					callCtx, callCancel := context.WithTimeout(ctx, 30*time.Second)
					defer callCancel()
					results[i], errs[i] = apply(callCtx, &cmd)
				}(i)
			}
			close(start)
			wg.Wait()
			var applied int
			for i, result := range results {
				require.NoError(t, errs[i], "request %d", i)
				require.NotNil(t, result, "request %d", i)
				if result.Applied {
					applied++
					require.NotNil(t, result.NewBalance)
					require.InDelta(t, 98.75, *result.NewBalance, 1e-8)
					require.False(t, result.BalanceOverdrafted)
				} else {
					require.Equal(t, &service.UsageBillingApplyResult{Applied: false}, result)
				}
			}
			require.Equal(t, 1, applied, "同request并发只能有一次已应用结算")
			state := readBatchBillingState(t, ctx, f)
			require.Equal(t, 1, state.dedupRows)
			for _, value := range []struct {
				name, raw string
				want      float64
			}{
				{"balance", state.balance, 98.75},
				{"key quota", state.keyAUsed, 0.75},
				{"key 5h", state.keyAUsage5h, 0.5},
				{"key 1d", state.keyAUsage1d, 0.5},
				{"account total", state.pUsed, 0.25},
				{"account daily", state.pDailyUsed, 0.25},
				{"account weekly unset", state.pWeeklyUsed, 0},
				{"subscription", state.subDaily, 0},
			} {
				got, err := strconv.ParseFloat(value.raw, 64)
				require.NoError(t, err, value.name)
				require.InDelta(t, value.want, got, 1e-8, value.name)
			}
			// 指纹必须由改后原始金额重算，不能复用已规范化命令的旧指纹。
			conflicting := base
			conflicting.BalanceCost = 2.5
			conflicting.RequestFingerprint = ""
			result, err := apply(ctx, &conflicting)
			require.ErrorIs(t, err, service.ErrUsageBillingRequestConflict)
			require.Nil(t, result)
			require.Equal(t, state, readBatchBillingState(t, ctx, f), "冲突不得再扣费或改变任何关联计数")
		})
	}
}
