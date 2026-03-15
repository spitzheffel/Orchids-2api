package orchids

import (
	"context"
	"errors"
	"fmt"
	"github.com/goccy/go-json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"orchids-api/internal/debug"
	"orchids-api/internal/upstream"
	"orchids-api/internal/util"
)

var orchidsAgentModelMap = map[string]string{
	"claude-sonnet-4-5":          "claude-sonnet-4-6",
	"claude-sonnet-4-6":          "claude-sonnet-4-6",
	"claude-sonnet-4-5-thinking": "claude-sonnet-4-5-thinking",
	"claude-sonnet-4-6-thinking": "claude-sonnet-4-6",
	"claude-opus-4-6":            "claude-opus-4-6",
	"claude-opus-4-5":            "claude-opus-4-6",
	"claude-opus-4-5-thinking":   "claude-opus-4-5-thinking",
	"claude-opus-4-6-thinking":   "claude-opus-4-6",
	"claude-haiku-4-5":           "claude-haiku-4-5",
	"claude-sonnet-4-20250514":   "claude-sonnet-4-20250514",
	"claude-3-7-sonnet-20250219": "claude-3-7-sonnet-20250219",
	"gemini-3-flash":             "gemini-3-flash",
	"gemini-3-pro":               "gemini-3-pro",
	"gpt-5.3-codex":              "gpt-5.3-codex",
	"gpt-5.2-codex":              "gpt-5.2-codex",
	"gpt-5.2":                    "gpt-5.2",
	"grok-4.1-fast":              "grok-4.1-fast",
	"glm-5":                      "glm-5",
	"kimi-k2.5":                  "kimi-k2.5",
}

const orchidsAgentDefaultModel = "claude-sonnet-4-6"

// Orchids Event Types
const (
	EventConnected          = "connected"
	EventCodingAgentStart   = "coding_agent.start"
	EventCodingAgentInit    = "coding_agent.initializing"
	EventCodingAgentTokens  = "coding_agent.tokens_used"
	EventCreditsExhausted   = "coding_agent.credits_exhausted"
	EventResponseDone       = "response_done"
	EventCodingAgentEnd     = "coding_agent.end"
	EventComplete           = "complete"
	EventFS                 = "fs_operation"
	EventTodoWriteStart     = "coding_agent.todo_write.started"
	EventRunItemStream      = "run_item_stream_event"
	EventToolCallOutput     = "tool_call_output_item"
	EventEditStart          = "coding_agent.Edit.edit.started"
	EventEditChunk          = "coding_agent.Edit.edit.chunk"
	EventEditFileCompleted  = "coding_agent.edit_file.completed"
	EventEditCompleted      = "coding_agent.Edit.edit.completed"
	EventWriteStart         = "coding_agent.Write.started"
	EventWriteContentStart  = "coding_agent.Write.content.started"
	EventWriteChunk         = "coding_agent.Write.content.chunk"
	EventWriteCompleted     = "coding_agent.Write.content.completed"
	EventReasoningChunk     = "coding_agent.reasoning.chunk"
	EventReasoningCompleted = "coding_agent.reasoning.completed"
	EventOutputTextDelta    = "output_text_delta"
	EventResponseChunk      = "coding_agent.response.chunk"
	EventModel              = "model"
	EventResponseStarted    = "response_started"
)

