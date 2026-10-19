package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

const streamTimeout = 30 * time.Minute

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
		return nil, fmt.Errorf("minimax credential expired at %s; waiting for host refresh", time.Unix(st.ExpiresAt, 0).Format(time.RFC3339))
	}

	callCtx, cancel := context.WithTimeout(ctx, requestTimeout())
	defer cancel()

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
		return nil, upstreamError(resp.StatusCode, string(body))
	}
	return executorResponse{
		Payload: restoreModelJSON(body, req.Model),
		Headers: map[string][]string{"Content-Type": {"application/json"}},
	}, nil
}

// handleExecutorExecuteStream 立即返回，真实数据通过 host.stream.emit 推给宿主。
// claude 格式下宿主会把 chunk 原样写给客户端，所以这里必须发完整的 SSE 帧
// （event: ... / data: ... / 空行），不能只发裸 JSON。
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
		return nil, fmt.Errorf("minimax credential expired at %s; waiting for host refresh", time.Unix(st.ExpiresAt, 0).Format(time.RFC3339))
	}
	if req.StreamID == "" {
		return nil, fmt.Errorf("stream_id is required for streaming execution")
	}

	streamID := req.StreamID
	payload := rewriteModel(req.Payload, req.Model)
	region := st.Region
	token := st.AccessToken

	go streamUpstream(streamID, region, token, payload, req.Model)

	return struct {
		Headers map[string][]string `json:"headers,omitempty"`
	}{Headers: map[string][]string{"Content-Type": {"text/event-stream"}}}, nil
}

func streamUpstream(streamID, region, token string, payload []byte, clientModel string) {
	ctx, cancel := context.WithTimeout(context.Background(), streamTimeout)
	defer cancel()

	fail := func(message string) {
		_, _ = hostCall(methodHostStreamEmit, streamEmitRequest{StreamID: streamID, Error: message})
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
		frame := upstreamErrorFrame(resp.StatusCode, string(body))
		_, _ = hostCall(methodHostStreamEmit, streamEmitRequest{StreamID: streamID, Payload: frame})
		_, _ = hostCall(methodHostStreamClose, streamCloseRequest{StreamID: streamID})
		return
	}

	buf := make([]byte, 16<<10)
	for {
		n, errRead := resp.Body.Read(buf)
		if n > 0 {
			chunk := restoreModelChunk(append([]byte(nil), buf[:n]...), clientModel)
			if _, errEmit := hostCall(methodHostStreamEmit, streamEmitRequest{StreamID: streamID, Payload: chunk}); errEmit != nil {
				// 宿主侧已关闭（客户端断开），停止读取。
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
