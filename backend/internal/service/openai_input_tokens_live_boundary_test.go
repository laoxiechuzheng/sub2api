package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	inputTokensBoundaryChildEnv = "SUB2API_OPENAI_INPUT_TOKENS_BOUNDARY_CHILD"
	inputTokensBoundaryMarker   = "input-tokens-boundary-sensitive-marker"
	inputTokensBoundaryCalls    = "INPUT_TOKENS_BOUNDARY_HTTP_CALLS="
)

type inputTokensBoundaryScenario struct {
	name      string
	optIn     string
	keyMode   string
	response  string
	wantCalls int
	wantEntry string
	wantLeaf  string
	wantExit  int
}

type inputTokensBoundaryEvidence struct {
	Scenario        string            `json:"scenario"`
	HTTPCalls       int               `json:"http_calls"`
	EntryStatus     string            `json:"entry_status"`
	LeafStatus      map[string]string `json:"leaf_status"`
	ChildExit       int               `json:"child_exit"`
	SensitiveOutput bool              `json:"sensitive_output"`
}

func TestOpenAIInputTokensLiveBoundary(t *testing.T) {
	scenarios := []inputTokensBoundaryScenario{
		{name: "without_opt_in", wantEntry: "SKIP"},
		{name: "zero_opt_in", optIn: "0", wantEntry: "SKIP"},
		{name: "true_opt_in", optIn: "true", wantEntry: "SKIP"},
		{name: "zero_padded_opt_in", optIn: "01", wantEntry: "SKIP"},
		{name: "spaced_opt_in", optIn: " 1 ", wantEntry: "SKIP"},
		{name: "without_key", optIn: "1", keyMode: "empty", wantEntry: "SKIP"},
		{name: "short_placeholder_key", optIn: "1", keyMode: "short", wantEntry: "SKIP"},
		{name: "explicit_opt_in", optIn: "1", wantCalls: 3, wantEntry: "PASS", wantLeaf: "PASS"},
		{name: "out_of_tolerance", optIn: "1", response: "out_of_tolerance", wantCalls: 3, wantEntry: "FAIL", wantLeaf: "FAIL", wantExit: 1},
		{name: "unauthorized", optIn: "1", response: "unauthorized", wantCalls: 3, wantEntry: "PASS", wantLeaf: "SKIP"},
		{name: "forbidden", optIn: "1", response: "forbidden", wantCalls: 3, wantEntry: "PASS", wantLeaf: "SKIP"},
		{name: "network_error", optIn: "1", response: "network_error", wantCalls: 3, wantEntry: "PASS", wantLeaf: "SKIP"},
		{name: "server_error", optIn: "1", response: "server_error", wantCalls: 3, wantEntry: "FAIL", wantLeaf: "FAIL", wantExit: 1},
		{name: "unexpected_redirect", optIn: "1", response: "unexpected_redirect", wantCalls: 3, wantEntry: "FAIL", wantLeaf: "FAIL", wantExit: 1},
		{name: "missing_count", optIn: "1", response: "missing_count", wantCalls: 3, wantEntry: "FAIL", wantLeaf: "FAIL", wantExit: 1},
		{name: "read_error", optIn: "1", response: "read_error", wantCalls: 3, wantEntry: "FAIL", wantLeaf: "FAIL", wantExit: 1},
	}
	if childScenario := os.Getenv(inputTokensBoundaryChildEnv); childScenario != "" {
		for _, scenario := range scenarios {
			if scenario.name == childScenario {
				runInputTokensBoundaryChild(t, scenario)
				return
			}
		}
		t.Fatal("unknown boundary child scenario")
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			executable, err := os.Executable()
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable,
				"-test.run=^TestOpenAIInputTokensLiveBoundary$", "-test.v", "-test.count=1", "-test.timeout=30s")
			cmd.Env = []string{inputTokensBoundaryChildEnv + "=" + scenario.name}
			if runtime.GOOS == "windows" {
				cmd.Env = append(cmd.Env, `SYSTEMROOT=C:\Windows`)
			}
			output, runErr := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatal("boundary child exceeded its execution deadline")
			}
			exitCode := 0
			if runErr != nil {
				if exitErr, ok := runErr.(*exec.ExitError); ok {
					exitCode = exitErr.ExitCode()
				} else {
					t.Fatal("boundary child could not start")
				}
			}

			evidence, ok := parseInputTokensBoundaryEvidence(scenario.name, string(output), exitCode)
			require.True(t, ok, "boundary child did not emit complete structured evidence")
			encoded, err := json.Marshal(evidence)
			require.NoError(t, err)
			t.Logf("live_boundary_evidence %s", encoded)
			if evidence.SensitiveOutput {
				t.Error("boundary child output contained forbidden synthetic data")
			}
			require.Equal(t, scenario.wantExit, evidence.ChildExit, "boundary child process exit")
			require.Equal(t, scenario.wantCalls, evidence.HTTPCalls, "entry HTTP boundary calls")
			require.Equal(t, scenario.wantEntry, evidence.EntryStatus)
			if scenario.wantLeaf == "" {
				require.Empty(t, evidence.LeafStatus)
			} else {
				require.Equal(t, map[string]string{
					"simple_user_text": scenario.wantLeaf,
					"system_plus_tool": scenario.wantLeaf,
					"multi_turn_text":  scenario.wantLeaf,
				}, evidence.LeafStatus, "original comparison leaf terminals")
			}
		})
	}
}