func (c *Client) sendRequestWS(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	slog.Debug("sendRequestWS called", "trace_id", traceIDForLog(req), "attempt", attemptForLog(req), "workdir", req.Workdir, "model", req.Model)
	parentCtx := ctx
	timeout := orchidsWSRequestTimeout
	if c.config != nil && c.config.RequestTimeout > 0 {
		timeout = time.Duration(c.config.RequestTimeout) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	startPool := time.Now()

	proxyFunc := http.ProxyFromEnvironment
	if c.config != nil {
		proxyFunc = util.ProxyFunc(c.config.ProxyHTTP, c.config.ProxyHTTPS, c.config.ProxyUser, c.config.ProxyPass, c.config.ProxyBypass)
	}

	// Get connection from pool (or create new if pool unavailable)
	var conn *websocket.Conn
	var err error
	var returnToPool bool
	var pingDone chan struct{}

	if c.wsPool != nil {
		conn, err = c.wsPool.Get(ctx)
		if err != nil {
			// Fall back to direct connection if pool fails
			token, err := c.getWSToken()
			if err != nil {
				return fmt.Errorf("failed to get ws token: %w", err)
			}
			wsURL := c.buildWSURL(token)
			if wsURL == "" {
				return errors.New("ws url not configured")
			}
			headers := http.Header{
				"User-Agent": []string{orchidsWSUserAgent},
				"Origin":     []string{orchidsWSOrigin},
			}
			dialer := websocket.Dialer{
				HandshakeTimeout: orchidsWSConnectTimeout,
				Proxy:            proxyFunc,
			}
			conn, _, err = dialer.DialContext(ctx, wsURL, headers)
			if err != nil {
				if parentCtx.Err() == nil {
					return wsFallbackError{err: fmt.Errorf("ws dial failed: %w", err)}
				}
				return fmt.Errorf("ws dial failed: %w", err)
			}
			defer conn.Close()
		} else {
			// Successfully got connection from pool
			// Return to pool when done (unless error occurs)
			returnToPool = true
			pingDone = make(chan struct{})
			defer func() {
				close(pingDone)
				if conn == nil {
					return
				}
				if returnToPool {
					c.wsPool.Put(conn)
				} else {
					conn.Close()
				}
			}()
		}
	} else {
		// No pool available, create connection directly
		token, err := c.getWSToken()
		if err != nil {
			return fmt.Errorf("failed to get ws token: %w", err)
		}
		wsURL := c.buildWSURL(token)
		if wsURL == "" {
			return errors.New("ws url not configured")
		}
		headers := http.Header{
			"User-Agent": []string{orchidsWSUserAgent},
			"Origin":     []string{orchidsWSOrigin},
		}
		dialer := websocket.Dialer{
			HandshakeTimeout: orchidsWSConnectTimeout,
			Proxy:            proxyFunc,
		}
		conn, _, err = dialer.DialContext(ctx, wsURL, headers)
		if err != nil {
			if parentCtx.Err() == nil {
				return wsFallbackError{err: fmt.Errorf("ws dial failed: %w", err)}
			}
			return fmt.Errorf("ws dial failed: %w", err)
		}
		defer conn.Close()
	}

	if c.config.DebugEnabled {
		slog.Info("[Performance] WS connection acquired", "duration", time.Since(startPool))
	}

	startWrite := time.Now()
	requestWritten := false

	prepared, err := c.buildWSRequest(req)
	if err != nil {
		returnToPool = false
		return err
	}

	// Note: Logger disabled for pooled connections
	// if logger != nil {
	// 	logger.LogUpstreamRequest(wsURL, logHeaders, wsPayload)
	// }

	// Lock to prevent race with ping loop which starts shortly after
	c.wsWriteMu.Lock()
	writeErr := conn.WriteJSON(prepared.Request)
	c.wsWriteMu.Unlock()

	if writeErr != nil {
		if parentCtx.Err() == nil {
			returnToPool = false
			slog.Warn("Orchids WS write failed before first message", "trace_id", traceIDForLog(req), "attempt", attemptForLog(req), "chat_session_id", prepared.Meta.ChatSessionID, "request_written", requestWritten, "error", writeErr)
			return wsFallbackError{err: fmt.Errorf("ws write failed: %w", writeErr)}
		}
		returnToPool = false
		return fmt.Errorf("ws write failed: %w", writeErr)
	}
	requestWritten = true
	slog.Info("Orchids WS request sent", "trace_id", traceIDForLog(req), "attempt", attemptForLog(req), "chat_session_id", prepared.Meta.ChatSessionID, "model", req.Model)

	if c.config.DebugEnabled {
		slog.Info("[Performance] WS WriteJSON completed", "duration", time.Since(startWrite))
	}

	startFirstToken := time.Now()
	firstReceived := false
	receivedAnyMessage := false

	state := newOrchidsRequestState(req)
	var fsWG sync.WaitGroup

	if state.stream {
		emitOrchidsMessageStart(&state, onMessage)
	}

	// Start Keep-Alive Ping Loop
	go func() {
		ticker := time.NewTicker(orchidsWSPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-pingDone:
				return
			case <-ticker.C:
				c.wsWriteMu.Lock()
				err := conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(10*time.Second))
				c.wsWriteMu.Unlock()
				if err != nil {
					return
				}
			}
		}
	}()

	ctxDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			if conn != nil {
				_ = conn.Close()
			}
		case <-ctxDone:
		}
	}()
	defer close(ctxDone)

	for {
		if ctx.Err() != nil {
			returnToPool = false
			return ctx.Err()
		}
		if err := conn.SetReadDeadline(time.Now().Add(orchidsWSReadTimeout)); err != nil {
			if ctx.Err() != nil {
				returnToPool = false
				return ctx.Err()
			}
			if parentCtx.Err() == nil && !receivedAnyMessage {
				returnToPool = false
				slog.Warn("Orchids WS fallback before first message", "trace_id", traceIDForLog(req), "attempt", attemptForLog(req), "chat_session_id", req.ChatSessionID, "request_written", requestWritten, "stage", "set_read_deadline", "error", err)
				return wsFallbackError{err: err}
			}
			returnToPool = false
			return err
		}

		_, data, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				returnToPool = false
				return ctx.Err()
			}
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				returnToPool = false
				break
			}
			if parentCtx.Err() == nil && !receivedAnyMessage {
				returnToPool = false
				slog.Warn("Orchids WS fallback before first message", "trace_id", traceIDForLog(req), "attempt", attemptForLog(req), "chat_session_id", req.ChatSessionID, "request_written", requestWritten, "stage", "read_message", "error", err)
				return wsFallbackError{err: err}
			}
			returnToPool = false
			break
		}

		handled, shouldBreak := c.handleOrchidsRawMessage(data, &state, onMessage, logger, req.Tools)
		if handled {
			receivedAnyMessage = true
			if !firstReceived {
				slog.Info("Orchids WS first upstream message received", "trace_id", traceIDForLog(req), "attempt", attemptForLog(req), "chat_session_id", req.ChatSessionID)
			}
			if !firstReceived && c.config.DebugEnabled {
				firstReceived = true
				slog.Info("[Performance] WS First response received (TTFT)", "duration", time.Since(startFirstToken))
			}
			firstReceived = true
			if shouldBreak {
				break
			}
			continue
		}

		var msg map[string]interface{}
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		receivedAnyMessage = true
		if !firstReceived {
			slog.Info("Orchids WS first upstream message received", "trace_id", traceIDForLog(req), "attempt", attemptForLog(req), "chat_session_id", req.ChatSessionID)
		}

		if !firstReceived && c.config.DebugEnabled {
			firstReceived = true
			slog.Info("[Performance] WS First response received (TTFT)", "duration", time.Since(startFirstToken))
		}
		firstReceived = true

		shouldBreak = c.handleOrchidsMessage(msg, data, &state, onMessage, logger, conn, &fsWG, req.Workdir, req.Tools)
		if shouldBreak {
			break
		}
	}

	if err := finalizeOrchidsTransport(ctx, "WS", &state, onMessage, &fsWG); err != nil {
		if state.errorMsg != "" {
			slog.Warn("Orchids WS stream ended with upstream error", "trace_id", traceIDForLog(req), "attempt", attemptForLog(req), "chat_session_id", req.ChatSessionID, "error", state.errorMsg)
		}
		if ctx.Err() != nil {
			returnToPool = false
		}
		return err
	}

	slog.Info("Orchids WS request completed", "trace_id", traceIDForLog(req), "attempt", attemptForLog(req), "chat_session_id", req.ChatSessionID, "saw_tool_call", state.sawToolCall, "response_started", state.responseStarted)
	return nil
}

