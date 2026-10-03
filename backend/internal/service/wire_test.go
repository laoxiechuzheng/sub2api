package service

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/spf13/viper"
	"github.com/zeromicro/go-zero/core/collection"
)

func TestProvideConcurrencyServiceStartupCleanup(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured bool
		skip       bool
	}{
		{name: "nil config"},
		{name: "default cleanup", configured: true},
		{name: "rolling deployment", configured: true, skip: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cfg *config.Config
			if tc.configured {
				v := viper.New()
				v.Set("concurrency.skip_startup_cleanup", tc.skip)
				cfg = &config.Config{}
				if err := v.Unmarshal(cfg); err != nil {
					t.Fatal(err)
				}
			}
			cache := &trackingConcurrencyCache{}
			svc := ProvideConcurrencyService(cache, nil, cfg)
			if svc == nil {
				t.Fatal("expected concurrency service")
			}
			if tc.skip {
				if cache.cleanupPrefix != "" {
					t.Fatalf("rolling deployment must preserve another live process's slots, got cleanup prefix %q", cache.cleanupPrefix)
				}
			} else if cache.cleanupPrefix != RequestIDPrefix() {
				t.Fatalf("default startup cleanup changed: got %q", cache.cleanupPrefix)
			}
		})
	}
}

func TestConcurrencyStartupCleanupEnv(t *testing.T) {
	t.Setenv("CONCURRENCY_SKIP_STARTUP_CLEANUP", "true")
	v := viper.New()
	v.SetDefault("concurrency.skip_startup_cleanup", false)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	var cfg config.Config
	if err := v.Unmarshal(&cfg); err != nil {
		t.Fatal(err)
	}
	cache := &trackingConcurrencyCache{}
	ProvideConcurrencyService(cache, nil, &cfg)
	if cache.cleanupPrefix != "" {
		t.Fatal("environment-only rolling deployment must preserve active slots")
	}
}

func TestProvideTimingWheelService_ReturnsError(t *testing.T) {
	original := newTimingWheel
	t.Cleanup(func() { newTimingWheel = original })

	newTimingWheel = func(_ time.Duration, _ int, _ collection.Execute) (*collection.TimingWheel, error) {
		return nil, errors.New("boom")
	}

	svc, err := ProvideTimingWheelService()
	if err == nil {
		t.Fatalf("期望返回 error，但得到 nil")
	}
	if svc != nil {
		t.Fatalf("期望返回 nil svc，但得到非空")
	}
}

func TestProvideTimingWheelService_Success(t *testing.T) {
	svc, err := ProvideTimingWheelService()
	if err != nil {
		t.Fatalf("期望 err 为 nil，但得到: %v", err)
	}
	if svc == nil {
		t.Fatalf("期望 svc 非空，但得到 nil")
	}
	svc.Stop()
}
