//go:build cgo

package main

/*
#include <stdint.h>
#include <stdlib.h>
typedef struct { void* ptr; size_t len; } cliproxy_buffer;
typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);
typedef struct { uint32_t abi_version; void* host_ctx; cliproxy_host_call_fn call; cliproxy_host_free_fn free_buffer; } cliproxy_host_api;
typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);
typedef struct { uint32_t abi_version; cliproxy_plugin_call_fn call; cliproxy_plugin_free_fn free_buffer; cliproxy_plugin_shutdown_fn shutdown; } cliproxy_plugin_api;
extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
static const cliproxy_host_api* stored_host;
static void store_host_api(const cliproxy_host_api* host) { stored_host = host; }
static int call_host_api(const char* method, const uint8_t* request, size_t length, cliproxy_buffer* response) {
    if (!stored_host || !stored_host->call) return 1;
    return stored_host->call(stored_host->host_ctx, method, request, length, response);
}
static void free_host_buffer(void* ptr, size_t length) {
    if (stored_host && stored_host->free_buffer && ptr) stored_host->free_buffer(ptr, length);
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"unsafe"
)

// abiVersion is the native C ABI version (sdk/pluginabi.ABIVersion).
const abiVersion = 1

// maxABIRequestBytes bounds accepted request payloads (usage records and
// completions are small; this only guards against malformed lengths).
const maxABIRequestBytes = 16 << 20

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if host == nil || plugin == nil || host.abi_version != abiVersion {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = abiVersion
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, length C.size_t, response *C.cliproxy_buffer) (result C.int) {
	if response == nil {
		return 1
	}
	response.ptr = nil
	response.len = 0
	defer func() {
		if recover() != nil {
			writeEnvelope(response, encodeEnvelope(nil, fmt.Errorf("plugin internal error")))
			result = 0
		}
	}()
	if method == nil || length > maxABIRequestBytes || (request == nil && length > 0) {
		writeEnvelope(response, encodeEnvelope(nil, fmt.Errorf("invalid plugin request")))
		return 0
	}
	var body []byte
	if request != nil && length > 0 {
		body = C.GoBytes(unsafe.Pointer(request), C.int(length))
	}
	value, err := dispatch(C.GoString(method), body)
	writeEnvelope(response, encodeEnvelope(value, err))
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, length C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() { plug.shutdown() }

func writeEnvelope(response *C.cliproxy_buffer, raw []byte) {
	if len(raw) == 0 {
		return
	}
	response.ptr = C.CBytes(raw)
	response.len = C.size_t(len(raw))
}

// callHost invokes a host callback such as host.log.
func callHost(method string, payload any) (json.RawMessage, error) {
	request, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	cRequest := C.CBytes(request)
	defer C.free(cRequest)
	var response C.cliproxy_buffer
	code := C.call_host_api(cMethod, (*C.uint8_t)(cRequest), C.size_t(len(request)), &response)
	if response.ptr == nil {
		return nil, fmt.Errorf("host callback %s returned no response (code %d)", method, int(code))
	}
	defer C.free_host_buffer(response.ptr, response.len)
	raw := C.GoBytes(response.ptr, C.int(response.len))
	var output envelope
	if json.Unmarshal(raw, &output) != nil || !output.OK || code != 0 {
		return nil, fmt.Errorf("host callback %s failed (code %d)", method, int(code))
	}
	return output.Result, nil
}
