package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const streamTimeout = 30 * time.Minute

// expiredCredentialError 本地判断凭据已过期时返回的错误。
//
// 必须走 upstreamError(401,…) 而不是普通 fmt.Errorf：宿主要么按文本里的
// "status 401" 判定 unauthorized 并先调 auth.refresh 再重试，要么把这个账号
// 判成 credential-scoped 失败直接摘出轮询池，剩下的就是 auth_unavailable 503。
func expiredCredentialError(st storage) error {
	return upstreamError(http.StatusUnauthorized,
		fmt.Sprintf("access token expired at %s, waiting for host refresh", time.Unix(st.ExpiresAt, 0).Format(time.RFC3339)))
}

func handleExecutorExecute(ctx context.Context, request []byte) (any, error) {
	var req executorRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	st, errStorage := decodeStorage(req.StorageJSON)
	if errStorage != nil {
		return nil, errStorage
	}
	if st.ExpiresAt > 0 && time.Now().Unix() > st.ExpiresAt-30 {
		return nil, expiredCredentialError(st)
	}

	callCtx, cancel := context.WithTimeout(ctx, requestTimeout())
	defer cancel()

	upstream := upstreamForPayload(req.Payload, req.Model)
	httpReq, errReq := newMessagesRequest(callCtx, st.Region, st.AccessToken, rewriteModel(req.Payload, req.Model))
	if errReq != nil {
		return nil, errReq
	}
	resp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		return nil, fmt.Errorf("minimax upstream unreachable: %w", errDo)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode >= 400 {
		hostLog(req.HostCallbackID, "warn", "minimax-code: upstream messages rejected", map[string]any{
			"status": resp.StatusCode,
			"model":  req.Model,
			"body":   truncate(string(body), 500),
		})
		return nil, upstreamError(resp.StatusCode, string(body))
	}
	if got := echoedModel(body); got != "" && got != upstream {
		hostLog(req.HostCallbackID, "error", "minimax-code: upstream switched model", map[string]any{
			"requested": upstream,
			"actual":    got,
			"model":     req.Model,
		})
		return nil, modelFallbackError(upstream, got)
	}
	return executorResponse{
		Payload: restoreModelJSON(body, req.Model),
		Headers: map[string][]string{"Content-Type": {"application/json"}},
	}, nil
}

// handleExecutorExecuteStream 立即返回，真实数据由 streamUpstream 通过
// host.stream.emit 逐行推给宿主（见那里的帧切分约定）。
func handleExecutorExecuteStream(ctx context.Context, request []byte) (any, error) {
	var req executorRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	st, errStorage := decodeStorage(req.StorageJSON)
	if errStorage != nil {
		return nil, errStorage
	}
	if st.ExpiresAt > 0 && time.Now().Unix() > st.ExpiresAt-30 {
		return nil, expiredCredentialError(st)
	}
	if req.StreamID == "" {
		return nil, fmt.Errorf("stream_id is required for streaming execution")
	}

	streamID := req.StreamID
	callbackID := req.HostCallbackID
	upstream := upstreamForPayload(req.Payload, req.Model)
	payload := rewriteModel(req.Payload, req.Model)
	region := st.Region
	token := st.AccessToken

	go streamUpstream(streamID, callbackID, region, token, payload, req.Model, upstream)

	return struct {
		Headers map[string][]string `json:"headers,omitempty"`
	}{Headers: map[string][]string{"Content-Type": {"text/event-stream"}}}, nil
}

