package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type openAIWSTTFTModeTestEvent struct {
	delay   time.Duration
	payload string
}

// openAIWSTTFTModeTestTurn replays a reasoning turn: the reasoning item is
// announced shortly after the out-of-band codex.* frame, the first delta only
// arrives after the reasoning ends.
func openAIWSTTFTModeTestTurn(responseID string, startDelay, reasoningDelay time.Duration) []openAIWSTTFTModeTestEvent {
	return []openAIWSTTFTModeTestEvent{
		{startDelay, `{"type":"response.created","response":{"id":"` + responseID + `","model":"gpt-5.4"}}`},
		{0, `{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":1}}}`},
		{0, `{"type":"response.in_progress","response":{"id":"` + responseID + `"}}`},
		{300 * time.Millisecond, `{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_` + responseID + `","type":"reasoning","summary":[]}}`},
		{reasoningDelay, `{"type":"response.output_text.delta","output_index":1,"delta":"Hi"}`},
		{0, `{"type":"response.completed","response":{"id":"` + responseID + `","model":"gpt-5.4","usage":{"input_tokens":3,"output_tokens":2}}}`},
	}
}

// openAIWSTTFTModeHTTPExpectedMs is the first-token time the SSE path would
// report for the same events: the delay up to the first event that
// openAIStreamDataStartsTTFT accepts. codex.* frames only exist on WebSocket.
func openAIWSTTFTModeHTTPExpectedMs(t *testing.T, events []openAIWSTTFTModeTestEvent, mode string) time.Duration {
	t.Helper()
	var elapsed time.Duration
	for _, event := range events {
		elapsed += event.delay
		eventType := gjson.Get(event.payload, "type").String()
		if strings.HasPrefix(eventType, "codex.") {
			continue
		}
		if openAIStreamDataStartsTTFT(event.payload, eventType, false, mode) {
			return elapsed
		}
	}
	t.Fatalf("no first-token event for mode %s", mode)
	return 0
}

func TestOpenAIWSPassthroughFirstTokenFollowsTTFTMode(t *testing.T) {
	for _, mode := range []string{OpenAITTFTModeSemantic, OpenAITTFTModeVisible} {
		t.Run(mode, func(t *testing.T) {
			runOpenAIWSPassthroughTTFTModeCase(t, mode)
		})
	}
}

func runOpenAIWSPassthroughTTFTModeCase(t *testing.T, mode string) {
	runOpenAIWSPassthroughTTFTModeSession(t, mode, false)
}

