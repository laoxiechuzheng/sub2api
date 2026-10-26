package repository

import (
	"bytes"
	"encoding/json"
)

// Validate a data line incrementally, retaining only container state and short
// top-level strings. Large terminal snapshots need no plaintext line buffer.
type diagnosticTerminalLine struct {
	prefix          int
	bad             bool
	sentinelPos     int
	sentinelInvalid bool
	stack           []diagnosticJSONFrame
	rootDone        bool
	mode            byte
	key             bool
	track           bool
	token           []byte
	escape          bool
	unicode         int
	number          byte
	literal         string
	literalPos      int
	topKey          string
	kind            string
	typeSeen        bool
	typeCandidate   bool
}

type diagnosticJSONFrame struct {
	kind  byte
	state byte // 0: first key/value, 1: colon, 2: required value, 3: comma/end, 4: required key.
}

func (s *diagnosticTerminalLine) observe(p []byte) {
	for _, c := range p {
		if s.bad && s.prefix < len("data:") {
			return
		}
		if s.prefix < len("data:") {
			if s.prefix == 0 && diagnosticJSONSpace(c) {
				continue
			}
			if c != "data:"[s.prefix] {
				s.bad = true
				return
			}
			s.prefix++
			continue
		}
		if !s.sentinelInvalid {
			switch {
			case s.sentinelPos == 0 && diagnosticJSONSpace(c):
			case s.sentinelPos < len("[DONE]") && c == "[DONE]"[s.sentinelPos]:
				s.sentinelPos++
			case s.sentinelPos == len("[DONE]") && diagnosticJSONSpace(c):
			default:
				s.sentinelInvalid = true
			}
		}
		if !s.bad {
			s.observeJSON(c)
		}
	}
}

func diagnosticJSONSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }

func (s *diagnosticTerminalLine) valueExpected() bool {
	if len(s.stack) == 0 {
		return !s.rootDone
	}
	f := s.stack[len(s.stack)-1]
	return f.state == 2 || (f.kind == '[' && f.state == 0)
}

func (s *diagnosticTerminalLine) valueDone() {
	if len(s.stack) == 0 {
		s.rootDone = true
		return
	}
	s.stack[len(s.stack)-1].state = 3
	if len(s.stack) == 1 {
		s.topKey = ""
		s.typeCandidate = false
	}
}

