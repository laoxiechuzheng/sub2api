package repository

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHTTPUpstreamDiagnosticTerminalObserverRejectsFalseTerminals(t *testing.T) {
	for _, fixture := range []struct {
		line     string
		terminal bool
	}{
		{`data: {"type":"delta","type":"response.completed"}`, false},
		{`data: {"type":"response.completed","type":"delta"}`, true},
		{`data: {"type":{},"type":"response.completed"}`, false},
		{`data: {"t\u0079pe":"response.\u0063ompleted","n":[0,-1.25e+3,true,false,null,{"x":"\\\""}]}`, true},
		{`data: {"type":"response.completed","x":1e}`, false},
		{`data: {"type":"response.completed","x":[1,]}`, false},
		{`data: {"type":"response.completed","x":"\q"}`, false},
		{"data: " + strings.Repeat(" ", 100000) + "[DONE]" + strings.Repeat("\t", 100000), true},
	} {
		for _, chunkSize := range []int{1, 7, 4096} {
			var observer diagnosticTerminalLine
			for start := 0; start < len(fixture.line); start += chunkSize {
				observer.observe([]byte(fixture.line[start:min(start+chunkSize, len(fixture.line))]))
			}
			require.Equal(t, fixture.terminal, observer.terminal(), "line bytes=%d chunk=%d", len(fixture.line), chunkSize)
		}
	}
}
