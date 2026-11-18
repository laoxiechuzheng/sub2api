package apicompat

import "github.com/tidwall/gjson"

// CacheUsage 只归一缓存细分；不推断总量、输出或 request TTL。
type CacheUsage struct {
	Read   int
	Write  int
	TTL30m int
	TTL12h int
}

// CacheUsageFromGJSON 保留既有 gjson 缓存字段的 presence/首值语义。
// 重复 JSON 键不属于支持输入；不扫描或改变非缓存字段。
func CacheUsageFromGJSON(usage gjson.Result) CacheUsage {
	read := cacheReadTokens(usage)
	write := cacheWriteTokens(usage)
	short, long := cacheWriteTTLTokens(usage, write)
	return CacheUsage{Read: read, Write: write, TTL30m: short, TTL12h: long}
}

func cacheReadTokens(value gjson.Result) int {
	for _, nested := range []gjson.Result{
		value.Get("input_tokens_details.cached_tokens"),
		value.Get("prompt_tokens_details.cached_tokens"),
	} {
		if nested.Exists() {
			return max(int(nested.Int()), 0)
		}
	}
	return firstPositiveCacheInt(
		value.Get("cache_read_input_tokens"),
		value.Get("cache_read_tokens"),
		value.Get("cached_tokens"),
		value.Get("prompt_cache_hit_tokens"),
	)
}

func cacheWriteTokens(value gjson.Result) int {
	for _, nested := range []gjson.Result{
		value.Get("input_tokens_details.cache_write_tokens"),
		value.Get("prompt_tokens_details.cache_write_tokens"),
		value.Get("input_tokens_details.cache_creation_tokens"),
		value.Get("prompt_tokens_details.cache_creation_tokens"),
	} {
		if nested.Exists() {
			return max(int(nested.Int()), 0)
		}
	}
	return firstPositiveCacheInt(
		value.Get("cache_write_tokens"),
		value.Get("cache_creation_input_tokens"),
		value.Get("cache_write_input_tokens"),
		value.Get("cache_creation_tokens"),
	)
}

func firstPositiveCacheInt(values ...gjson.Result) int {
	for _, value := range values {
		if v := value.Int(); v > 0 {
			return int(v)
		}
	}
	return 0
}

func cacheWriteTTLTokens(value gjson.Result, aggregate int) (int, int) {
	for _, path := range []string{"input_tokens_details", "prompt_tokens_details"} {
		details := value.Get(path + ".cache_write_tokens_details")
		if !details.Exists() {
			continue
		}
		short, long := details.Get("ttl_30m_tokens"), details.Get("ttl_12h_tokens")
		if short.Type != gjson.Number || long.Type != gjson.Number {
			return 0, 0
		}
		a, b := short.Int(), long.Int()
		if a < 0 || b < 0 || float64(a) != short.Float() || float64(b) != long.Float() ||
			a > int64(aggregate) || b != int64(aggregate)-a {
			return 0, 0
		}
		return int(a), int(b)
	}
	return 0, 0
}
