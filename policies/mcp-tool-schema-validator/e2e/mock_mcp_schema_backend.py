# --------------------------------------------------------------------
# Copyright (c) 2026, WSO2 LLC. (https://www.wso2.com).
#
# WSO2 LLC. licenses this file to you under the Apache License,
# Version 2.0 (the "License"); you may not use this file except
# in compliance with the License.
# You may obtain a copy of the License at
#
# http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing,
# software distributed under the License is distributed on an
# "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
# KIND, either express or implied.  See the License for the
# specific language governing permissions and limitations
# under the License.
# --------------------------------------------------------------------
"""Scripted MCP server for the mcp-tool-schema-validator e2e feature.

A tools/call answers according to the tool's name, so each scenario can make the
server return exactly the shape it needs: a valid or invalid structuredContent, as
JSON or SSE, a tool error, a JSON-RPC error, a mismatched id, an HTTP 500, and so on.
Text after "__" in a name is ignored, so one behaviour can back several tool rules.

The server also counts what reaches it, which is how the feature proves a rejected
call never got past the gateway:

  GET  /calls          {"total": <every JSON-RPC POST>}
  GET  /calls/<tool>   {"tool": <tool>, "count": <tools/call for that name>}
  POST /reset          clears the counters
"""

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

VALID = {"sku": "A-100", "qty": 3}
# qty has the wrong type and token is not allowed. The token value must never
# appear in what the gateway sends the client.
INVALID = {"sku": "A-100", "qty": "three", "token": "sk-live-SECRET-9999"}


def nested(depth):
    value = 1
    for _ in range(depth):
        value = [value]
    return value


def compact(payload):
    return json.dumps(payload, separators=(",", ":"))


def result(request_id, structured, text="A-100 x3"):
    return {
        "jsonrpc": "2.0",
        "id": request_id,
        "result": {"content": [{"type": "text", "text": text}], "structuredContent": structured},
    }


PROGRESS = {"jsonrpc": "2.0", "method": "notifications/progress", "params": {"progressToken": "p-1", "progress": 1}}

