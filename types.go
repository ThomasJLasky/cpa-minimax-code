package main

import "time"

// providerKey 同时是凭证文件里 type 字段的取值，必须与文件名前缀一致。
const providerKey = "minimax-code"

// RPC 方法名（与宿主 sdk/pluginabi 保持一致）。
const (
	methodPluginRegister    = "plugin.register"
	methodPluginReconfigure = "plugin.reconfigure"

	methodModelRegister = "model.register"
	methodModelStatic   = "model.static"
	methodModelForAuth  = "model.for_auth"

	methodAuthIdentifier = "auth.identifier"
	methodAuthParse      = "auth.parse"
	methodAuthLoginStart = "auth.login.start"
	methodAuthLoginPoll  = "auth.login.poll"
	methodAuthRefresh    = "auth.refresh"

	methodExecutorIdentifier    = "executor.identifier"
	methodExecutorExecute       = "executor.execute"
	methodExecutorExecuteStream = "executor.execute_stream"
	methodExecutorCountTokens   = "executor.count_tokens"

	methodManagementRegister = "management.register"
	methodManagementHandle   = "management.handle"

	methodQuotaIdentifier = "quota.identifier"
	methodQuotaDescribe   = "quota.describe"
	methodQuotaFetch      = "quota.fetch"
	methodQuotaReset      = "quota.reset"

	methodHostStreamEmit  = "host.stream.emit"
	methodHostStreamClose = "host.stream.close"
	methodHostLog         = "host.log"
)

// ---------------------------------------------------------------- 注册

type registration struct {
	SchemaVersion uint32           `json:"schema_version"`
	Metadata      metadata         `json:"metadata"`
	Capabilities  registrationCaps `json:"capabilities"`
}

type metadata struct {
	Name             string        `json:"Name"`
	Version          string        `json:"Version"`
	Author           string        `json:"Author"`
	GitHubRepository string        `json:"GitHubRepository"`
	Logo             string        `json:"Logo"`
	ConfigFields     []configField `json:"ConfigFields"`
}

type configField struct {
	Name        string   `json:"Name"`
	Type        string   `json:"Type"`
	EnumValues  []string `json:"EnumValues,omitempty"`
	Description string   `json:"Description"`
}

type registrationCaps struct {
	ModelProvider         bool     `json:"model_provider"`
	AuthProvider          bool     `json:"auth_provider"`
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string `json:"executor_output_formats,omitempty"`
	ManagementAPI         bool     `json:"management_api"`
	QuotaProvider         bool     `json:"quota_provider"`
}

// ---------------------------------------------------------------- 模型

type modelInfo struct {
	ID                         string   `json:"ID"`
	Object                     string   `json:"Object,omitempty"`
	OwnedBy                    string   `json:"OwnedBy,omitempty"`
	Type                       string   `json:"Type,omitempty"`
	DisplayName                string   `json:"DisplayName,omitempty"`
	Name                       string   `json:"Name,omitempty"`
	Description                string   `json:"Description,omitempty"`
	ContextLength              int64    `json:"ContextLength,omitempty"`
	MaxCompletionTokens        int64    `json:"MaxCompletionTokens,omitempty"`
	InputTokenLimit            int64    `json:"InputTokenLimit,omitempty"`
	OutputTokenLimit           int64    `json:"OutputTokenLimit,omitempty"`
	SupportedGenerationMethods []string `json:"SupportedGenerationMethods,omitempty"`
	UserDefined                bool     `json:"UserDefined,omitempty"`
}

type modelResponse struct {
	Provider string      `json:"Provider"`
	Models   []modelInfo `json:"Models"`
}

type modelRegistrationResponse struct {
	Provider string      `json:"Provider"`
	Models   []modelInfo `json:"Models"`
}

// ---------------------------------------------------------------- 凭证

// storage 是插件自己持有、由宿主持久化的凭证内容。
type storage struct {
	Type         string `json:"type"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at"` // unix 秒
	UID          string `json:"uid,omitempty"`
	Region       string `json:"region,omitempty"`
	Label        string `json:"label,omitempty"`
}

type authData struct {
	Provider         string            `json:"Provider"`
	ID               string            `json:"ID"`
	FileName         string            `json:"FileName"`
	Label            string            `json:"Label"`
	Disabled         bool              `json:"Disabled"`
	StorageJSON      []byte            `json:"StorageJSON"`
	Metadata         map[string]any    `json:"Metadata,omitempty"`
	Attributes       map[string]string `json:"Attributes,omitempty"`
	NextRefreshAfter time.Time         `json:"NextRefreshAfter"`
}

