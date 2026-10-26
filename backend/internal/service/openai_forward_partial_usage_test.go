package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type openAIForwardPartialUsageDelayedReader struct {
	io.Reader
	firstRead sync.Once
}

func (r *openAIForwardPartialUsageDelayedReader) Read(p []byte) (int, error) {
	// Keep this fixture's upstream elapsed time observable on coarse clocks.
	r.firstRead.Do(func() { time.Sleep(20 * time.Millisecond) })
	return r.Reader.Read(p)
}

func TestOpenAIForwardFailedTerminalPreservesPartialUsage(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		name := "native"
		if passthrough {
			name = "passthrough"
		}
		t.Run(name, func(t *testing.T) {
			body := []byte("{\"model\":\"gpt-5.4\",\"stream\":true,\"instructions\":\"test instructions\",\"input\":[],\"service_tier\":\"priority\",\"reasoning\":{\"effort\":\"high\"}}")
			c := newOpenAIRejectedFieldTestContext(body)
			// A single terminal failure carries input/cache usage, without any visible output.
			stream := "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_partial\",\"object\":\"response\",\"model\":\"gpt-5.4\",\"service_tier\":\"default\",\"status\":\"failed\",\"output\":[],\"error\":{\"code\":\"server_error\",\"message\":\"upstream failed\"},\"usage\":{\"input_tokens\":11,\"input_tokens_details\":{\"cached_tokens\":7,\"cache_write_tokens\":3},\"output_tokens\":0}}}\n\n"
			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "X-Request-Id": []string{"rid_partial"}},
				Body:       io.NopCloser(&openAIForwardPartialUsageDelayedReader{Reader: strings.NewReader(stream)}),
			}}
			svc := newOpenAIRejectedFieldTestService(upstream)
			account := newOpenAIRejectedFieldTestAccount()
			account.Extra["openai_passthrough"] = passthrough
			result, err := svc.Forward(context.Background(), c, account, body)

			var failover *UpstreamFailoverError
			require.ErrorAs(t, err, &failover, "the original upstream failure must remain available to the handler")
			require.NotNil(t, result, "Forward must retain observed usage even before visible output")
			require.Equal(t, 11, result.Usage.InputTokens)
			require.Zero(t, result.Usage.OutputTokens)
			require.Equal(t, 7, result.Usage.CacheReadInputTokens)
			require.Equal(t, 3, result.Usage.CacheCreationInputTokens)
			require.Equal(t, "rid_partial", result.RequestID)
			require.Equal(t, "rid_partial", result.UpstreamHeaders.Get("x-request-id"))
			require.Equal(t, "resp_partial", result.ResponseID)
			require.Equal(t, "gpt-5.4", result.Model)
			if !passthrough {
				require.Equal(t, "gpt-5.4", result.UpstreamModel)
			}
			require.Equal(t, "gpt-5.4", result.UpstreamResponseModel)
			require.Equal(t, "default", result.UpstreamResponseServiceTier)
			require.NotNil(t, result.ServiceTier)
			require.Equal(t, "priority", *result.ServiceTier)
			require.NotNil(t, result.ReasoningEffort)
			require.Equal(t, "high", *result.ReasoningEffort)
			require.True(t, result.Stream)
			require.Positive(t, result.Duration)
			require.Len(t, upstream.requests, 1)
		})
	}
}

func TestOpenAIForwardFailedTerminalWithoutUsageKeepsNilResult(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		name := "native"
		if passthrough {
			name = "passthrough"
		}
		t.Run(name, func(t *testing.T) {
			body := []byte("{\"model\":\"gpt-5.4\",\"stream\":true,\"instructions\":\"test instructions\",\"input\":[]}")
			c := newOpenAIRejectedFieldTestContext(body)
			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"failed\"}}}\n\n")),
			}}
			svc := newOpenAIRejectedFieldTestService(upstream)
			account := newOpenAIRejectedFieldTestAccount()
			account.Extra["openai_passthrough"] = passthrough
			result, err := svc.Forward(context.Background(), c, account, body)
			var failover *UpstreamFailoverError
			require.True(t, errors.As(err, &failover))
			require.Nil(t, result, "zero usage must remain eligible for safe account failover")
		})
	}
}

func TestOpenAIForwardStreamErrorPreservesPartialImageBilling(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		name := "native"
		if passthrough {
			name = "passthrough"
		}
		t.Run(name, func(t *testing.T) {
			body := []byte("{\"model\":\"gpt-5.4\",\"stream\":true,\"tools\":[{\"type\":\"image_generation\",\"model\":\"gpt-image-2\",\"size\":\"2048x1152\"}],\"input\":\"draw\"}")
			c, _ := newOpenAIImageGenerationControlTestContext(true, "curl/8.0")
			stream := "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"ig_1\",\"type\":\"image_generation_call\",\"result\":\"final-image\",\"size\":\"2048x1152\"}}\n\n" +
				"data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_image\",\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"failed after image\"}}}\n\n"
			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(stream)),
			}}
			svc := newOpenAIRejectedFieldTestService(upstream)
			account := newOpenAIRejectedFieldTestAccount()
			account.Extra["openai_passthrough"] = passthrough
			result, err := svc.Forward(context.Background(), c, account, body)
			require.Error(t, err)
			require.NotNil(t, result, "image billing must survive an error even with zero token usage")
			require.Equal(t, 1, result.ImageCount)
			require.Equal(t, "gpt-image-2", result.BillingModel)
			require.Equal(t, "2K", result.ImageSize)
			require.Equal(t, "2048x1152", result.ImageInputSize)
			require.Equal(t, []string{"2048x1152"}, result.ImageOutputSizes)
			require.Equal(t, "resp_image", result.ResponseID)
		})
	}
}
