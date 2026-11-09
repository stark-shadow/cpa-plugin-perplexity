package main

/*
#include <stdint.h>
#include <stdlib.h>
typedef struct { void *ptr; size_t len; } buffer;
typedef int (*host_call)(void*, const char*, const uint8_t*, size_t, buffer*);
typedef void (*free_buffer)(void*, size_t);
typedef struct { uint32_t abi_version; void *host_ctx; host_call call; free_buffer free_buffer; } host_api;
typedef int (*plugin_call)(char*, uint8_t*, size_t, buffer*);
typedef void (*plugin_shutdown)(void);
typedef struct { uint32_t abi_version; plugin_call call; free_buffer free_buffer; plugin_shutdown shutdown; } plugin_api;
extern int perplexityCall(char*, uint8_t*, size_t, buffer*);
extern void perplexityFree(void*, size_t);
extern void perplexityShutdown(void);
static void initialize(plugin_api *out) {
 out->abi_version = 1;
 out->call = perplexityCall;
 out->free_buffer = perplexityFree;
 out->shutdown = perplexityShutdown;
}
static int invoke(host_call fn, void *ctx, const char *method, const uint8_t *request, size_t length, buffer *response) {
 return fn(ctx, method, request, length, response);
}
static void release(free_buffer fn, void *ptr, size_t len) { fn(ptr, len); }
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

func main() {}

type hostBridge struct {
	call C.host_call
	free C.free_buffer
	ctx  unsafe.Pointer
}

var bridgeMu sync.RWMutex
var bridge *hostBridge

func hostCall(ctx context.Context, method string, payload any) (json.RawMessage, error) {
	bridgeMu.RLock()
	handler := bridge
	bridgeMu.RUnlock()
	if handler == nil {
		return nil, errors.New("host callback unavailable")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, errors.New("host request encoding failed")
	}
	methodName := C.CString(method)
	defer C.free(unsafe.Pointer(methodName))
	request := C.CBytes(encoded)
	defer C.free(request)
	var response C.buffer
	code := C.invoke(handler.call, handler.ctx, methodName, (*C.uint8_t)(request), C.size_t(len(encoded)), &response)
	if response.ptr != nil {
		defer C.release(handler.free, response.ptr, response.len)
	}
	if code != 0 || response.ptr == nil || response.len > 32<<20 {
		return nil, errors.New("host callback failed")
	}
	var envelope pluginabi.Envelope
	if json.Unmarshal(C.GoBytes(response.ptr, C.int(response.len)), &envelope) != nil {
		return nil, errors.New("host response decoding failed")
	}
	if !envelope.OK {
		if envelope.Error != nil {
			return nil, envelope.Error
		}
		return nil, errors.New("host callback failed")
	}
	return envelope.Result, nil
}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.host_api, out *C.plugin_api) (code C.int) {
	defer func() {
		if recover() != nil {
			code = 1
		}
	}()
	if host == nil || out == nil || uint32(host.abi_version) != pluginabi.ABIVersion || host.call == nil || host.free_buffer == nil || host.host_ctx == nil {
		return 1
	}
	bridgeMu.Lock()
	bridge = &hostBridge{call: host.call, free: host.free_buffer, ctx: host.host_ctx}
	bridgeMu.Unlock()
	C.initialize(out)
	return 0
}

func putEnvelope(out *C.buffer, encoded []byte) {
	out.ptr = C.CBytes(encoded)
	out.len = C.size_t(len(encoded))
}

//export perplexityCall
func perplexityCall(method *C.char, data *C.uint8_t, length C.size_t, out *C.buffer) (code C.int) {
	if out == nil {
		return 1
	}
	out.ptr, out.len = nil, 0
	defer func() {
		if recover() != nil {
			encoded, _ := pluginabi.NewErrorEnvelope("internal_error", "Perplexity plugin internal error", 500)
			putEnvelope(out, encoded)
			code = 0
		}
	}()
	if method == nil || length > maxRPCPayload || (length > 0 && data == nil) {
		encoded, _ := pluginabi.NewErrorEnvelope("invalid_request", "Invalid RPC input (maximum 1 MiB)", 400)
		putEnvelope(out, encoded)
		return 0
	}
	putEnvelope(out, rpcEnvelope(C.GoString(method), C.GoBytes(unsafe.Pointer(data), C.int(length)), runtime.dispatch, hostCall))
	return 0
}

//export perplexityFree
func perplexityFree(pointer unsafe.Pointer, length C.size_t) { C.free(pointer) }

//export perplexityShutdown
func perplexityShutdown() {
	defer func() { _ = recover() }()
	runtime.stop()
	bridgeMu.Lock()
	bridge = nil
	bridgeMu.Unlock()
}
