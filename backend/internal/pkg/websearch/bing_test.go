package websearch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBingRSSProvider_Name(t *testing.T) {
	p := NewBingRSSProvider(nil)
	require.Equal(t, ProviderTypeBingRSS, p.Name())
}

func TestBingRSSProvider_SearchParsesResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "OpenAI Codex", r.URL.Query().Get("q"))
		require.Equal(t, "rss", r.URL.Query().Get("format"))
		require.Equal(t, "3", r.URL.Query().Get("count"))
		require.Equal(t, "application/rss+xml, application/xml;q=0.9, */*;q=0.1", r.Header.Get("Accept"))
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(`<?xml version="1.0"?><rss version="2.0"><channel>
			<item><title>OpenAI</title><link>https://openai.com</link><description>Latest &amp; useful</description><pubDate>Sun, 23 Aug 2026 05:12:00 GMT</pubDate></item>
			<item><title>Codex</title><link>https://openai.com/codex</link><description>Agent tooling</description></item>
		</channel></rss>`))
	}))
	defer srv.Close()

	p := NewBingRSSProvider(srv.Client())
	originalURL := *bingRSSSearchURL
	*bingRSSSearchURL = *mustParseSearchURL(t, srv.URL)
	defer func() { *bingRSSSearchURL = originalURL }()

	resp, err := p.Search(context.Background(), SearchRequest{Query: "OpenAI Codex", MaxResults: 3})
	require.NoError(t, err)
	require.Equal(t, "OpenAI Codex", resp.Query)
	require.Len(t, resp.Results, 2)
	require.Equal(t, "https://openai.com", resp.Results[0].URL)
	require.Equal(t, "Latest & useful", resp.Results[0].Snippet)
	require.Equal(t, "Sun, 23 Aug 2026 05:12:00 GMT", resp.Results[0].PageAge)
}

func TestBingRSSProvider_SearchErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream unavailable"))
	}))
	defer srv.Close()

	p := NewBingRSSProvider(srv.Client())
	originalURL := *bingRSSSearchURL
	*bingRSSSearchURL = *mustParseSearchURL(t, srv.URL)
	defer func() { *bingRSSSearchURL = originalURL }()

	_, err := p.Search(context.Background(), SearchRequest{Query: "test"})
	require.ErrorContains(t, err, "bing_rss: status 502")
}

func mustParseSearchURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}
