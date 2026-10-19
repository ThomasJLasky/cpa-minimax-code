package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

// C 不能直接在 Go 里调用函数指针，这里包一层。
static int mm_host_call(cliproxy_host_api* api, const char* method, const uint8_t* req, size_t len, cliproxy_buffer* out) {
	if (api == NULL || api->call == NULL) {
		return -1;
	}
	return api->call(api->host_ctx, method, req, len, out);
}

static void mm_host_free(cliproxy_host_api* api, void* ptr, size_t len) {
	if (api == NULL || api->free_buffer == NULL || ptr == NULL) {
		return;
	}
	api->free_buffer(ptr, len);
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"sync"
	"unsafe"
)

// abiVersion 必须严格等于宿主支持的 ABI 版本（v8 为 1）。
const abiVersion = 1

// schemaVersion 对应 v8.0.20 的契约版本上限 6。
const schemaVersion = 6

var (
	hostAPI  *C.cliproxy_host_api
	hostMu   sync.RWMutex
	shutdown bool
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	hostMu.Lock()
	hostAPI = host
	hostMu.Unlock()

	plugin.abi_version = C.uint32_t(abiVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	hostMu.Lock()
	shutdown = true
	hostAPI = nil
	hostMu.Unlock()
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

// hostCall 通过 C ABI 回调宿主（host.stream.emit 等）。
func hostCall(method string, req any) (json.RawMessage, error) {
	hostMu.RLock()
	api := hostAPI
	hostMu.RUnlock()
	if api == nil {
		return nil, fmt.Errorf("host api unavailable")
	}
	raw, errMarshal := json.Marshal(req)
	if errMarshal != nil {
		return nil, errMarshal
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var payload *C.uint8_t
	if len(raw) > 0 {
		payload = (*C.uint8_t)(C.CBytes(raw))
		defer C.free(unsafe.Pointer(payload))
	}
	var buf C.cliproxy_buffer
	if rc := C.mm_host_call(api, cMethod, payload, C.size_t(len(raw)), &buf); rc != 0 {
		return nil, fmt.Errorf("host call %s failed with code %d", method, int(rc))
	}
	if buf.ptr == nil {
		return nil, nil
	}
	out := C.GoBytes(buf.ptr, C.int(buf.len))
	C.mm_host_free(api, buf.ptr, buf.len)

	var env envelope
	if errUnmarshal := json.Unmarshal(out, &env); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host response: %w", errUnmarshal)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("%s: %s", env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("host call %s returned an error", method)
	}
	return env.Result, nil
}

func okEnvelope(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}
