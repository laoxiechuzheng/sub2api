package requestdiagnostic

import (
	"bytes"
	"compress/gzip"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResponseBodyCompressorZeroBytesPreservesCompleteness(t *testing.T) {
	for _, tc := range []struct {
		name       string
		incomplete bool
	}{
		{name: "incomplete", incomplete: true},
		{name: "complete", incomplete: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := NewResponseBodyCompressor(4096).Finish(tc.incomplete)
			require.Equal(t, tc.incomplete, payload.Truncated)
			require.Equal(t, OmittedEmptyBody, payload.OmittedReason)
			require.Zero(t, payload.OriginalBytes)
			require.Zero(t, payload.StoredBytes)
			require.Zero(t, payload.UncompressedBytes)
			require.Empty(t, payload.Content)
			require.Nil(t, payload.ContentGzip)
		})
	}
}

func TestResponseBodyCompressorExpandedResourceGuardIsExplicit(t *testing.T) {
	compressor := NewResponseBodyCompressor(2 << 20)
	chunk := bytes.Repeat([]byte{'x'}, 1<<20)
	for i := 0; i < 513; i++ {
		compressor.Observe(chunk)
	}
	payload := compressor.Finish(false)
	require.Nil(t, payload.ContentGzip)
	require.Empty(t, payload.Content)
	require.Equal(t, 513<<20, payload.OriginalBytes)
	require.Zero(t, payload.StoredBytes)
	require.True(t, payload.Truncated)
	require.Equal(t, OmittedDecompressionLimit, payload.OmittedReason)
}

func TestResponseBodyCompressorStoredBudgetNeverEmitsInvalidGzip(t *testing.T) {
	compressor := NewResponseBodyCompressor(32)
	compressor.Observe([]byte(strings.Repeat("0123456789abcdefghijklmnopqrstuvwxyz", 100)))
	payload := compressor.Finish(false)
	require.Nil(t, payload.ContentGzip)
	require.True(t, payload.Truncated)
	require.Equal(t, OmittedBudget, payload.OmittedReason)
	require.Zero(t, payload.StoredBytes)
}

func TestResponseBodyCompressorValidatesUTF8AndInterruptedRune(t *testing.T) {
	for _, tc := range []struct {
		name         string
		body         []byte
		incomplete   bool
		text, reason string
	}{
		{"split_rune", []byte("fixture-\u4f60\U0001f642-tail"), false, "fixture-\u4f60\U0001f642-tail", ""},
		{"incomplete_rune", []byte("fixture-\xe4\xbd"), true, "fixture-", ""},
		{"incomplete_at_eof", []byte("fixture-\xe4\xbd"), false, "", OmittedInvalidUTF8},
		{"invalid_lead", []byte("fixture-\xff"), true, "", OmittedInvalidUTF8},
		{"invalid_middle", []byte("fixture-\xff\xe4\xbd"), true, "", OmittedInvalidUTF8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			compressor := NewResponseBodyCompressor(4096)
			for i := range tc.body {
				compressor.Observe(tc.body[i : i+1])
			}
			payload := compressor.Finish(tc.incomplete)
			require.Equal(t, tc.reason, payload.OmittedReason)
			require.Equal(t, len(tc.body), payload.OriginalBytes)
			if tc.reason != "" {
				require.Nil(t, payload.ContentGzip)
				require.True(t, payload.Truncated)
				return
			}
			reader, err := gzip.NewReader(bytes.NewReader(payload.ContentGzip))
			require.NoError(t, err)
			text, err := io.ReadAll(reader)
			require.NoError(t, err)
			require.NoError(t, reader.Close())
			require.Equal(t, tc.text, string(text))
			require.Equal(t, len(tc.text), payload.UncompressedBytes)
			require.Equal(t, tc.incomplete, payload.Truncated)
		})
	}
}
