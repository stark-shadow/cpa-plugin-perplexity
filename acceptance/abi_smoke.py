#!/usr/bin/env python3
"""Exercise the real C ABI with a hermetic in-process host; no sockets."""

import base64
import ctypes
import json
import pathlib
import sys


class Buffer(ctypes.Structure):
    _fields_ = [("ptr", ctypes.c_void_p), ("len", ctypes.c_size_t)]


HostCall = ctypes.CFUNCTYPE(
    ctypes.c_int, ctypes.c_void_p, ctypes.c_char_p, ctypes.c_void_p,
    ctypes.c_size_t, ctypes.POINTER(Buffer),
)
Free = ctypes.CFUNCTYPE(None, ctypes.c_void_p, ctypes.c_size_t)
PluginCall = ctypes.CFUNCTYPE(
    ctypes.c_int, ctypes.c_char_p, ctypes.c_void_p, ctypes.c_size_t, ctypes.POINTER(Buffer),
)
Shutdown = ctypes.CFUNCTYPE(None)


class HostAPI(ctypes.Structure):
    _fields_ = [("abi_version", ctypes.c_uint32), ("host_ctx", ctypes.c_void_p),
                ("call", HostCall), ("free_buffer", Free)]


class PluginAPI(ctypes.Structure):
    _fields_ = [("abi_version", ctypes.c_uint32), ("call", PluginCall),
                ("free_buffer", Free), ("shutdown", Shutdown)]


def main() -> None:
    if len(sys.argv) != 2:
        raise SystemExit("usage: abi_smoke.py <perplexity.so>")
    library = ctypes.CDLL(str(pathlib.Path(sys.argv[1]).resolve()))
    libc = ctypes.CDLL(None)
    libc.malloc.argtypes = [ctypes.c_size_t]
    libc.malloc.restype = ctypes.c_void_p
    libc.free.argtypes = [ctypes.c_void_p]
    frees = []
    callbacks = []
    saved = {}

    @HostCall
    def host_call(_ctx, method, request, length, out):
        method = method.decode()
        callbacks.append(method)
        result = {}
        if method == "host.http.operation_open":
            result = {"operation_id": "smoke-operation"}
        elif method == "host.http.do":
            wire = json.loads(ctypes.string_at(request, length))
            url = wire["request"]["URL"]
            if url.endswith("/api/auth/session"):
                body = json.dumps({"user": {"id": "smoke-account", "email": "smoke@example.invalid", "subscription_tier": "pro"}}).encode()
            else:
                assert "/rest/models/config/v2?version=2.18&source=default" in url
                body = json.dumps({"models": {"smoke_model": {"label": "Smoke", "mode": "search"}}}).encode()
            result = {"StatusCode": 200, "Body": base64.b64encode(body).decode()}
        elif method == "host.auth.save":
            wire = json.loads(ctypes.string_at(request, length))
            saved[wire["name"]] = wire["json"]
            result = {"name": wire["name"]}
        elif method != "host.http.cancel":
            raise AssertionError("unexpected host RPC")
        encoded = json.dumps({"ok": True, "result": result}).encode()
        pointer = libc.malloc(len(encoded))
        ctypes.memmove(pointer, encoded, len(encoded))
        out.contents.ptr, out.contents.len = pointer, len(encoded)
        return 0

    @Free
    def host_free(pointer, length):
        frees.append(length)
        libc.free(pointer)

    initialize = library.cliproxy_plugin_init
    initialize.argtypes = [ctypes.POINTER(HostAPI), ctypes.POINTER(PluginAPI)]
    initialize.restype = ctypes.c_int
    for symbol in ("perplexityCall", "perplexityFree", "perplexityShutdown"):
        assert getattr(library, symbol)
    api = PluginAPI()
    valid = HostAPI(1, 1, host_call, host_free)
    assert initialize(None, ctypes.byref(api)) != 0
    assert initialize(ctypes.byref(valid), None) != 0
    for invalid in (HostAPI(2, 1, host_call, host_free), HostAPI(1, 1, HostCall(), host_free),
                    HostAPI(1, 1, host_call, Free()), HostAPI(1, None, host_call, host_free)):
        assert initialize(ctypes.byref(invalid), ctypes.byref(api)) != 0
    assert initialize(ctypes.byref(valid), ctypes.byref(api)) == 0
    assert api.abi_version == 1 and api.call and api.free_buffer and api.shutdown

    def invoke(method, payload=b"{}", length=None):
        out = Buffer()
        data = ctypes.create_string_buffer(payload) if payload is not None else None
        encoded_method = method.encode() if method is not None else None
        size = len(payload or b"") if length is None else length
        assert api.call(encoded_method, data, size, ctypes.byref(out)) == 0
        try:
            return json.loads(ctypes.string_at(out.ptr, out.len))
        finally:
            api.free_buffer(out.ptr, out.len)

    for method, payload, length in ((None, b"", 0), ("plugin.register", None, 1),
                                     ("plugin.register", None, (1 << 20) + 1)):
        response = invoke(method, payload, length)
        assert not response["ok"] and response["error"]["http_status"] == 400
    response = invoke("plugin.register", b"{}" + b" " * ((1 << 20) - 2))
    assert response["ok"] and response["result"]["schema_version"] == 10
    assert response["result"]["metadata"]["Author"] == "lzt404"
    for yaml in ("unknown_field: true", "language: en\n---\n{}", "{}\n---\n"):
        request = json.dumps({"config_yaml": base64.b64encode(yaml.encode()).decode()}).encode()
        response = invoke("plugin.register", request)
        assert not response["ok"] and response["error"]["code"] == "invalid_config"
    response = invoke("model.static")
    assert response["ok"] and len(response["result"]["Models"]) == 5
    assert len(frees) == len(callbacks) == 3
    response = invoke("auth.login.start")
    assert not response["ok"] and response["error"]["http_status"] == 400
    response = invoke("auth.login.start", json.dumps({"BaseURL": "http://127.0.0.1:8317/v0/management/oauth/callback"}).encode())
    assert response["ok"]
    state = response["result"]["State"]
    assert len(state) <= 128
    assert response["result"]["URL"] == "http://127.0.0.1:8317/v0/resource/plugins/perplexity/login?state=" + state
    response = invoke("auth.login.poll", json.dumps({"State": state}).encode())
    assert response["ok"] and response["result"]["Status"] == "pending"
    token = "hermetic-smoke-session-000000000000000000"
    body = json.dumps({"session_token": token, "state": state}).encode()
    response = invoke("management.handle", json.dumps({"Method": "POST", "Path": "/v0/management/plugins/perplexity/auth/import", "Body": base64.b64encode(body).decode()}).encode())
    assert response["ok"] and response["result"]["StatusCode"] == 201
    assert token not in base64.b64decode(response["result"]["Body"]).decode()
    response = invoke("auth.login.poll", json.dumps({"State": state}).encode())
    assert response["ok"] and response["result"]["Status"] == "success"
    assert response["result"]["Auth"]["Label"] == "smoke@example.invalid" and len(saved) == 1
    response = invoke("unsupported")
    assert not response["ok"] and response["error"]["code"] == "method_not_found"
    api.shutdown()
    api.shutdown()
    print("PASS: C ABI exports, init validation, 1 MiB boundary, strict registration, host bridge/free, model catalog, login pending/import/success, shutdown (hermetic)")


if __name__ == "__main__":
    main()