func runOpenAIWSPassthroughTTFTModeSession(t *testing.T, mode string, admissionBubble bool) {
	gin.SetMode(gin.TestMode)
	gatewayForwardingCache.Store(&cachedGatewayForwardingSettings{openAITTFTMode: mode, expiresAt: time.Now().Add(time.Minute).UnixNano()})
	t.Cleanup(func() {
		gatewayForwardingCache.Store(&cachedGatewayForwardingSettings{openAITTFTMode: OpenAITTFTModeSemantic, expiresAt: time.Now().Add(time.Minute).UnixNano()})
	})

	const reasoningDelay = 600 * time.Millisecond
	const tolerance = 250 * time.Millisecond
	// Each turn starts its event delays only after its own upstream create succeeds.
	turns := [][]openAIWSTTFTModeTestEvent{
		openAIWSTTFTModeTestTurn("resp_ttft_1", 0, reasoningDelay),
		openAIWSTTFTModeTestTurn("resp_ttft_2", 200*time.Millisecond, reasoningDelay),
	}

	upstreamConn := newOpenAIWSTTFTModeConn(turns)
	defer func() { _ = upstreamConn.Close() }()

	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	cfg.Gateway.OpenAIWS.IngressModeDefault = OpenAIWSIngressModeCtxPool
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3
	svc := &OpenAIGatewayService{
		cfg:                       cfg,
		httpUpstream:              &httpUpstreamRecorder{},
		cache:                     &stubGatewayCache{},
		openaiWSResolver:          NewOpenAIWSProtocolResolver(cfg),
		toolCorrector:             NewCodexToolCorrector(),
		openaiWSPassthroughDialer: &openAIWSTTFTModeDialer{conn: upstreamConn},
	}
	account := &Account{
		ID: 454, Name: "openai-ws-ttft-mode", Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test"},
		Extra:       map[string]any{"openai_apikey_responses_websockets_v2_mode": OpenAIWSIngressModePassthrough},
	}

	resultCh := make(chan *OpenAIForwardResult, len(turns))
	startedCh := make(chan time.Time, len(turns))
	admissionCh := make(chan time.Time, 1)
	hooks := &OpenAIWSIngressHooks{
		TurnStarted: func(_ int, startedAt time.Time) { startedCh <- startedAt },
		BeforeTurn: func(turn int) error {
			if admissionBubble && turn == 2 {
				// 仅synctest/net.Pipe：显式100ms虚拟准入timer，不sleep或改产品clock。
				admissionCh <- time.Now()
				timer := time.NewTimer(100 * time.Millisecond)
				defer timer.Stop()
				<-timer.C
			}
			return nil
		},
		AfterTurn: func(_ int, result *OpenAIForwardResult, turnErr error) {
			if turnErr == nil && result != nil {
				resultCh <- result
			}
		},
	}
	serverDone := make(chan struct{})
	serve := func(conn *coderws.Conn, r *http.Request) {
		defer close(serverDone)
		defer func() { _ = conn.CloseNow() }()
		ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ginCtx.Request = r.Clone(r.Context())
		readCtx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		_, firstMessage, readErr := conn.Read(readCtx)
		cancel()
		if readErr != nil {
			return
		}
		hooks.InitialTurnStartedAt = time.Now()
		_ = svc.ProxyResponsesWebSocketFromClient(r.Context(), ginCtx, conn, account, "sk-test", firstMessage, hooks)
	}
	dialURL := "http://ttft-fixture.invalid/v1"
	var dialOptions *coderws.DialOptions
	if admissionBubble {
		dialOptions = &coderws.DialOptions{HTTPClient: &http.Client{Transport: openAIWSTTFTModePipeTransport{serve: serve}}}
	} else {
		wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := coderws.Accept(w, r, nil)
			if err != nil {
				close(serverDone)
				return
			}
			serve(conn, r)
		}))
		defer wsServer.Close()
		dialURL = "ws" + strings.TrimPrefix(wsServer.URL, "http")
	}

	dialCtx, cancelDial := context.WithTimeout(context.Background(), 3*time.Second)
	clientConn, _, err := coderws.Dial(dialCtx, dialURL, dialOptions)
	cancelDial()
	require.NoError(t, err)
	defer func() {
		_ = clientConn.CloseNow()
		_ = upstreamConn.Close()
		select {
		case <-serverDone:
		case <-time.After(5 * time.Second):
			t.Error("TTFT proxy handler did not exit")
		}
	}()

	request := []byte(`{"type":"response.create","model":"gpt-5.4","input":"hi"}`)
	for i, turn := range turns {
		createSentAt := time.Now()
		selectedIndex := 3
		selectedType := "response.output_item.added"
		if mode == OpenAITTFTModeVisible {
			selectedIndex, selectedType = 4, "response.output_text.delta"
		}
		var selectedReceivedAt time.Time
		writeCtx, cancelWrite := context.WithTimeout(context.Background(), 3*time.Second)
		require.NoError(t, clientConn.Write(writeCtx, coderws.MessageText, request))
		cancelWrite()
		for {
			readCtx, cancelRead := context.WithTimeout(context.Background(), 5*time.Second)
			_, message, readErr := clientConn.Read(readCtx)
			cancelRead()
			require.NoError(t, readErr)
			if gjson.GetBytes(message, "type").String() == selectedType && selectedReceivedAt.IsZero() {
				selectedReceivedAt = time.Now()
			}
			if gjson.GetBytes(message, "type").String() == "response.completed" {
				break
			}
		}

		var result *OpenAIForwardResult
		select {
		case result = <-resultCh:
		case <-time.After(5 * time.Second):
			t.Fatalf("turn %d result was not reported", i+1)
		}
		var startedAt time.Time
		select {
		case startedAt = <-startedCh:
		case <-time.After(5 * time.Second):
			t.Fatalf("turn %d origin was not reported", i+1)
		}
		writtenAt, eventStartedAt, emittedAt, _ := upstreamConn.snapshot(i, selectedIndex)
		require.False(t, startedAt.Before(createSentAt), "origin must belong to this client create")
		require.False(t, startedAt.After(writtenAt), "origin cannot follow the successful upstream create")
		require.False(t, eventStartedAt.Before(writtenAt), "event delay cannot start before successful create")
		require.False(t, emittedAt.IsZero() || selectedReceivedAt.IsZero(), "the independently selected event must traverse the Proxy")
		if admissionBubble && i == 1 {
			admissionStartedAt := <-admissionCh
			require.Equal(t, admissionStartedAt, startedAt, "turn origin must include test-controlled admission, not write/fallback time")
		}
		var independentDelay time.Duration
		for index := 0; index <= selectedIndex; index++ {
			independentDelay += turn[index].delay
		}
		expected := openAIWSTTFTModeHTTPExpectedMs(t, turn, mode)
		require.Equal(t, independentDelay, expected, "fixed event choice must independently agree with the SSE classifier")
		require.NotNil(t, result.FirstTokenMs, "turn %d must record first token", i+1)
		got := time.Duration(*result.FirstTokenMs) * time.Millisecond
		require.GreaterOrEqual(t, int64(*result.FirstTokenMs), emittedAt.Sub(startedAt).Milliseconds(), "TTFT cannot precede the chosen upstream event")
		require.LessOrEqual(t, int64(*result.FirstTokenMs), selectedReceivedAt.Sub(startedAt).Milliseconds(), "TTFT cannot follow client receipt of the chosen event")
		require.GreaterOrEqual(t, got, expected, "turn %d first token must not precede the SSE first-token event", i+1)
		require.Less(t, got, expected+tolerance, "turn %d first token must match the SSE rule for mode %s", i+1, mode)
	}
	_ = clientConn.Close(coderws.StatusNormalClosure, "done")
}