type authParseRequest struct {
	Provider string `json:"Provider"`
	Path     string `json:"Path"`
	FileName string `json:"FileName"`
	RawJSON  []byte `json:"RawJSON"`
}

type authParseResponse struct {
	Handled bool       `json:"Handled"`
	Auth    authData   `json:"Auth"`
	Auths   []authData `json:"Auths,omitempty"`
}

type authLoginStartRequest struct {
	Provider string         `json:"Provider"`
	BaseURL  string         `json:"BaseURL"`
	Metadata map[string]any `json:"Metadata,omitempty"`
}

type authLoginStartResponse struct {
	Provider  string         `json:"Provider"`
	URL       string         `json:"URL"`
	State     string         `json:"State"`
	ExpiresAt time.Time      `json:"ExpiresAt"`
	Metadata  map[string]any `json:"Metadata,omitempty"`
}

type authLoginPollRequest struct {
	Provider string         `json:"Provider"`
	State    string         `json:"State"`
	Metadata map[string]any `json:"Metadata,omitempty"`
}

type authLoginPollResponse struct {
	Status  string     `json:"Status"`
	Message string     `json:"Message,omitempty"`
	Auth    authData   `json:"Auth"`
	Auths   []authData `json:"Auths,omitempty"`
}

type authRefreshRequest struct {
	AuthID       string            `json:"AuthID"`
	AuthProvider string            `json:"AuthProvider"`
	StorageJSON  []byte            `json:"StorageJSON"`
	Metadata     map[string]any    `json:"Metadata,omitempty"`
	Attributes   map[string]string `json:"Attributes,omitempty"`
}

type authRefreshResponse struct {
	Auth             authData  `json:"Auth"`
	NextRefreshAfter time.Time `json:"NextRefreshAfter"`
}

type authModelRequest struct {
	AuthID       string         `json:"AuthID"`
	AuthProvider string         `json:"AuthProvider"`
	StorageJSON  []byte         `json:"StorageJSON"`
	Metadata     map[string]any `json:"Metadata,omitempty"`
}

// ---------------------------------------------------------------- 执行

type executorRequest struct {
	AuthID          string            `json:"AuthID"`
	AuthProvider    string            `json:"AuthProvider"`
	Model           string            `json:"Model"`
	Format          string            `json:"Format"`
	Stream          bool              `json:"Stream"`
	Alt             string            `json:"Alt"`
	Headers         map[string][]string `json:"Headers,omitempty"`
	OriginalRequest []byte            `json:"OriginalRequest,omitempty"`
	SourceFormat    string            `json:"SourceFormat,omitempty"`
	Payload         []byte            `json:"Payload"`
	StorageJSON     []byte            `json:"StorageJSON,omitempty"`
	Metadata        map[string]any    `json:"Metadata,omitempty"`
	StreamID        string            `json:"stream_id,omitempty"`
	// HostCallbackID 标识本次调用的宿主回调上下文，host.log 带上它日志才能挂到请求 id 上。
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type executorResponse struct {
	Payload  []byte              `json:"Payload"`
	Headers  map[string][]string `json:"Headers,omitempty"`
	Metadata map[string]any      `json:"Metadata,omitempty"`
}

type streamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
	Error    string `json:"error,omitempty"`
}

type streamCloseRequest struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

type hostLogRequest struct {
	HostCallbackID string         `json:"host_callback_id,omitempty"`
	Level          string         `json:"level,omitempty"`
	Message        string         `json:"message,omitempty"`
	Fields         map[string]any `json:"fields,omitempty"`
}

// ---------------------------------------------------------------- 管理

type managementRegistrationResponse struct {
	Routes    []managementRoute `json:"routes,omitempty"`
	Resources []resourceRoute   `json:"resources,omitempty"`
}

type managementRoute struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Menu        string `json:"Menu,omitempty"`
	Description string `json:"Description,omitempty"`
}

type resourceRoute struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu,omitempty"`
	Description string `json:"Description,omitempty"`
}

type managementRequest struct {
	Method  string              `json:"Method"`
	Path    string              `json:"Path"`
	Headers map[string][]string `json:"Headers,omitempty"`
	Query   map[string][]string `json:"Query,omitempty"`
	Body    []byte              `json:"Body,omitempty"`
}

type managementResponse struct {
	StatusCode int                 `json:"StatusCode"`
	Headers    map[string][]string `json:"Headers,omitempty"`
	Body       []byte              `json:"Body,omitempty"`
}

type identifierResponse struct {
	Identifier string `json:"Identifier"`
}