counts_lock = threading.Lock()
counts = {"total": 0, "tools": {}}


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def send_raw(self, body, content_type="application/json", status=200, headers=None):
        if isinstance(body, str):
            body = body.encode()
        self.send_response(status)
        if content_type:
            self.send_header("Content-Type", content_type)
        for name, value in (headers or {}).items():
            self.send_header(name, value)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def send_json(self, payload, status=200, headers=None):
        self.send_raw(compact(payload), status=status, headers=headers)

    def send_sse(self, message):
        """Streamable HTTP framing: an unrelated progress notification, then the answer."""
        body = (
            "event: message\n"
            "data: " + compact(PROGRESS) + "\n\n"
            "event: message\n"
            "id: evt-2\n"
            "data: " + (message if isinstance(message, str) else compact(message)) + "\n\n"
        )
        self.send_raw(body, content_type="text/event-stream")

    def do_GET(self):
        if self.path == "/health":
            self.send_json({"status": "ready"})
            return
        if self.path == "/calls":
            with counts_lock:
                self.send_json({"total": counts["total"]})
            return
        if self.path.startswith("/calls/"):
            tool = self.path[len("/calls/"):]
            with counts_lock:
                self.send_json({"tool": tool, "count": counts["tools"].get(tool, 0)})
            return
        self.send_json({"error": "not found"}, 404)

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        raw = self.rfile.read(length)

        if self.path == "/reset":
            with counts_lock:
                counts["total"] = 0
                counts["tools"] = {}
            self.send_json({"ok": True})
            return

        with counts_lock:
            counts["total"] += 1

        try:
            request = json.loads(raw)
        except Exception:
            self.send_json({"jsonrpc": "2.0", "id": None, "error": {"code": -32700, "message": "Parse error"}}, 400)
            return
        if not isinstance(request, dict):
            self.send_json({"jsonrpc": "2.0", "id": None, "error": {"code": -32600, "message": "Invalid Request"}}, 400)
            return

        method = request.get("method")
        has_id = "id" in request
        request_id = request.get("id")

        if method == "initialize":
            self.send_json({
                "jsonrpc": "2.0",
                "id": request_id,
                "result": {
                    "protocolVersion": "2025-06-18",
                    "capabilities": {"tools": {"listChanged": False}},
                    "serverInfo": {"name": "mcp-tool-schema-e2e-backend", "version": "1.0.0"},
                },
            }, headers={"mcp-session-id": "schema-e2e-session"})
            return

        if not has_id:
            # Notifications, tools/call ones included, get no JSON-RPC response.
            if method == "tools/call":
                self.count_tool((request.get("params") or {}).get("name"))
            self.send_raw(b"", content_type=None, status=202)
            return

        if method == "tools/list":
            self.send_json({"jsonrpc": "2.0", "id": request_id, "result": {"tools": [
                {"name": "lookup", "description": "Looks up a SKU.", "inputSchema": {"type": "object"}},
            ]}})
            return

        if method == "tools/call":
            name = (request.get("params") or {}).get("name")
            self.count_tool(name)
            self.answer_tool_call(str(name or "").split("__")[0], request_id)
            return

        self.send_json({"jsonrpc": "2.0", "id": request_id, "error": {"code": -32601, "message": "Method not found"}})

    def count_tool(self, name):
        if not isinstance(name, str):
            return
        with counts_lock:
            counts["tools"][name] = counts["tools"].get(name, 0) + 1

    def answer_tool_call(self, behaviour, request_id):
        if behaviour == "result_valid":
            self.send_json(result(request_id, VALID))
        elif behaviour == "result_invalid":
            self.send_json(result(request_id, INVALID))
        elif behaviour == "result_valid_sse":
            self.send_sse(result(request_id, VALID))
        elif behaviour == "result_invalid_sse":
            self.send_sse(result(request_id, INVALID))
        elif behaviour == "result_tool_error":
            self.send_json({"jsonrpc": "2.0", "id": request_id, "result": {
                "content": [{"type": "text", "text": "inventory service offline"}], "isError": True}})
        elif behaviour == "result_rpc_error":
            self.send_json({"jsonrpc": "2.0", "id": request_id,
                            "error": {"code": -32000, "message": "Upstream tool crashed"}})
        elif behaviour == "result_no_structured":
            self.send_json({"jsonrpc": "2.0", "id": request_id, "result": {
                "content": [{"type": "text", "text": "A-100 x3"}]}})
        elif behaviour == "result_null_structured":
            self.send_json(result(request_id, None))
        elif behaviour == "result_not_object":
            self.send_json({"jsonrpc": "2.0", "id": request_id, "result": "done"})
        elif behaviour == "result_too_deep":
            self.send_json(result(request_id, {"sku": "A-100", "qty": 3, "extra": nested(70)}))
        elif behaviour == "result_ambiguous":
            # A client keeping the last member would read the invalid copy.
            self.send_raw(
                '{"jsonrpc":"2.0","id":' + compact(request_id) + ',"result":{"content":[],'
                '"structuredContent":' + compact(VALID) + ',"structuredContent":' + compact(INVALID) + '}}')
        elif behaviour == "result_other_id":
            self.send_json(result("not-your-call", INVALID))
        elif behaviour == "result_string_id":
            # Echoes a numeric id as a string: per JSON-RPC that is a different id.
            self.send_json(result(str(request_id), INVALID))
        elif behaviour == "result_http_500":
            self.send_json(result(request_id, INVALID), status=500)
        else:
            self.send_json({"jsonrpc": "2.0", "id": request_id, "result": {
                "content": [{"type": "text", "text": "ok"}]}})

    def log_message(self, fmt, *args):
        print("%s - %s" % (self.address_string(), fmt % args), flush=True)


ThreadingHTTPServer(("0.0.0.0", 3001), Handler).serve_forever()