// 本fixture只服务TTFT两轮；不改变共享capture或产品时钟。
type openAIWSTTFTModeTurnState struct {
	events    []openAIWSTTFTModeTestEvent
	ready     chan struct{}
	opened    bool
	writtenAt time.Time
	startedAt []time.Time
	emittedAt []time.Time
}

type openAIWSTTFTModeConn struct {
	mu         sync.Mutex
	writes     openAIWSCaptureConn
	turns      []openAIWSTTFTModeTurnState
	nextTurn   int
	nextEvent  int
	nextWrite  int
	reading    bool
	closed     bool
	done       chan struct{}
	activeRead sync.WaitGroup
}

func newOpenAIWSTTFTModeConn(turns [][]openAIWSTTFTModeTestEvent) *openAIWSTTFTModeConn {
	c := &openAIWSTTFTModeConn{done: make(chan struct{})}
	for _, events := range turns {
		c.turns = append(c.turns, openAIWSTTFTModeTurnState{
			events: events, ready: make(chan struct{}),
			startedAt: make([]time.Time, len(events)), emittedAt: make([]time.Time, len(events)),
		})
	}
	return c
}

func (c *openAIWSTTFTModeConn) WriteJSON(ctx context.Context, value any) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		_ = c.Close()
		return err
	}
	payload, err := json.Marshal(value)
	if err != nil || gjson.GetBytes(payload, "type").String() != "response.create" {
		_ = c.Close()
		return errors.New("TTFT fixture requires response.create")
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errOpenAIWSConnClosed
	}
	if c.nextWrite >= len(c.turns) {
		c.mu.Unlock()
		_ = c.Close()
		return errors.New("TTFT fixture received an unexpected turn")
	}
	// 这里仅同步写入内存recorder；成功返回之后才发布本轮gate。
	if err := c.writes.WriteJSON(ctx, value); err != nil {
		c.mu.Unlock()
		_ = c.Close()
		return err
	}
	turn := &c.turns[c.nextWrite]
	turn.writtenAt = time.Now()
	turn.opened = true
	close(turn.ready)
	c.nextWrite++
	c.mu.Unlock()
	return nil
}

