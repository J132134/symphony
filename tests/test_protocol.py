from __future__ import annotations

import json

from symphony.agent.protocol import (
    JsonRpcError,
    JsonRpcNotification,
    JsonRpcRequest,
    JsonRpcResponse,
    Methods,
    format_message,
    parse_message,
)


class TestJsonRpcSerialization:
    def test_request_to_json(self):
        req = JsonRpcRequest(method="initialize", id=1, params={"key": "val"})
        data = json.loads(req.to_json())
        assert data["jsonrpc"] == "2.0"
        assert data["method"] == "initialize"
        assert data["id"] == 1
        assert data["params"]["key"] == "val"

    def test_response_to_json(self):
        resp = JsonRpcResponse(id=1, result={"status": "ok"})
        data = json.loads(resp.to_json())
        assert data["id"] == 1
        assert data["result"]["status"] == "ok"

    def test_notification_to_json(self):
        notif = JsonRpcNotification(method="turn/completed", params={"turnId": "abc"})
        data = json.loads(notif.to_json())
        assert data["method"] == "turn/completed"
        assert "id" not in data

    def test_error_to_json(self):
        err = JsonRpcError(id=1, code=-32601, message="Method not found")
        data = json.loads(err.to_json())
        assert data["error"]["code"] == -32601
        assert data["error"]["message"] == "Method not found"

    def test_error_with_data(self):
        err = JsonRpcError(id=1, code=-1, message="err", data={"detail": "x"})
        data = json.loads(err.to_json())
        assert data["error"]["data"]["detail"] == "x"

    def test_error_without_data(self):
        err = JsonRpcError(id=1, code=-1, message="err")
        data = json.loads(err.to_json())
        assert "data" not in data["error"]


class TestParseMessage:
    def test_parse_response(self):
        line = json.dumps({"jsonrpc": "2.0", "id": 1, "result": {"ok": True}})
        msg = parse_message(line)
        assert isinstance(msg, JsonRpcResponse)
        assert msg.id == 1
        assert msg.result["ok"] is True

    def test_parse_error(self):
        line = json.dumps({
            "jsonrpc": "2.0", "id": 1,
            "error": {"code": -32600, "message": "Invalid"},
        })
        msg = parse_message(line)
        assert isinstance(msg, JsonRpcError)
        assert msg.code == -32600

    def test_parse_request(self):
        line = json.dumps({
            "jsonrpc": "2.0", "id": "req-1",
            "method": "item/commandExecution/requestApproval",
            "params": {"command": "ls"},
        })
        msg = parse_message(line)
        assert isinstance(msg, JsonRpcRequest)
        assert msg.method == Methods.COMMAND_APPROVAL
        assert msg.id == "req-1"

    def test_parse_notification(self):
        line = json.dumps({
            "jsonrpc": "2.0",
            "method": "turn/completed",
            "params": {},
        })
        msg = parse_message(line)
        assert isinstance(msg, JsonRpcNotification)
        assert msg.method == Methods.TURN_COMPLETED

    def test_error_takes_priority_over_result(self):
        line = json.dumps({
            "jsonrpc": "2.0", "id": 1,
            "error": {"code": -1, "message": "err"},
            "result": {"ok": True},
        })
        msg = parse_message(line)
        assert isinstance(msg, JsonRpcError)

    def test_parse_invalid_json_raises(self):
        import pytest
        with pytest.raises(json.JSONDecodeError):
            parse_message("not json")


class TestFormatMessage:
    def test_format_adds_newline(self):
        req = JsonRpcRequest(method="test", id=1)
        result = format_message(req)
        assert result.endswith("\n")
        assert not result.endswith("\n\n")

    def test_roundtrip(self):
        original = JsonRpcRequest(method="initialize", id=42, params={"a": 1})
        line = format_message(original).strip()
        parsed = parse_message(line)
        assert isinstance(parsed, JsonRpcRequest)
        assert parsed.method == "initialize"
        assert parsed.id == 42
        assert parsed.params["a"] == 1


class TestMethodConstants:
    def test_all_methods_defined(self):
        assert Methods.INITIALIZE == "initialize"
        assert Methods.INITIALIZED == "initialized"
        assert Methods.THREAD_START == "thread/start"
        assert Methods.TURN_START == "turn/start"
        assert Methods.TURN_COMPLETED == "turn/completed"
        assert Methods.TURN_FAILED == "turn/failed"
        assert Methods.TURN_CANCELLED == "turn/cancelled"
        assert Methods.TOKEN_USAGE_UPDATED == "thread/tokenUsage/updated"
        assert Methods.RATE_LIMITS_UPDATED == "account/rateLimits/updated"
        assert Methods.COMMAND_APPROVAL == "item/commandExecution/requestApproval"
        assert Methods.FILE_CHANGE_APPROVAL == "item/fileChange/requestApproval"
        assert Methods.USER_INPUT_REQUEST == "item/tool/requestUserInput"
