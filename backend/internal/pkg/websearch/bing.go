package websearch

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

const (
	bingRSSSearchEndpoint = "https://www.bing.com/search"
	bingRSSProviderName   = ProviderTypeBingRSS
	bingRSSMaxCount       = 20
)

var bingRSSSearchURL, _ = url.Parse(bingRSSSearchEndpoint) //nolint:errcheck

// BingRSSProvider provides a no-credential search fallback for Codex
// standalone search. It is intentionally separate from the configurable
// Brave/Tavily emulation providers and is only used when an OpenAI-compatible
// upstream lacks /v1/alpha/search.
type BingRSSProvider struct {
	httpClient *http.Client
}

func NewBingRSSProvider(httpClient *http.Client) *BingRSSProvider {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &BingRSSProvider{httpClient: httpClient}
}

func (b *BingRSSProvider) Name() string { return bingRSSProviderName }

func (b *BingRSSProvider) Search(ctx context.Context, req SearchRequest) (*SearchResponse, error) {
	count := req.MaxResults
	if count <= 0 {
		count = defaultMaxResults
	}
	if count > bingRSSMaxCount {
		count = bingRSSMaxCount
	}

	u := *bingRSSSearchURL
	query := u.Query()
	query.Set("q", req.Query)
	query.Set("format", "rss")
	query.Set("count", strconv.Itoa(count))
	u.RawQuery = query.Encode()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("bing_rss: build request: %w", err)
	}
	httpReq.Header.Set("Accept", "application/rss+xml, application/xml;q=0.9, */*;q=0.1")
	httpReq.Header.Set("User-Agent", "sub2api-codex-search/1.0")

	resp, err := b.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("bing_rss: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return nil, fmt.Errorf("bing_rss: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bing_rss: status %d: %s", resp.StatusCode, truncateBody(body))
	}

	var raw bingRSSResponse
	if err := xml.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("bing_rss: decode response: %w", err)
	}

	results := make([]SearchResult, 0, len(raw.Channel.Items))
	for _, item := range raw.Channel.Items {
		if item.Link == "" || item.Title == "" {
			continue
		}
		results = append(results, SearchResult{
			URL:     item.Link,
			Title:   item.Title,
			Snippet: item.Description,
			PageAge: item.PubDate,
		})
		if len(results) >= count {
			break
		}
	}
	return &SearchResponse{Results: results, Query: req.Query}, nil
}

type bingRSSResponse struct {
	Channel bingRSSChannel `xml:"channel"`
}

type bingRSSChannel struct {
	Items []bingRSSItem `xml:"item"`
}

type bingRSSItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	Description string `xml:"description"`
	PubDate     string `xml:"pubDate"`
}
