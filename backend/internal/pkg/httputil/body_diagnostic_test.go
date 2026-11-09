//go:build unit

package httputil

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/requestdiagnostic"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

func TestRequestBodyDiagnosticCapturesDecodedOriginalBeforeRewrite(t *testing.T) {
	const original = " {\n\"model\":\"client-model\",\"input\":\"original text\"} "
	for _, encoding := range []string{"identity", "gzip", "zstd", "deflate"} {
		t.Run(encoding, func(t *testing.T) {
			var encoded bytes.Buffer
			switch encoding {
			case "identity":
				encoded.WriteString(original)
			case "gzip", "deflate":
				var writer io.WriteCloser
				if encoding == "gzip" {
					writer = gzip.NewWriter(&encoded)
				} else {
					writer = zlib.NewWriter(&encoded)
				}
				_, err := io.WriteString(writer, original)
				require.NoError(t, err)
				require.NoError(t, writer.Close())
			case "zstd":
				writer, err := zstd.NewWriter(&encoded)
				require.NoError(t, err)
				_, err = io.WriteString(writer, original)
				require.NoError(t, err)
				require.NoError(t, writer.Close())
			}
			var snapshot *requestdiagnostic.Snapshot
			capture := requestdiagnostic.NewCapture(requestdiagnostic.Options{MaxBytes: 4096, PreserveRaw: true}, requestdiagnostic.Inbound{}, func(s *requestdiagnostic.Snapshot) { snapshot = s })
			req := newRequestWithBody(t, encoded.Bytes(), encoding)
			req = req.WithContext(requestdiagnostic.WithCapture(req.Context(), capture))
			body, err := ReadLenientJSONRequestBodyWithPrealloc(req, 4096)
			require.NoError(t, err)
			require.Equal(t, original, string(body))
			capture.SetInbound([]byte(`{"model":"rewritten-model"}`))
			require.True(t, capture.BindUsage(1, "diagnostic-encoding", time.Now()))
			capture.Finish(http.StatusOK)
			require.NotNil(t, snapshot)
			require.Equal(t, original, snapshot.Meta.Body.Content)
			require.Equal(t, len(original), snapshot.Meta.Body.OriginalBytes)
		})
	}
}