func (c *Client) handleOrchidsMessage(
	msg map[string]interface{},
	rawData []byte,
	state *requestState,
	onMessage func(upstream.SSEMessage),
	logger *debug.Logger,
	conn *websocket.Conn,
	fsWG *sync.WaitGroup,
	workdir string,
	clientTools []interface{},
) bool {
	return dispatchOrchidsDecodedEvent(c, msg, rawData, state, onMessage, logger, conn, fsWG, workdir, clientTools)
}

func (c *Client) buildWSURL(token string) string {
	if c.config == nil {
		return ""
	}
	wsURL := c.config.OrchidsWSURL
	sep := "?"
	if strings.Contains(wsURL, "?") {
		sep = "&"
	}
	return fmt.Sprintf("%s%stoken=%s", wsURL, sep, urlEncode(token))
}

func (c *Client) buildWSRequest(req upstream.UpstreamRequest) (*orchidsPreparedRequest, error) {
	if c.config == nil {
		return nil, errors.New("server config unavailable")
	}
	prepared := buildOrchidsPreparedRequest(req, c.config)
	return &prepared, nil
}

func isSuggestionModeText(text string) bool {
	normalized := strings.ToLower(text)
	return strings.Contains(normalized, "suggestion mode")
}

func normalizeOrchidsAgentModel(model string) string {
	mapped := normalizeOrchidsModelKey(model)
	if mapped == "" {
		return orchidsAgentDefaultModel
	}
	if resolved, ok := orchidsAgentModelMap[mapped]; ok {
		return resolved
	}
	return orchidsAgentDefaultModel
}

func normalizeOrchidsModelKey(model string) string {
	normalized := strings.ToLower(strings.TrimSpace(model))
	if strings.HasPrefix(normalized, "claude-") {
		normalized = strings.ReplaceAll(normalized, "4.6", "4-6")
		normalized = strings.ReplaceAll(normalized, "4.5", "4-5")
	}
	return normalized
}
