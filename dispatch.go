package main

import (
	"context"
	"encoding/json"
)

const pluginVersion = "0.5.4"

func handleMethod(method string, request []byte) ([]byte, error) {
	ctx := context.Background()

	switch method {
	case methodPluginRegister, methodPluginReconfigure:
		startCheckinScheduler()
		return okEnvelope(pluginRegistration())

	case methodModelRegister:
		return okEnvelope(modelRegistrationResponse{Provider: providerKey, Models: pluginModels()})
	case methodModelStatic, methodModelForAuth:
		return okEnvelope(modelResponse{Provider: providerKey, Models: pluginModels()})

	case methodAuthIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerKey})
	case methodAuthParse:
		result, errParse := handleAuthParse(request)
		if errParse != nil {
			return nil, errParse
		}
		return okEnvelope(result)
	case methodAuthLoginStart:
		result, errStart := handleAuthLoginStart(ctx, request)
		if errStart != nil {
			return nil, errStart
		}
		return okEnvelope(result)
	case methodAuthLoginPoll:
		result, errPoll := handleAuthLoginPoll(ctx, request)
		if errPoll != nil {
			return nil, errPoll
		}
		return okEnvelope(result)
	case methodAuthRefresh:
		result, errRefresh := handleAuthRefresh(ctx, request)
		if errRefresh != nil {
			return nil, errRefresh
		}
		return okEnvelope(result)

	case methodExecutorIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerKey})
	case methodExecutorExecute:
		result, errExecute := handleExecutorExecute(ctx, request)
		if errExecute != nil {
			return nil, errExecute
		}
		return okEnvelope(result)
	case methodExecutorExecuteStream:
		result, errStream := handleExecutorExecuteStream(ctx, request)
		if errStream != nil {
			return nil, errStream
		}
		return okEnvelope(result)
	case methodExecutorCountTokens:
		result, errCount := handleExecutorCountTokens(request)
		if errCount != nil {
			return nil, errCount
		}
		return okEnvelope(result)

	case methodQuotaIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerKey})
	case methodQuotaDescribe:
		result, errDescribe := handleQuotaDescribe()
		if errDescribe != nil {
			return nil, errDescribe
		}
		return okEnvelope(result)
	case methodQuotaFetch:
		result, errFetch := handleQuotaFetch(ctx, request)
		if errFetch != nil {
			return nil, errFetch
		}
		return okEnvelope(result)

	case methodManagementRegister:
		result, errRegister := handleManagementRegister()
		if errRegister != nil {
			return nil, errRegister
		}
		return okEnvelope(result)
	case methodManagementHandle:
		result, errHandle := handleManagement(ctx, request)
		if errHandle != nil {
			return nil, errHandle
		}
		return okEnvelope(result)

	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: schemaVersion,
		Metadata: metadata{
			Name:             "minimax-code",
			Version:          pluginVersion,
			Author:           "local",
			GitHubRepository: "https://github.com/router-for-me/CLIProxyAPI",
			Logo:             "",
			ConfigFields: []configField{
				{Name: "region", Type: "enum", EnumValues: []string{"cn", "global"}, Description: "MiniMax Code 站点：cn（agent.minimax.cn）或 global（agent.minimax.io）。"},
			},
		},
		Capabilities: registrationCaps{
			ModelProvider:         true,
			AuthProvider:          true,
			Executor:              true,
			ExecutorModelScope:    "both",
			ExecutorInputFormats:  []string{"anthropic"},
			ExecutorOutputFormats: []string{"anthropic"},
			ManagementAPI:         true,
			QuotaProvider:         true,
		},
	}
}

// 让编译器保留 json 包（envelope 里用到），同时避免误删。
var _ = json.Marshal
