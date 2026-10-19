package main

import (
	"bytes"
	"encoding/json"
)

type modelSpec struct {
	ID       string
	Upstream string
	Display  string
	Context  int64
	MaxOut   int64
}

// 客户端 id 统一用 mm- 前缀，避开 workbuddy 已有的 minimax-m3 / minimax-m2.7 同名模型
// （宿主按 id 去重，priority 高的胜出，我们的 55 会被 100 完全遮住）。
//
// Context/MaxOut 取自 models.dev 目录（MiniMax Code 客户端自己读的那份，
// ~/.minimax/cache/models-dev-catalog.json），并且都低于 mavis 实测的
// max_tokens 硬上限（M3 家族 524288、M2.x 196608），所以声明值一定可用。
var modelSpecs = []modelSpec{
	{ID: "mm-m3.1-flash", Upstream: "MiniMax-M3.1-Flash", Display: "MiniMax M3.1 Flash", Context: 1000000, MaxOut: 512000},
	{ID: "mm-m3", Upstream: "MiniMax-M3", Display: "MiniMax M3", Context: 1000000, MaxOut: 512000},
	{ID: "mm-m2.7", Upstream: "MiniMax-M2.7", Display: "MiniMax M2.7", Context: 204800, MaxOut: 131072},
	{ID: "mm-m2.7-highspeed", Upstream: "MiniMax-M2.7-highspeed", Display: "MiniMax M2.7 Highspeed", Context: 204800, MaxOut: 131072},
}

func pluginModels() []modelInfo {
	out := make([]modelInfo, 0, len(modelSpecs))
	for _, spec := range modelSpecs {
		out = append(out, modelInfo{
			ID:                         spec.ID,
			Object:                     "model",
			OwnedBy:                    providerKey,
			Type:                       "chat",
			DisplayName:                spec.Display,
			Name:                       spec.Upstream,
			ContextLength:              spec.Context,
			MaxCompletionTokens:        spec.MaxOut,
			InputTokenLimit:            spec.Context,
			OutputTokenLimit:           spec.MaxOut,
			SupportedGenerationMethods: []string{"chat"},
			UserDefined:                true,
		})
	}
	return out
}

func upstreamModel(id string) string {
	for _, spec := range modelSpecs {
		if spec.ID == id {
			return spec.Upstream
		}
	}
	// 已经是上游名（或未知）就原样透传。
	return id
}

// upstreamForPayload 返回这次请求实际会发给 mavis 的模型名（与 rewriteModel 同一套规则）。
func upstreamForPayload(payload []byte, requested string) string {
	model := requested
	if len(payload) > 0 {
		var body struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(payload, &body) == nil && body.Model != "" {
			model = body.Model
		}
	}
	return upstreamModel(model)
}

// rewriteModel 把客户端 id 换成上游 model 名。mavis 只认 MiniMax-M*。
func rewriteModel(payload []byte, requested string) []byte {
	if len(payload) == 0 {
		return payload
	}
	var body map[string]any
	if errUnmarshal := json.Unmarshal(payload, &body); errUnmarshal != nil {
		return payload
	}
	body["model"] = upstreamForPayload(payload, requested)
	out, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		return payload
	}
	return out
}

// restoreModelJSON 把上游回显的 model 名改回客户端 id，避免界面上出现 MiniMax-M3。
func restoreModelJSON(body []byte, clientModel string) []byte {
	if clientModel == "" || len(body) == 0 {
		return body
	}
	var doc map[string]any
	if errUnmarshal := json.Unmarshal(body, &doc); errUnmarshal != nil {
		return body
	}
	if _, ok := doc["model"]; !ok {
		return body
	}
	doc["model"] = clientModel
	out, errMarshal := json.Marshal(doc)
	if errMarshal != nil {
		return body
	}
	return out
}

// restoreModelChunk 处理流式帧：只在整段匹配出现时替换，避免把 JSON 改坏。
func restoreModelChunk(chunk []byte, clientModel string) []byte {
	if clientModel == "" || len(chunk) == 0 {
		return chunk
	}
	upstream := upstreamModel(clientModel)
	if upstream == clientModel {
		return chunk
	}
	from := []byte(`"model":"` + upstream + `"`)
	if !bytes.Contains(chunk, from) {
		return chunk
	}
	to := []byte(`"model":"` + clientModel + `"`)
	return bytes.ReplaceAll(chunk, from, to)
}