func (c *openAIWSTTFTModeConn) WriteFrame(ctx context.Context, _ coderws.MessageType, payload []byte) error {
	return c.WriteJSON(ctx, json.RawMessage(payload))
}

func (c *openAIWSTTFTModeConn) ReadMessage(ctx context.Context) (payload []byte, readErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errOpenAIWSConnClosed
	}
	if c.reading {
		c.mu.Unlock()
		return nil, errors.New("TTFT fixture does not support concurrent readers")
	}
	if c.nextTurn >= len(c.turns) {
		c.mu.Unlock()
		return nil, io.EOF
	}
	turnIndex, eventIndex := c.nextTurn, c.nextEvent
	turn := &c.turns[turnIndex]
	c.reading = true
	c.activeRead.Add(1)
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.reading = false
		c.mu.Unlock()
		c.activeRead.Done()
		if readErr != nil {
			_ = c.Close()
		}
	}()

	// 先等本轮上游create成功；预读不得消耗这一轮任何delay。
	select {
	case <-turn.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, errOpenAIWSConnClosed
	}
	c.mu.Lock()
	if c.closed || ctx.Err() != nil {
		c.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, errOpenAIWSConnClosed
	}
	event := turn.events[eventIndex]
	turn.startedAt[eventIndex] = time.Now()
	c.mu.Unlock()
	if event.delay > 0 {
		timer := time.NewTimer(event.delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.done:
			return nil, errOpenAIWSConnClosed
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errOpenAIWSConnClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	turn.emittedAt[eventIndex] = time.Now()
	c.nextEvent++
	if c.nextEvent == len(turn.events) {
		c.nextTurn++
		c.nextEvent = 0
	}
	return []byte(event.payload), nil
}

func (c *openAIWSTTFTModeConn) ReadFrame(ctx context.Context) (coderws.MessageType, []byte, error) {
	payload, err := c.ReadMessage(ctx)
	return coderws.MessageText, payload, err
}

func (c *openAIWSTTFTModeConn) Ping(context.Context) error { return nil }

func (c *openAIWSTTFTModeConn) Close() error {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		close(c.done)
		for i := range c.turns {
			if !c.turns[i].opened {
				c.turns[i].opened = true
				close(c.turns[i].ready)
			}
		}
	}
	c.mu.Unlock()
	_ = c.writes.Close()
	// Add与closed检查同锁，禁止Close开始之后再Add；等待时不持锁。
	c.activeRead.Wait()
	return nil
}

func (c *openAIWSTTFTModeConn) snapshot(turnIndex, eventIndex int) (written, started, emitted time.Time, reading bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	turn := &c.turns[turnIndex]
	return turn.writtenAt, turn.startedAt[eventIndex], turn.emittedAt[eventIndex], c.reading
}

type openAIWSTTFTModeDialer struct{ conn *openAIWSTTFTModeConn }

func (d *openAIWSTTFTModeDialer) Dial(context.Context, string, http.Header, string) (openAIWSClientConn, int, http.Header, error) {
	return d.conn, http.StatusSwitchingProtocols, http.Header{}, nil
}

// net.Pipe只用于起点负控的synctest bubble，真实WebSocket握手/帧/Proxy仍执行。
// 原semantic/visible两叶保持httptest loopback，不把真实网络放入虚拟时钟bubble。
type openAIWSTTFTModePipeWriter struct {
	header http.Header
	status int
	conn   net.Conn
}