func (s *diagnosticTerminalLine) observeJSON(c byte) {
	if s.mode == 's' {
		if s.track {
			if len(s.token) < 256 {
				s.token = append(s.token, c)
			} else {
				s.track = false
				s.token = nil
			}
		}
		if s.unicode > 0 {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				s.bad = true
			}
			s.unicode--
			return
		}
		if s.escape {
			s.escape = false
			if c == 'u' {
				s.unicode = 4
			} else if !bytes.ContainsRune([]byte(`"\/bfnrt`), rune(c)) {
				s.bad = true
			}
			return
		}
		if c == '\\' {
			s.escape = true
			return
		}
		if c < 0x20 {
			s.bad = true
			return
		}
		if c != '"' {
			return
		}
		s.mode = 0
		var text string
		if s.track && json.Unmarshal(s.token, &text) != nil {
			s.bad = true
			return
		}
		if s.key {
			if len(s.stack) == 1 {
				s.topKey = text
				s.typeCandidate = text == "type" && !s.typeSeen
				s.typeSeen = s.typeSeen || text == "type"
			}
			s.stack[len(s.stack)-1].state = 1
		} else {
			if len(s.stack) == 1 && s.typeCandidate {
				s.kind = text
			}
			s.valueDone()
		}
		s.token = nil
		return
	}
	if s.mode == 'l' {
		if c != s.literal[s.literalPos] {
			s.bad = true
			return
		}
		s.literalPos++
		if s.literalPos == len(s.literal) {
			s.mode = 0
			s.valueDone()
		}
		return
	}
	if s.mode == 'n' {
		if s.numberByte(c) {
			return
		}
		if !s.numberComplete() {
			s.bad = true
			return
		}
		s.mode = 0
		s.valueDone()
	}
	if diagnosticJSONSpace(c) {
		return
	}
	if c == '"' {
		s.key = len(s.stack) > 0 && s.stack[len(s.stack)-1].kind == '{' && (s.stack[len(s.stack)-1].state == 0 || s.stack[len(s.stack)-1].state == 4)
		if !s.key && !s.valueExpected() {
			s.bad = true
			return
		}
		s.mode, s.track = 's', len(s.stack) == 1 && (s.key || s.topKey == "type")
		if s.track {
			s.token = []byte{'"'}
		}
		return
	}
	switch c {
	case '{', '[':
		if !s.valueExpected() || len(s.stack) >= 10000 {
			s.bad = true
			return
		}
		s.stack = append(s.stack, diagnosticJSONFrame{kind: c})
	case '}', ']':
		if len(s.stack) == 0 {
			s.bad = true
			return
		}
		f := s.stack[len(s.stack)-1]
		if (c == '}' && f.kind != '{') || (c == ']' && f.kind != '[') || (f.state != 0 && f.state != 3) {
			s.bad = true
			return
		}
		s.stack = s.stack[:len(s.stack)-1]
		s.valueDone()
	case ':':
		if len(s.stack) == 0 || s.stack[len(s.stack)-1].kind != '{' || s.stack[len(s.stack)-1].state != 1 {
			s.bad = true
			return
		}
		s.stack[len(s.stack)-1].state = 2
	case ',':
		if len(s.stack) == 0 || s.stack[len(s.stack)-1].state != 3 {
			s.bad = true
			return
		}
		f := &s.stack[len(s.stack)-1]
		if f.kind == '{' {
			f.state = 4
		} else {
			f.state = 2
		}
	case 't', 'f', 'n':
		if !s.valueExpected() {
			s.bad = true
			return
		}
		s.mode, s.literalPos = 'l', 1
		s.literal = map[byte]string{'t': "true", 'f': "false", 'n': "null"}[c]
	default:
		if !s.valueExpected() || (c != '-' && (c < '0' || c > '9')) {
			s.bad = true
			return
		}
		s.mode, s.number = 'n', 0
		if !s.numberByte(c) {
			s.bad = true
		}
	}
}

func (s *diagnosticTerminalLine) numberByte(c byte) bool {
	digit := c >= '0' && c <= '9'
	switch s.number {
	case 0:
		if c == '-' {
			s.number = 1
			return true
		}
		fallthrough
	case 1:
		if c == '0' {
			s.number = 2
			return true
		}
		if c >= '1' && c <= '9' {
			s.number = 3
			return true
		}
	case 2, 3:
		if s.number == 3 && digit {
			return true
		}
		if c == '.' {
			s.number = 4
			return true
		}
		if c == 'e' || c == 'E' {
			s.number = 6
			return true
		}
	case 4:
		if digit {
			s.number = 5
			return true
		}
	case 5:
		if digit {
			return true
		}
		if c == 'e' || c == 'E' {
			s.number = 6
			return true
		}
	case 6:
		if c == '+' || c == '-' {
			s.number = 7
			return true
		}
		fallthrough
	case 7:
		if digit {
			s.number = 8
			return true
		}
	case 8:
		if digit {
			return true
		}
	}
	return false
}

func (s *diagnosticTerminalLine) numberComplete() bool {
	return s.number == 2 || s.number == 3 || s.number == 5 || s.number == 8
}

func (s *diagnosticTerminalLine) terminal() bool {
	// [DONE] is an SSE sentinel, not JSON. Its parser failure is intentional.
	if s.prefix == 5 && !s.sentinelInvalid && s.sentinelPos == len("[DONE]") {
		return true
	}
	if s.bad || s.prefix != 5 {
		return false
	}
	if s.mode == 'n' && s.numberComplete() {
		s.mode = 0
		s.valueDone()
	}
	if s.mode != 0 || !s.rootDone || len(s.stack) != 0 {
		return false
	}
	switch s.kind {
	case "message_stop", "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "response.canceled", "error":
		return true
	}
	return false
}
