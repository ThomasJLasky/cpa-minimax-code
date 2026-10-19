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
var modelSpecs = []modelSpec{
	{ID: "mm-m3.1-flash", Upstream: "MiniMax-M3.1-Flash-Preview", Display: "MiniMax M3.1 Flash Preview", Context: 200000, MaxOut: 32768},
	{ID: "mm-m3", Upstream: "MiniMax-M3", Display: "MiniMax M3", Context: 200000, MaxOut: 32768},
	{ID: "mm-m2.7", Upstream: "MiniMax-M2.7", Display: "MiniMax M2.7", Context: 200000, MaxOut: 32768},
	{ID: "mm-m2.7-highspeed", Upstream: "MiniMax-M2.7-highspeed", Display: "MiniMax M2.7 Highspeed", Context: 200000, MaxOut: 32768},
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

// rewriteModel 把客户端 id 换成上游 model 名。mavis 只认 MiniMax-M*。
func rewriteModel(payload []byte, requested string) []byte {
	if len(payload) == 0 {
		return payload
	}
	var body map[string]any
	if errUnmarshal := json.Unmarshal(payload, &body); errUnmarshal != nil {
		return payload
	}
	model, _ := body["model"].(string)
	if model == "" {
		model = requested
	}
	body["model"] = upstreamModel(model)
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