func (w *openAIWSTTFTModePipeWriter) Header() http.Header         { return w.header }
func (w *openAIWSTTFTModePipeWriter) WriteHeader(status int)      { w.status = status }
func (w *openAIWSTTFTModePipeWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *openAIWSTTFTModePipeWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, bufio.NewReadWriter(bufio.NewReader(w.conn), bufio.NewWriter(w.conn)), nil
}

type openAIWSTTFTModePipeTransport struct {
	serve func(*coderws.Conn, *http.Request)
}

func (p openAIWSTTFTModePipeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	client, server := net.Pipe()
	w := &openAIWSTTFTModePipeWriter{header: make(http.Header), conn: server}
	conn, err := coderws.Accept(w, r, nil)
	if err != nil {
		_ = client.Close()
		_ = server.Close()
		return nil, err
	}
	go p.serve(conn, r.Clone(context.Background()))
	return &http.Response{StatusCode: w.status, Header: w.header, Body: client, Request: r}, nil
}

func TestOpenAIWSPassthroughTTFTOriginIncludesAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runOpenAIWSPassthroughTTFTModeSession(t, OpenAITTFTModeSemantic, true)
	})
}

func TestOpenAIWSTTFTModeConn(t *testing.T) {
	t.Run("waits_for_each_successful_create", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			turns := [][]openAIWSTTFTModeTestEvent{
				{{0, `{"type":"response.created","response":{"id":"resp_gate_1"}}`}},
				{{200 * time.Millisecond, `{"type":"response.created","response":{"id":"resp_gate_2"}}`}},
			}
			conn := newOpenAIWSTTFTModeConn(turns)
			defer func() { _ = conn.Close() }()
			for i := range turns {
				readCh := make(chan []byte, 1)
				go func() {
					message, _ := conn.ReadMessage(context.Background())
					readCh <- message
				}()
				synctest.Wait()
				written, started, emitted, reading := conn.snapshot(i, 0)
				require.True(t, written.IsZero() && started.IsZero() && emitted.IsZero(), "no event delay may start before the corresponding successful create")
				require.True(t, reading, "pre-read must already be waiting at its own gate")
				select {
				case <-readCh:
					t.Fatal("event returned before create")
				default:
				}
				require.NoError(t, conn.WriteFrame(context.Background(), coderws.MessageText, []byte(`{"type":"response.create"}`)))
				message := <-readCh
				require.Equal(t, turns[i][0].payload, string(message))
				written, started, emitted, _ = conn.snapshot(i, 0)
				require.False(t, started.Before(written))
				require.Equal(t, turns[i][0].delay, emitted.Sub(started), "only post-create time consumes the fixture delay")
			}
		})
	})
	for _, operation := range []string{"context_cancel", "close", "write_failure"} {
		t.Run(operation+"_releases_waiter", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				conn := newOpenAIWSTTFTModeConn([][]openAIWSTTFTModeTestEvent{{{0, `{"type":"response.created"}`}}})
				defer func() { _ = conn.Close() }()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				errCh := make(chan error, 1)
				go func() { _, err := conn.ReadMessage(ctx); errCh <- err }()
				synctest.Wait()
				_, started, _, reading := conn.snapshot(0, 0)
				require.True(t, reading)
				require.True(t, started.IsZero())
				switch operation {
				case "context_cancel":
					cancel()
				case "close":
					require.NoError(t, conn.Close())
				case "write_failure":
					require.NoError(t, conn.writes.Close())
					require.Error(t, conn.WriteFrame(context.Background(), coderws.MessageText, []byte(`{"type":"response.create"}`)))
				}
				synctest.Wait()
				require.Error(t, <-errCh)
				_, _, emitted, reading := conn.snapshot(0, 0)
				require.False(t, reading, "waiter must leave before cleanup completes")
				require.True(t, emitted.IsZero())
				require.NoError(t, conn.Close())
				require.NoError(t, conn.Close(), "repeated Close must not panic")
			})
		})
	}
}
