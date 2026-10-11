package requestdiagnostic

import "unicode/utf8"

func rawPayload(raw []byte, maxBytes int) Payload {
	if len(raw) == 0 {
		return omittedPayload(0, OmittedEmptyBody)
	}
	if !utf8.Valid(raw) {
		return omittedPayload(len(raw), OmittedInvalidUTF8)
	}
	payload := Payload{Content: string(raw), OriginalBytes: len(raw)}
	if len(raw) > maxBytes {
		payload.Content = utf8Prefix(payload.Content, maxBytes)
		payload.Truncated = true
	}
	payload.StoredBytes = len(payload.Content)
	return payload
}

func rawResponsePayload(raw []byte, originalBytes, maxBytes int, incomplete bool) Payload {
	originalBytes = max(originalBytes, len(raw))
	// A bounded transport prefix can end inside the last UTF-8 character.
	if (incomplete || originalBytes > len(raw)) && !utf8.Valid(raw) {
		for size := len(raw) - 1; size >= max(0, len(raw)-utf8.UTFMax+1); size-- {
			if utf8.Valid(raw[:size]) && !utf8.FullRune(raw[size:]) {
				raw = raw[:size]
				break
			}
		}
	}
	payload := rawPayload(raw, maxBytes)
	payload.OriginalBytes = originalBytes
	payload.Truncated = payload.Truncated || incomplete
	if originalBytes > payload.StoredBytes && payload.OmittedReason == "" {
		payload.Truncated = true
		payload.OmittedReason = OmittedBudget
	}
	return payload
}
