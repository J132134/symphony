from __future__ import annotations

import json
from dataclasses import dataclass, field
from typing import Any


# JSON-RPC 2.0 message types for line-delimited JSON over stdio


@dataclass
class JsonRpcRequest:
    method: str
    id: str | int
    params: dict[str, Any] = field(default_factory=dict)
    jsonrpc: str = "2.0"

    def to_json(self) -> str:
        return json.dumps(
            {
                "jsonrpc": self.jsonrpc,
                "method": self.method,
                "id": self.id,
                "params": self.params,
            }
        )


@dataclass
class JsonRpcResponse:
    id: str | int
    result: dict[str, Any] = field(default_factory=dict)
    jsonrpc: str = "2.0"

    def to_json(self) -> str:
        return json.dumps(
            {"jsonrpc": self.jsonrpc, "id": self.id, "result": self.result}
        )


@dataclass
class JsonRpcError:
    id: str | int | None
    code: int
    message: str
    data: Any = None
    jsonrpc: str = "2.0"

    def to_json(self) -> str:
        err: dict[str, Any] = {"code": self.code, "message": self.message}
        if self.data is not None:
            err["data"] = self.data
        return json.dumps(
            {"jsonrpc": self.jsonrpc, "id": self.id, "error": err}
        )


@dataclass
class JsonRpcNotification:
    method: str
    params: dict[str, Any] = field(default_factory=dict)
    jsonrpc: str = "2.0"

    def to_json(self) -> str:
        return json.dumps(
            {
                "jsonrpc": self.jsonrpc,
                "method": self.method,
                "params": self.params,
            }
        )


class Methods:
    """JSON-RPC method constants for the Codex app-server protocol."""

    # Client -> Server
    INITIALIZE = "initialize"
    INITIALIZED = "initialized"
    THREAD_START = "thread/start"
    TURN_START = "turn/start"

    # Server -> Client (notifications)
    TURN_COMPLETED = "turn/completed"
    TURN_FAILED = "turn/failed"
    TURN_CANCELLED = "turn/cancelled"
    TOKEN_USAGE_UPDATED = "thread/tokenUsage/updated"
    RATE_LIMITS_UPDATED = "account/rateLimits/updated"

    # Server -> Client (requests requiring response)
    COMMAND_APPROVAL = "item/commandExecution/requestApproval"
    FILE_CHANGE_APPROVAL = "item/fileChange/requestApproval"
    USER_INPUT_REQUEST = "item/tool/requestUserInput"


MessageType = JsonRpcRequest | JsonRpcResponse | JsonRpcNotification | JsonRpcError


def parse_message(line: str) -> MessageType:
    """Parse a JSON-RPC message from a line of text.

    Returns the appropriate message type based on content:
    - Has 'method' and 'id' -> JsonRpcRequest (server request)
    - Has 'method' no 'id' -> JsonRpcNotification
    - Has 'result' -> JsonRpcResponse
    - Has 'error' -> JsonRpcError
    """
    data = json.loads(line)

    if "error" in data:
        err = data["error"]
        return JsonRpcError(
            id=data.get("id"),
            code=err["code"],
            message=err["message"],
            data=err.get("data"),
            jsonrpc=data.get("jsonrpc", "2.0"),
        )

    if "result" in data:
        return JsonRpcResponse(
            id=data["id"],
            result=data.get("result", {}),
            jsonrpc=data.get("jsonrpc", "2.0"),
        )

    method = data.get("method", "")
    if "id" in data:
        return JsonRpcRequest(
            method=method,
            id=data["id"],
            params=data.get("params", {}),
            jsonrpc=data.get("jsonrpc", "2.0"),
        )

    return JsonRpcNotification(
        method=method,
        params=data.get("params", {}),
        jsonrpc=data.get("jsonrpc", "2.0"),
    )


def format_message(msg: MessageType) -> str:
    """Format a message as a line of JSON (with newline)."""
    return msg.to_json() + "\n"