// streamUpstream 在独立 goroutine 里读上游并把 SSE 帧推给宿主。
//
// 关键约定：任何失败都必须用 stream.close 的错误形式收尾。宿主在收到 0 个 chunk
// 且通道关闭时会把它报成 "empty_stream: upstream stream closed before first payload"
// （sdk/cliproxy/auth/conductor_stream.go:437），真实原因（emit 失败 / 上游不可达）
// 就丢了，日志里只留下一条无法定位的 empty_stream。
func streamUpstream(streamID, callbackID, region, token string, payload []byte, clientModel, upstream string) {
	ctx, cancel := context.WithTimeout(context.Background(), streamTimeout)
	defer cancel()

	fail := func(message string) {
		hostLog(callbackID, "error", "minimax-code: stream failed", map[string]any{
			"stream_id": streamID,
			"model":     clientModel,
			"reason":    message,
		})
		if _, errEmit := hostCall(methodHostStreamEmit, streamEmitRequest{StreamID: streamID, Error: message}); errEmit != nil {
			hostLog(callbackID, "warn", "minimax-code: host.stream.emit failed", map[string]any{
				"stream_id": streamID,
				"error":     errEmit.Error(),
			})
		}
		_, _ = hostCall(methodHostStreamClose, streamCloseRequest{StreamID: streamID, Error: message})
	}

	httpReq, errReq := newMessagesRequest(ctx, region, token, payload)
	if errReq != nil {
		fail("build request: " + errReq.Error())
		return
	}
	resp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		fail("upstream unreachable: " + errDo.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		hostLog(callbackID, "warn", "minimax-code: upstream messages rejected", map[string]any{
			"stream_id": streamID,
			"status":    resp.StatusCode,
			"model":     clientModel,
			"body":      truncate(string(body), 500),
		})
		frame := upstreamErrorFrame(resp.StatusCode, string(body))
		if _, errEmit := hostCall(methodHostStreamEmit, streamEmitRequest{StreamID: streamID, Payload: frame}); errEmit != nil {
			hostLog(callbackID, "warn", "minimax-code: host.stream.emit failed", map[string]any{
				"stream_id": streamID,
				"error":     errEmit.Error(),
			})
		}
		_, _ = hostCall(methodHostStreamClose, streamCloseRequest{StreamID: streamID})
		return
	}

	buf := bufio.NewReaderSize(resp.Body, 64<<10)
	echoChecked := false
	for {
		// 一次一行，且保留结尾的 \n：宿主的 claude→openai/responses 流式转换器
		// 只认以 "data:" 开头的 chunk（internal/translator/claude/openai/*\/init.go 注册的
		// Stream 实现里先做 bytes.HasPrefix(rawJSON, "data:")，不匹配就返回空切片），
		// 整帧 "event: …\ndata: …\n\n" 会被全部丢弃，最后表现成无法定位的
		// "empty_stream: upstream stream closed before first payload"。
		// 逐行发与宿主内置的 claude_executor_stream 行为一致；claude→claude 直透时
		// 字节流原样重组，客户端看到的 SSE 不变。
		line, errRead := buf.ReadBytes('\n')
		if len(line) > 0 {
			if !echoChecked {
				if got := echoedModel(line); got != "" {
					echoChecked = true
					if got != upstream {
						hostLog(callbackID, "error", "minimax-code: upstream switched model", map[string]any{
							"requested": upstream,
							"actual":    got,
							"model":     clientModel,
						})
						frame := upstreamErrorFrame(http.StatusBadRequest, modelFallbackMessage(upstream, got))
						_, _ = hostCall(methodHostStreamEmit, streamEmitRequest{StreamID: streamID, Payload: frame})
						_, _ = hostCall(methodHostStreamClose, streamCloseRequest{StreamID: streamID})
						return
					}
				}
			}
			chunk := restoreModelChunk(line, clientModel)
			if _, errEmit := hostCall(methodHostStreamEmit, streamEmitRequest{StreamID: streamID, Payload: chunk}); errEmit != nil {
				// 宿主侧已关闭（客户端断开或 bridge 报错），日志记一下再停。
				hostLog(callbackID, "warn", "minimax-code: host.stream.emit rejected chunk", map[string]any{
					"stream_id": streamID,
					"error":     errEmit.Error(),
					"bytes":     len(line),
				})
				return
			}
		}
		if errRead != nil {
			if errRead == io.EOF {
				_, _ = hostCall(methodHostStreamClose, streamCloseRequest{StreamID: streamID})
				return
			}
			fail("read upstream: " + errRead.Error())
			return
		}
	}
}

// handleExecutorCountTokens 用粗略估算，mavis 网关没有 count_tokens 端点。
func handleExecutorCountTokens(request []byte) (any, error) {
	var req executorRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	var body struct {
		System   any `json:"system"`
		Messages []struct {
			Content any `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(req.Payload, &body)
	total := 0
	if body.System != nil {
		total += estimateTokens(flatten(body.System))
	}
	for _, msg := range body.Messages {
		total += estimateTokens(flatten(msg.Content))
	}
	if total == 0 {
		total = 1
	}
	payload, _ := json.Marshal(map[string]any{"input_tokens": total})
	return executorResponse{Payload: payload, Headers: map[string][]string{"Content-Type": {"application/json"}}}, nil
}

func flatten(v any) string {
	switch typed := v.(type) {
	case string:
		return typed
	case []any:
		out := ""
		for _, item := range typed {
			out += flatten(item)
		}
		return out
	case map[string]any:
		if text, ok := typed["text"].(string); ok {
			return text
		}
		return ""
	default:
		return ""
	}
}

func estimateTokens(text string) int {
	if text == "" {
		return 0
	}
	// 中文按字、英文按 4 字符一个 token 粗算，取折中值。
	return len([]rune(text))/2 + 1
}
