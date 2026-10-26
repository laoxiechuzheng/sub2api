package service

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/requestdiagnostic"
)

func expandDiagnosticPayloads(snapshot *requestdiagnostic.Snapshot) error {
	remaining := requestdiagnostic.MaxExpandedBodyBytes
	expand := func(payload *requestdiagnostic.Payload) error {
		if payload == nil {
			return nil
		}
		if payload.ContentGzip == nil {
			if payload.UncompressedBytes != 0 || !utf8.ValidString(payload.Content) {
				return errors.New("invalid stored request diagnostic text")
			}
			if len(payload.Content) > remaining {
				snapshot.StoredBytes = max(0, snapshot.StoredBytes-payload.StoredBytes)
				payload.Content, payload.StoredBytes = "", 0
				payload.Truncated, payload.OmittedReason = true, requestdiagnostic.OmittedDecompressionLimit
				snapshot.CaptureStatus = requestdiagnostic.CaptureStatusPartial
				return nil
			}
			remaining -= len(payload.Content)
			return nil
		}
		if snapshot.SchemaVersion != requestdiagnostic.CompressedSchemaVersion || payload.Content != "" ||
			payload.StoredBytes != len(payload.ContentGzip) || payload.UncompressedBytes < 0 ||
			payload.OriginalBytes < payload.UncompressedBytes {
			return errors.New("invalid stored compressed request diagnostic")
		}
		stored := payload.StoredBytes
		if payload.UncompressedBytes > remaining {
			payload.ContentGzip = nil
			payload.UncompressedBytes, payload.StoredBytes = 0, 0
			payload.Truncated, payload.OmittedReason = true, requestdiagnostic.OmittedDecompressionLimit
			snapshot.StoredBytes = max(0, snapshot.StoredBytes-stored)
			snapshot.CaptureStatus = requestdiagnostic.CaptureStatusPartial
			return nil
		}
		source := bytes.NewReader(payload.ContentGzip)
		reader, err := gzip.NewReader(source)
		if err != nil {
			return errors.New("invalid stored request diagnostic gzip")
		}
		reader.Multistream(false)
		// The declared length also bounds a dishonest small-length bomb. The
		// extra byte forces EOF/checksum verification for correctly sized bodies.
		body, readErr := io.ReadAll(io.LimitReader(reader, int64(payload.UncompressedBytes)+1))
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil || source.Len() != 0 || len(body) != payload.UncompressedBytes || !utf8.Valid(body) {
			return errors.New("invalid stored request diagnostic expanded payload")
		}
		remaining -= len(body)
		payload.Content = string(body)
		payload.ContentGzip = nil
		payload.UncompressedBytes = 0
		payload.StoredBytes = len(body)
		snapshot.StoredBytes += payload.StoredBytes - stored
		return nil
	}
	// Response takes the expansion budget before the two request copies, just
	// as it does during capture. All attempts share the one 512 MiB guard.
	for i := len(snapshot.Attempts) - 1; i >= 0; i-- {
		if err := expand(snapshot.Attempts[i].ResponseBody); err != nil {
			return err
		}
	}
	if err := expand(&snapshot.Meta.Body); err != nil {
		return err
	}
	for i := range snapshot.Attempts {
		if err := expand(&snapshot.Attempts[i].Body); err != nil {
			return err
		}
		if err := expand(&snapshot.Attempts[i].ResponseSummary); err != nil {
			return err
		}
	}
	return nil
}