func runInputTokensBoundaryChild(t *testing.T, scenario inputTokensBoundaryScenario) {
	var calls atomic.Int32
	previous := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		index := int(calls.Add(1)) - 1
		counts := []int{6, 50, 24}
		if index >= len(counts) {
			return nil, fmt.Errorf("unexpected boundary HTTP call")
		}
		actual := counts[index]
		if scenario.response == "out_of_tolerance" {
			actual = 4096
		}
		status := http.StatusOK
		responseBody := fmt.Sprintf(`{"input_tokens":%d}`, actual)
		switch scenario.response {
		case "unauthorized":
			status, responseBody = http.StatusUnauthorized, inputTokensBoundaryMarker
		case "forbidden":
			status, responseBody = http.StatusForbidden, inputTokensBoundaryMarker
		case "network_error":
			return nil, inputTokensBoundaryNetworkError{}
		case "server_error":
			status, responseBody = http.StatusInternalServerError, inputTokensBoundaryMarker
		case "unexpected_redirect":
			status, responseBody = http.StatusFound, inputTokensBoundaryMarker
		case "missing_count":
			responseBody = fmt.Sprintf(`{"error":%q}`, inputTokensBoundaryMarker)
		}
		var responseReader io.ReadCloser = io.NopCloser(strings.NewReader(responseBody))
		if scenario.response == "read_error" {
			responseReader = inputTokensBoundaryReadError{}
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"X-Synthetic-Private": []string{inputTokensBoundaryMarker}},
			Body:       responseReader,
		}, nil
	})
	defer func() {
		http.DefaultTransport = previous
		fmt.Printf("%s%d\n", inputTokensBoundaryCalls, calls.Load())
	}()
	if scenario.optIn != "" {
		t.Setenv("OPENAI_INPUT_TOKENS_LIVE_TEST", scenario.optIn)
	}
	apiKey := "synthetic-" + inputTokensBoundaryMarker + strings.Repeat("x", 32)
	if scenario.keyMode == "empty" {
		apiKey = ""
	} else if scenario.keyMode == "short" {
		apiKey = string([]byte{'s', 'k', '-'}) + "stub"
	}
	t.Setenv("OPENAI_API_KEY", apiKey)
	t.Run("entry", TestEstimateOpenAIInputTokens_CompareWithOpenAIAPI)
}

type inputTokensBoundaryNetworkError struct{}

func (inputTokensBoundaryNetworkError) Error() string   { return inputTokensBoundaryMarker }
func (inputTokensBoundaryNetworkError) Timeout() bool   { return false }
func (inputTokensBoundaryNetworkError) Temporary() bool { return false }

type inputTokensBoundaryReadError struct{}

func (inputTokensBoundaryReadError) Read([]byte) (int, error) {
	return 0, fmt.Errorf("%s", inputTokensBoundaryMarker)
}

func (inputTokensBoundaryReadError) Close() error { return nil }

func parseInputTokensBoundaryEvidence(scenario, output string, exitCode int) (inputTokensBoundaryEvidence, bool) {
	evidence := inputTokensBoundaryEvidence{
		Scenario: scenario, HTTPCalls: -1, ChildExit: exitCode,
		LeafStatus: make(map[string]string), SensitiveOutput: strings.Contains(output, inputTokensBoundaryMarker),
	}
	const entryName = "TestOpenAIInputTokensLiveBoundary/entry"
	allowedLeaves := map[string]bool{"simple_user_text": true, "system_plus_tool": true, "multi_turn_text": true}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, inputTokensBoundaryCalls) {
			value, err := strconv.Atoi(strings.TrimPrefix(line, inputTokensBoundaryCalls))
			if err != nil || evidence.HTTPCalls >= 0 {
				return evidence, false
			}
			evidence.HTTPCalls = value
		}
		for _, status := range []string{"PASS", "FAIL", "SKIP"} {
			prefix := "--- " + status + ": "
			if !strings.HasPrefix(line, prefix) {
				continue
			}
			name := strings.SplitN(strings.TrimPrefix(line, prefix), " ", 2)[0]
			if name == entryName {
				if evidence.EntryStatus != "" {
					return evidence, false
				}
				evidence.EntryStatus = status
			} else if strings.HasPrefix(name, entryName+"/") {
				leaf := strings.TrimPrefix(name, entryName+"/")
				if !allowedLeaves[leaf] || evidence.LeafStatus[leaf] != "" {
					return evidence, false
				}
				evidence.LeafStatus[leaf] = status
			}
		}
	}
	return evidence, evidence.HTTPCalls >= 0 && evidence.EntryStatus != ""
}
