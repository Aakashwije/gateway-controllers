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

Feature: MCP Tool Schema Validator
    The gateway validates tools/call arguments against a per-tool input schema
    before the MCP server is called, and validates result.structuredContent
    against a per-tool output schema before the result reaches the client.

    Rejections are JSON-RPC error bodies, which the MCP Client steps treat as
    failures, so those calls use raw POST steps. The raw steps reuse the
    mcp-session-id the MCP Client captured from initialize.

    The first two scenarios run against the reference MCP server. The rest run
    against schema-mcp-backend (e2e/mock_mcp_schema_backend.py), a scripted server
    that answers each tool with a chosen result shape and counts the calls that
    reach it on 127.0.0.1:3021, so a scenario can prove a rejected call never left
    the gateway.

    Background:
        Given the gateway services are running
        # A plain GET, not "I wait for the endpoint": that step also waits for the controller
        # and policy engine snapshot versions to match, and deleting the only deployed proxy
        # leaves an empty snapshot the controller never delivers, so they never would.
        When I send a GET request to "http://127.0.0.1:3021/health"
        Then the response should be successful

    Scenario: Tool arguments are validated before they reach the MCP server
        Given I authenticate using basic auth as "admin"
        When I deploy this MCP configuration:
            """
            apiVersion: gateway.api-platform.wso2.com/v1
            kind: Mcp
            metadata:
              name: tool-schema-request-v1.0
            spec:
              displayName: Tool Schema Request
              version: v1.0
              context: /tool-schema-request
              specVersion: "2025-06-18"
              upstream:
                url: http://mcp-server-backend:3001/mcp
              policies:
                - name: mcp-tool-schema-validator
                  version: v0
                  params:
                    tools:
                      - name: echo
                        input:
                          enabled: true
                          showAssessment: true
                          schema: '{"type":"object","properties":{"message":{"type":"string","minLength":1}},"required":["message"],"additionalProperties":false}'
              tools: []
              resources: []
              prompts: []
            """
        Then the response should be successful
        And the response should be valid JSON
        And the JSON response field "status" should be "success"
        And I wait for 2 seconds

        When I use the MCP Client to send an initialize request to "http://127.0.0.1:8080/tool-schema-request/mcp"
        Then the response should be successful

        # Valid arguments reach the server and its answer comes back.
        When I use the MCP Client to send "echo" tools/call request to "http://127.0.0.1:8080/tool-schema-request/mcp"
        Then the response should be successful
        And the response body should contain "Hello, World!"

        # A tool with no rule is not touched.
        When I use the MCP Client to send "add" tools/call request to "http://127.0.0.1:8080/tool-schema-request/mcp"
        Then the response should be successful

        # Wrong type plus an extra property: rejected at the gateway with paths, never values.
        When I set header "Accept" to "application/json, text/event-stream"
        And I set header "Content-Type" to "application/json"
        And I send a POST request to "http://127.0.0.1:8080/tool-schema-request/mcp" with body:
            """
            {"jsonrpc":"2.0","id":"call-7","method":"tools/call","params":{"name":"echo","arguments":{"message":42,"extra":"secret-value-123"}}}
            """
        Then the response status code should be 400
        And the response should be valid JSON
        And the JSON response field "id" should be "call-7"
        And the JSON response field "error.message" should be "Tool arguments failed schema validation"
        And the JSON response field "error.data.tool" should be "echo"
        And the JSON response field "error.data.direction" should be "REQUEST"
        And the response body should match pattern "code.:-32602"
        And the response body should contain "/message"
        And the response body should contain "/extra"
        And the response body should not contain "secret-value-123"

        # Missing arguments are validated as {}, so a required property fails.
        When I send a POST request to "http://127.0.0.1:8080/tool-schema-request/mcp" with body:
            """
            {"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"echo"}}
            """
        Then the response status code should be 400
        And the JSON response field "id" should be 8
        And the response body should contain "missing required properties: message"

        # Arguments that are not an object.
        When I send a POST request to "http://127.0.0.1:8080/tool-schema-request/mcp" with body:
            """
            {"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"echo","arguments":"hi"}}
            """
        Then the response status code should be 400
        And the JSON response field "error.data.reason" should be "arguments_not_object"

        # A second arguments member could carry values the gateway never validated.
        When I send a POST request to "http://127.0.0.1:8080/tool-schema-request/mcp" with body:
            """
            {"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"echo","arguments":{"message":"ok"},"arguments":{"message":42}}}
            """
        Then the response status code should be 400
        And the response body should match pattern "code.:-32600"
        And the response body should contain "Request body names a member more than once"

        # A client that accepts only SSE gets the error as one event.
        When I set header "Accept" to "text/event-stream"
        And I send a POST request to "http://127.0.0.1:8080/tool-schema-request/mcp" with body:
            """
            {"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"echo","arguments":{}}}
            """
        Then the response status code should be 400
        And the response header "Content-Type" should contain "text/event-stream"
        And the response body should contain "event: message"
        And the response body should match pattern "code.:-32602"

        When I delete the MCP proxy "tool-schema-request-v1.0"
        Then the response should be successful

    Scenario: Tool results are validated before they reach the client
        Given I authenticate using basic auth as "admin"
        When I deploy this MCP configuration:
            """
            apiVersion: gateway.api-platform.wso2.com/v1
            kind: Mcp
            metadata:
              name: tool-schema-response-v1.0
            spec:
              displayName: Tool Schema Response
              version: v1.0
              context: /tool-schema-response
              specVersion: "2025-06-18"
              upstream:
                url: http://mcp-server-backend:3001/mcp
              policies:
                - name: mcp-tool-schema-validator
                  version: v0
                  params:
                    tools:
                      - name: add
                        input:
                          enabled: true
                          schema: '{"type":"object","properties":{"a":{"type":"number"},"b":{"type":"number"}},"required":["a","b"]}'
                        output:
                          enabled: true
                          schema: '{"type":"object","required":["sum"]}'
                      - name: echo
                        input:
                          enabled: true
                          schema: '{"type":"object","required":["message"]}'
                        output:
                          enabled: false
              tools: []
              resources: []
              prompts: []
            """
        Then the response should be successful
        And the JSON response field "status" should be "success"
        And I wait for 2 seconds

        When I use the MCP Client to send an initialize request to "http://127.0.0.1:8080/tool-schema-response/mcp"
        Then the response should be successful

        # Output validation disabled: echo's result passes as the server sent it.
        When I use the MCP Client to send "echo" tools/call request to "http://127.0.0.1:8080/tool-schema-response/mcp"
        Then the response should be successful
        And the response body should contain "Hello, World!"

        # showAssessment is off: no error list in the rejection.
        When I set header "Accept" to "application/json, text/event-stream"
        And I set header "Content-Type" to "application/json"
        And I send a POST request to "http://127.0.0.1:8080/tool-schema-response/mcp" with body:
            """
            {"jsonrpc":"2.0","id":21,"method":"tools/call","params":{"name":"add","arguments":{"a":1,"b":"x"}}}
            """
        Then the response status code should be 400
        And the response body should match pattern "code.:-32602"
        And the JSON response field "error.data.errors" should not exist

        # Valid arguments reach the server, but its result does not satisfy the output
        # schema, so the gateway replaces it with -32021 at HTTP 200.
        When I send a POST request to "http://127.0.0.1:8080/tool-schema-response/mcp" with body:
            """
            {"jsonrpc":"2.0","id":"r-1","method":"tools/call","params":{"name":"add","arguments":{"a":2,"b":3}}}
            """
        Then the response status code should be 200
        And the response body should match pattern "code.:-32021"
        And the response body should contain "Tool result failed schema validation"
        And the response body should contain "RESPONSE"
        And the response body should contain "r-1"

        When I delete the MCP proxy "tool-schema-response-v1.0"
        Then the response should be successful

    # ==================== INPUT VALIDATION ====================

    Scenario: Invalid arguments are refused at the gateway and never reach the MCP server
        Given I authenticate using basic auth as "admin"
        When I deploy this MCP configuration:
            """
            apiVersion: gateway.api-platform.wso2.com/v1
            kind: Mcp
            metadata:
              name: tool-schema-args-v1.0
            spec:
              displayName: Tool Schema Args
              version: v1.0
              context: /tool-schema-args
              specVersion: "2025-06-18"
              upstream:
                url: http://schema-mcp-backend:3001/mcp
              policies:
                - name: mcp-tool-schema-validator
                  version: v0
                  params:
                    tools:
                      - name: create_user
                        input:
                          enabled: true
                          showAssessment: true
                          schema: '{"type":"object","properties":{"name":{"type":"string","minLength":1},"email":{"type":"string","format":"email"},"age":{"type":"integer","minimum":0,"maximum":150},"role":{"enum":["admin","user"]},"tags":{"type":"array","items":{"type":"string"},"uniqueItems":true,"maxItems":3},"address":{"$ref":"#/$defs/address"}},"required":["name","email"],"additionalProperties":false,"$defs":{"address":{"type":"object","properties":{"zip":{"type":"string","pattern":"^[0-9]{5}$"}},"required":["zip"]}}}'
                      - name: bulk
                        input:
                          enabled: true
                          showAssessment: true
                          schema: '{"type":"object","additionalProperties":false}'
              tools: []
              resources: []
              prompts: []
            """
        Then the response should be successful
        And the JSON response field "status" should be "success"
        And I wait for 2 seconds
        When I send a POST request to "http://127.0.0.1:3021/reset"
        Then the response should be successful

        When I use the MCP Client to send an initialize request to "http://127.0.0.1:8080/tool-schema-args/mcp"
        Then the response should be successful
        When I set header "Accept" to "application/json, text/event-stream"
        And I set header "Content-Type" to "application/json"

        # Valid arguments, a $ref included, reach the server.
        When I send a POST request to "http://127.0.0.1:8080/tool-schema-args/mcp" with body:
            """
            {"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"create_user","arguments":{"name":"Ada","email":"ada@example.com","age":36,"role":"admin","tags":["a","b"],"address":{"zip":"12345"}}}}
            """
        Then the response status code should be 200
        And the response body should contain "ok"

        # format is asserted, and the offending value is never echoed.
        When I send a POST request to "http://127.0.0.1:8080/tool-schema-args/mcp" with body:
            """
            {"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"create_user","arguments":{"name":"Ada","email":"ada-SECRET-EMAIL"}}}
            """
        Then the response status code should be 400
        And the response body should match pattern "code.:-32602"
        And the JSON response field "error.data.errors[0].path" should be "/email"
        And the JSON response field "error.data.errors[0].message" should be "value is not a valid email"
        And the JSON response array field "error.data.errors" should have 1 items
        And the response body should not contain "ada-SECRET-EMAIL"

        # Every failing keyword is reported by path and rule, through $ref, without values.
        When I send a POST request to "http://127.0.0.1:8080/tool-schema-args/mcp" with body:
            """
            {"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"create_user","arguments":{"name":"","email":"ada@example.com","age":200,"role":"superuser","tags":["x","x"],"address":{"zip":"ABCDE-SECRET-ZIP"}}}}
            """
        Then the response status code should be 400
        And the JSON response array field "error.data.errors" should have 5 items
        And the response body should contain "/name"
        And the response body should contain "length must be at least 1"
        And the response body should contain "/age"
        # encoding/json writes "<" as <.
        And the response body should match pattern "must be (<|.u003c)= 150"
        And the response body should contain "/role"
        And the response body should contain "value is not one of the allowed values"
        And the response body should contain "/tags"
        And the response body should contain "items at 0 and 1 are equal"
        And the response body should contain "/address/zip"
        And the response body should contain "value does not match the required pattern"
        And the response body should not contain "superuser"
        And the response body should not contain "ABCDE-SECRET-ZIP"

        # The error list is capped at 20 entries.
        When I send a POST request to "http://127.0.0.1:8080/tool-schema-args/mcp" with body:
            """
            {"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"bulk","arguments":{"p01":1,"p02":1,"p03":1,"p04":1,"p05":1,"p06":1,"p07":1,"p08":1,"p09":1,"p10":1,"p11":1,"p12":1,"p13":1,"p14":1,"p15":1,"p16":1,"p17":1,"p18":1,"p19":1,"p20":1,"p21":1,"p22":1,"p23":1,"p24":1,"p25":1}}}
            """
        Then the response status code should be 400
        And the JSON response array field "error.data.errors" should have 20 items

        # Property names are escaped as RFC 6901 pointer tokens.
        When I send a POST request to "http://127.0.0.1:8080/tool-schema-args/mcp" with body:
            """
            {"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"bulk","arguments":{"a/b":1,"c~d":2}}}
            """
        Then the response status code should be 400
        And the response body should contain "/a~1b"
        And the response body should contain "/c~0d"
        And the response body should contain "property is not allowed"

        # Only the one valid call reached the server.
        When I send a GET request to "http://127.0.0.1:3021/calls/create_user"
        Then the JSON response field "count" should be 1
        When I send a GET request to "http://127.0.0.1:3021/calls/bulk"
        Then the JSON response field "count" should be 0

        When I delete the MCP proxy "tool-schema-args-v1.0"
        Then the response should be successful

    Scenario: The tools/call envelope is read strictly and ids are echoed exactly
        Given I authenticate using basic auth as "admin"
        When I deploy this MCP configuration:
            """
            apiVersion: gateway.api-platform.wso2.com/v1
            kind: Mcp
            metadata:
              name: tool-schema-envelope-v1.0
            spec:
              displayName: Tool Schema Envelope
              version: v1.0
              context: /tool-schema-envelope
              specVersion: "2025-06-18"
              upstream:
                url: http://schema-mcp-backend:3001/mcp
              policies:
                - name: mcp-tool-schema-validator
                  version: v0
                  params:
                    tools:
                      - name: create_user
                        input:
                          enabled: true
                          schema: '{"type":"object","properties":{"name":{"type":"string"},"email":{"type":"string"}},"required":["name","email"]}'
              tools: []
              resources: []
              prompts: []
            """
        Then the response should be successful
        And the JSON response field "status" should be "success"
        And I wait for 2 seconds
        When I send a POST request to "http://127.0.0.1:3021/reset"
        Then the response should be successful

        When I use the MCP Client to send an initialize request to "http://127.0.0.1:8080/tool-schema-envelope/mcp"
        Then the response should be successful
        When I set header "Accept" to "application/json, text/event-stream"
        And I set header "Content-Type" to "application/json"

        # Methods other than tools/call are not touched.
        When I send a POST request to "http://127.0.0.1:8080/tool-schema-envelope/mcp" with body:
            """
            {"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}
            """
        Then the response status code should be 200
        And the response body should contain "lookup"

        # Tool names match exactly: Create_User has no rule and is forwarded as sent.
        When I send a POST request to "http://127.0.0.1:8080/tool-schema-envelope/mcp" with body:
            """
            {"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"Create_User","arguments":{"name":42}}}
            """
        Then the response status code should be 200

        # showAssessment defaults to off: the error names the tool, not the failures.
        When I send a POST request to "http://127.0.0.1:8080/tool-schema-envelope/mcp" with body:
            """
            {"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"create_user","arguments":{"name":"Ada"}}}
            """
        Then the response status code should be 400
        And the JSON response field "error.data.tool" should be "create_user"
        And the JSON response field "error.data.errors" should not exist

        # null arguments are validated as {}.
        When I send a POST request to "http://127.0.0.1:8080/tool-schema-envelope/mcp" with body:
            """
            {"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"create_user","arguments":null}}
            """
        Then the response status code should be 400
        And the response body should match pattern "code.:-32602"

        # A notification carries no id, so the error's id is null. It is still refused.
        When I send a POST request to "http://127.0.0.1:8080/tool-schema-envelope/mcp" with body:
            """
            {"jsonrpc":"2.0","method":"tools/call","params":{"name":"create_user","arguments":{}}}
            """
        Then the response status code should be 400
        And the response body should match pattern "id.:null"

        # A number too large for a float64 comes back with every digit.
        When I send a POST request to "http://127.0.0.1:8080/tool-schema-envelope/mcp" with body:
            """
            {"jsonrpc":"2.0","id":12345678901234567890,"method":"tools/call","params":{"name":"create_user","arguments":{}}}
            """
        Then the response status code should be 400
        And the response body should match pattern "id.:12345678901234567890[,}]"

        # Arguments nested past 64 levels are refused before the validator runs.
        When I send a POST request to "http://127.0.0.1:8080/tool-schema-envelope/mcp" with body:
            """
            {"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"create_user","arguments":{"name":"Ada","email":"ada@example.com","deep":[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[1]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]}}}
            """
        Then the response status code should be 400
        And the JSON response field "error.data.reason" should be "too_deep"

        # A member named twice, or once more in another letter case, could be read one way
        # by the gateway and another by the server. Every such body is refused with -32600,
        # even when the copy the gateway would read is valid.
        When I send a POST request to "http://127.0.0.1:8080/tool-schema-envelope/mcp" with body:
            """
            {"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"create_user","arguments":{"name":"Ada","email":"ada@example.com"},"Arguments":{"name":42}}}
            """
        Then the response status code should be 400
        And the response body should match pattern "code.:-32600"
        When I send a POST request to "http://127.0.0.1:8080/tool-schema-envelope/mcp" with body:
            """
            {"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"Create_User","name":"create_user","arguments":{"name":42}}}
            """
        Then the response status code should be 400
        And the response body should match pattern "code.:-32600"
        When I send a POST request to "http://127.0.0.1:8080/tool-schema-envelope/mcp" with body:
            """
            {"jsonrpc":"2.0","id":8,"method":"tools/list","method":"tools/call","params":{"name":"create_user","arguments":{"name":42}}}
            """
        Then the response status code should be 400
        And the response body should match pattern "code.:-32600"

        # Accept is ranked as RFC 9110 ranks it: JSON at q=0 leaves SSE as the only format.
        When I set header "Accept" to "application/json;q=0, text/event-stream"
        And I send a POST request to "http://127.0.0.1:8080/tool-schema-envelope/mcp" with body:
            """
            {"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"create_user","arguments":{}}}
            """
        Then the response status code should be 400
        And the response header "Content-Type" should contain "text/event-stream"
        And the response body should contain "event: message"
        When I set header "Accept" to "application/json"
        And I send a POST request to "http://127.0.0.1:8080/tool-schema-envelope/mcp" with body:
            """
            {"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"create_user","arguments":{}}}
            """
        Then the response status code should be 400
        And the response header "Content-Type" should contain "application/json"

        # Of all the tools/call above, only the ungoverned Create_User reached the server.
        When I send a GET request to "http://127.0.0.1:3021/calls/create_user"
        Then the JSON response field "count" should be 0
        When I send a GET request to "http://127.0.0.1:3021/calls/Create_User"
        Then the JSON response field "count" should be 1

        When I delete the MCP proxy "tool-schema-envelope-v1.0"
        Then the response should be successful

    # ==================== OUTPUT VALIDATION ====================

    Scenario: JSON tool results are validated against the output schema
        Given I authenticate using basic auth as "admin"
        When I deploy this MCP configuration:
            """
            apiVersion: gateway.api-platform.wso2.com/v1
            kind: Mcp
            metadata:
              name: tool-schema-results-v1.0
            spec:
              displayName: Tool Schema Results
              version: v1.0
              context: /tool-schema-results
              specVersion: "2025-06-18"
              upstream:
                url: http://schema-mcp-backend:3001/mcp
              policies:
                - name: mcp-tool-schema-validator
                  version: v0
                  params:
                    tools:
                      - name: result_valid
                        output:
                          enabled: true
                          showAssessment: true
                          schema: '{"type":"object","properties":{"sku":{"type":"string"},"qty":{"type":"integer","minimum":0}},"required":["sku","qty"],"additionalProperties":false}'
                      - name: result_invalid
                        output:
                          enabled: true
                          showAssessment: true
                          schema: '{"type":"object","properties":{"sku":{"type":"string"},"qty":{"type":"integer","minimum":0}},"required":["sku","qty"],"additionalProperties":false}'
                      - name: result_no_structured
                        output:
                          enabled: true
                          showAssessment: true
                          schema: '{"type":"object","properties":{"sku":{"type":"string"},"qty":{"type":"integer","minimum":0}},"required":["sku","qty"],"additionalProperties":false}'
                      - name: result_null_structured
                        output:
                          enabled: true
                          showAssessment: true
                          schema: '{"type":"object","properties":{"sku":{"type":"string"},"qty":{"type":"integer","minimum":0}},"required":["sku","qty"],"additionalProperties":false}'
                      - name: result_not_object
                        output:
                          enabled: true
                          showAssessment: true
                          schema: '{"type":"object","properties":{"sku":{"type":"string"},"qty":{"type":"integer","minimum":0}},"required":["sku","qty"],"additionalProperties":false}'
                      - name: result_too_deep
                        output:
                          enabled: true
                          showAssessment: true
                          schema: '{"type":"object","properties":{"sku":{"type":"string"},"qty":{"type":"integer","minimum":0}},"required":["sku","qty"],"additionalProperties":false}'
                      - name: result_ambiguous
                        output:
                          enabled: true
                          showAssessment: true
                          schema: '{"type":"object","properties":{"sku":{"type":"string"},"qty":{"type":"integer","minimum":0}},"required":["sku","qty"],"additionalProperties":false}'
                      - name: result_tool_error
                        output:
                          enabled: true
                          showAssessment: true
                          schema: '{"type":"object","properties":{"sku":{"type":"string"},"qty":{"type":"integer","minimum":0}},"required":["sku","qty"],"additionalProperties":false}'
                      - name: result_rpc_error
                        output:
                          enabled: true
                          showAssessment: true
                          schema: '{"type":"object","properties":{"sku":{"type":"string"},"qty":{"type":"integer","minimum":0}},"required":["sku","qty"],"additionalProperties":false}'
                      - name: result_other_id
                        output:
                          enabled: true
                          showAssessment: true
                          schema: '{"type":"object","properties":{"sku":{"type":"string"},"qty":{"type":"integer","minimum":0}},"required":["sku","qty"],"additionalProperties":false}'
                      - name: result_string_id
                        output:
                          enabled: true
                          showAssessment: true
                          schema: '{"type":"object","properties":{"sku":{"type":"string"},"qty":{"type":"integer","minimum":0}},"required":["sku","qty"],"additionalProperties":false}'
                      - name: result_http_500
                        output:
                          enabled: true
                          showAssessment: true
                          schema: '{"type":"object","properties":{"sku":{"type":"string"},"qty":{"type":"integer","minimum":0}},"required":["sku","qty"],"additionalProperties":false}'
              tools: []
              resources: []
              prompts: []
            """
        Then the response should be successful
        And the JSON response field "status" should be "success"
        And I wait for 2 seconds
        When I send a POST request to "http://127.0.0.1:3021/reset"
        Then the response should be successful

        When I use the MCP Client to send an initialize request to "http://127.0.0.1:8080/tool-schema-results/mcp"
        Then the response should be successful
        When I set header "Accept" to "application/json, text/event-stream"
        And I set header "Content-Type" to "application/json"

        # A valid result is forwarded byte for byte.
        When I send a POST request to "http://127.0.0.1:8080/tool-schema-results/mcp" with body:
            """
            {"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"result_valid","arguments":{}}}
            """
        Then the response status code should be 200
        And the response body should be:
            """
            {"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"A-100 x3"}],"structuredContent":{"sku":"A-100","qty":3}}}
            """

        # An invalid result is replaced by -32021 at HTTP 200, with paths but no values.
        When I send a POST request to "http://127.0.0.1:8080/tool-schema-results/mcp" with body:
            """
            {"jsonrpc":"2.0","id":"inv-1","method":"tools/call","params":{"name":"result_invalid","arguments":{}}}
            """
        Then the response status code should be 200
        And the JSON response field "id" should be "inv-1"
        And the response body should match pattern "code.:-32021"
        And the JSON response field "error.message" should be "Tool result failed schema validation"
        And the JSON response field "error.data.tool" should be "result_invalid"
        And the JSON response field "error.data.direction" should be "RESPONSE"
        And the JSON response field "error.data.reason" should not exist
        And the response body should contain "/qty"
        And the response body should contain "got string, want integer"
        And the response body should contain "/token"
        And the response body should not contain "sk-live-SECRET-9999"
        And the response body should not contain "structuredContent"

        # A result with no usable structuredContent cannot satisfy an output schema.
        When I send a POST request to "http://127.0.0.1:8080/tool-schema-results/mcp" with body:
            """
            {"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"result_no_structured","arguments":{}}}
            """
        Then the response status code should be 200
        And the response body should match pattern "code.:-32021"
        And the JSON response field "error.data.reason" should be "missing_structured_content"
        When I send a POST request to "http://127.0.0.1:8080/tool-schema-results/mcp" with body:
            """
            {"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"result_null_structured","arguments":{}}}
            """
        Then the JSON response field "error.data.reason" should be "missing_structured_content"
        When I send a POST request to "http://127.0.0.1:8080/tool-schema-results/mcp" with body:
            """
            {"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"result_not_object","arguments":{}}}
            """
        Then the JSON response field "error.data.reason" should be "missing_structured_content"
        # Not covered here: a 200 with an empty body. The policy replaces it, but Envoy hands
        # a body-less response to the policy engine as headers only, and the engine returns
        # the new body as a body_mutation on that headers reply. Envoy drops the body and keeps
        # the new content-length, so the client waits for bytes that never come. That is a
        # gateway limitation, not this policy's; response_test.go covers the policy's side.

        # structuredContent nested past 64 levels.
        When I send a POST request to "http://127.0.0.1:8080/tool-schema-results/mcp" with body:
            """
            {"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"result_too_deep","arguments":{}}}
            """
        Then the JSON response field "error.data.reason" should be "too_deep"

        # structuredContent named twice: the valid first copy does not vouch for the second.
        When I send a POST request to "http://127.0.0.1:8080/tool-schema-results/mcp" with body:
            """
            {"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"result_ambiguous","arguments":{}}}
            """
        Then the JSON response field "error.data.reason" should be "ambiguous_payload"
        And the response body should not contain "sk-live-SECRET-9999"

        # A tool error (isError) and a JSON-RPC error are not results, so they pass.
        When I send a POST request to "http://127.0.0.1:8080/tool-schema-results/mcp" with body:
            """
            {"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"result_tool_error","arguments":{}}}
            """
        Then the response status code should be 200
        And the JSON response field "result.isError" should be true
        And the response body should contain "inventory service offline"
        When I send a POST request to "http://127.0.0.1:8080/tool-schema-results/mcp" with body:
            """
            {"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"result_rpc_error","arguments":{}}}
            """
        Then the response status code should be 200
        And the response body should match pattern "code.:-32000"
        And the response body should contain "Upstream tool crashed"

        # A response that answers some other id is not the result of this call. JSON-RPC
        # ids compare by type too, so "10" does not answer 10.
        When I send a POST request to "http://127.0.0.1:8080/tool-schema-results/mcp" with body:
            """
            {"jsonrpc":"2.0","id":"mine","method":"tools/call","params":{"name":"result_other_id","arguments":{}}}
            """
        Then the response status code should be 200
        And the response body should contain "not-your-call"
        And the response body should not contain "-32021"
        When I send a POST request to "http://127.0.0.1:8080/tool-schema-results/mcp" with body:
            """
            {"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"result_string_id","arguments":{}}}
            """
        Then the response status code should be 200
        And the JSON response field "id" should be "10"
        And the response body should not contain "-32021"

        # A non-2xx status is a transport failure, not a tool result: passed as sent.
        When I send a POST request to "http://127.0.0.1:8080/tool-schema-results/mcp" with body:
            """
            {"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"result_http_500","arguments":{}}}
            """
        Then the response status code should be 500
        And the response body should not contain "-32021"

        # A tools/call notification expects no response, so none is validated.
        When I send a POST request to "http://127.0.0.1:8080/tool-schema-results/mcp" with body:
            """
            {"jsonrpc":"2.0","method":"tools/call","params":{"name":"result_invalid","arguments":{}}}
            """
        Then the response status code should be 202
        When I send a GET request to "http://127.0.0.1:3021/calls/result_invalid"
        Then the JSON response field "count" should be 2

        When I delete the MCP proxy "tool-schema-results-v1.0"
        Then the response should be successful

    Scenario: SSE tool results are validated event by event
        Given I authenticate using basic auth as "admin"
        When I deploy this MCP configuration:
            """
            apiVersion: gateway.api-platform.wso2.com/v1
            kind: Mcp
            metadata:
              name: tool-schema-sse-v1.0
            spec:
              displayName: Tool Schema SSE
              version: v1.0
              context: /tool-schema-sse
              specVersion: "2025-06-18"
              upstream:
                url: http://schema-mcp-backend:3001/mcp
              policies:
                - name: mcp-tool-schema-validator
                  version: v0
                  params:
                    tools:
                      - name: result_valid_sse
                        output:
                          enabled: true
                          schema: '{"type":"object","properties":{"sku":{"type":"string"},"qty":{"type":"integer","minimum":0}},"required":["sku","qty"],"additionalProperties":false}'
                      - name: result_invalid_sse
                        output:
                          enabled: true
                          schema: '{"type":"object","properties":{"sku":{"type":"string"},"qty":{"type":"integer","minimum":0}},"required":["sku","qty"],"additionalProperties":false}'
              tools: []
              resources: []
              prompts: []
            """
        Then the response should be successful
        And the JSON response field "status" should be "success"
        And I wait for 2 seconds

        When I use the MCP Client to send an initialize request to "http://127.0.0.1:8080/tool-schema-sse/mcp"
        Then the response should be successful
        When I set header "Accept" to "application/json, text/event-stream"
        And I set header "Content-Type" to "application/json"

        # A valid stream is forwarded byte for byte, the progress notification included.
        When I send a POST request to "http://127.0.0.1:8080/tool-schema-sse/mcp" with body:
            """
            {"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"result_valid_sse","arguments":{}}}
            """
        Then the response status code should be 200
        And the response header "Content-Type" should contain "text/event-stream"
        And the response body should be:
            """
            event: message
            data: {"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"p-1","progress":1}}

            event: message
            id: evt-2
            data: {"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"A-100 x3"}],"structuredContent":{"sku":"A-100","qty":3}}}
            """

        # In an invalid stream only the answering event's data is replaced. The
        # notification before it and the event's own id line are kept.
        When I send a POST request to "http://127.0.0.1:8080/tool-schema-sse/mcp" with body:
            """
            {"jsonrpc":"2.0","id":"s-1","method":"tools/call","params":{"name":"result_invalid_sse","arguments":{}}}
            """
        Then the response status code should be 200
        And the response header "Content-Type" should contain "text/event-stream"
        And the response body should be:
            """
            event: message
            data: {"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"p-1","progress":1}}

            event: message
            id: evt-2
            data: {"error":{"code":-32021,"data":{"tool":"result_invalid_sse","direction":"RESPONSE"},"message":"Tool result failed schema validation"},"id":"s-1","jsonrpc":"2.0"}
            """

        When I delete the MCP proxy "tool-schema-sse-v1.0"
        Then the response should be successful

    Scenario: Results of tools without an enabled output rule pass unchanged
        Given I authenticate using basic auth as "admin"
        When I deploy this MCP configuration:
            """
            apiVersion: gateway.api-platform.wso2.com/v1
            kind: Mcp
            metadata:
              name: tool-schema-unarmed-v1.0
            spec:
              displayName: Tool Schema Unarmed
              version: v1.0
              context: /tool-schema-unarmed
              specVersion: "2025-06-18"
              upstream:
                url: http://schema-mcp-backend:3001/mcp
              policies:
                - name: mcp-tool-schema-validator
                  version: v0
                  params:
                    tools:
                      - name: result_invalid__input_only
                        input:
                          enabled: true
                          schema: '{"type":"object"}'
                      - name: result_invalid__output_disabled
                        input:
                          enabled: true
                          schema: '{"type":"object"}'
                        output:
                          enabled: false
                          schema: '{"type":"object","required":["never"]}'
              tools: []
              resources: []
              prompts: []
            """
        Then the response should be successful
        And the JSON response field "status" should be "success"
        And I wait for 2 seconds

        When I use the MCP Client to send an initialize request to "http://127.0.0.1:8080/tool-schema-unarmed/mcp"
        Then the response should be successful
        When I set header "Accept" to "application/json, text/event-stream"
        And I set header "Content-Type" to "application/json"

        When I send a POST request to "http://127.0.0.1:8080/tool-schema-unarmed/mcp" with body:
            """
            {"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"result_invalid__input_only","arguments":{}}}
            """
        Then the response status code should be 200
        And the JSON response field "result.structuredContent.qty" should be "three"
        When I send a POST request to "http://127.0.0.1:8080/tool-schema-unarmed/mcp" with body:
            """
            {"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"result_invalid__output_disabled","arguments":{}}}
            """
        Then the response status code should be 200
        And the JSON response field "result.structuredContent.qty" should be "three"
        When I send a POST request to "http://127.0.0.1:8080/tool-schema-unarmed/mcp" with body:
            """
            {"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"result_invalid__ungoverned","arguments":{}}}
            """
        Then the response status code should be 200
        And the JSON response field "result.structuredContent.qty" should be "three"

        When I delete the MCP proxy "tool-schema-unarmed-v1.0"
        Then the response should be successful

    # ==================== CONFIGURATION ====================

    Scenario Outline: The controller rejects parameters the policy definition does not allow
        Given I authenticate using basic auth as "admin"
        When I deploy this MCP configuration:
            """
            apiVersion: gateway.api-platform.wso2.com/v1
            kind: Mcp
            metadata:
              name: tool-schema-bad-def-v1.0
            spec:
              displayName: Tool Schema Bad Definition
              version: v1.0
              context: /tool-schema-bad-def
              specVersion: "2025-06-18"
              upstream:
                url: http://schema-mcp-backend:3001/mcp
              policies:
                - name: mcp-tool-schema-validator
                  version: v0
                  params:
                    tools: <tools>
              tools: []
              resources: []
              prompts: []
            """
        Then the response should be a client error

        Examples:
            | case                  | tools                                                                           |
            | no tools              | []                                                                              |
            | tool without a name   | [{"input":{"enabled":true,"schema":"{}"}}]                                      |
            | empty tool name       | [{"name":"","input":{"enabled":true,"schema":"{}"}}]                            |
            | unknown tool property | [{"name":"t","strict":true,"input":{"enabled":true,"schema":"{}"}}]             |
            | unknown input field   | [{"name":"t","input":{"enabled":true,"schema":"{}","mode":"strict"}}]           |

    Scenario Outline: A configuration the policy cannot compile fails closed
        Given I authenticate using basic auth as "admin"
        When I deploy this MCP configuration:
            """
            apiVersion: gateway.api-platform.wso2.com/v1
            kind: Mcp
            metadata:
              name: tool-schema-broken-<n>-v1.0
            spec:
              displayName: Tool Schema Broken
              version: v1.0
              context: /tool-schema-broken-<n>
              specVersion: "2025-06-18"
              upstream:
                url: http://schema-mcp-backend:3001/mcp
              policies:
                - name: mcp-tool-schema-validator
                  version: v0
                  params:
                    tools: <tools>
              tools: []
              resources: []
              prompts: []
            """
        Then the response should be successful
        And I wait for 2 seconds
        When I send a POST request to "http://127.0.0.1:3021/reset"
        Then the response should be successful

        # The route has no usable policy chain, so nothing is forwarded to the server.
        When I set header "Accept" to "application/json, text/event-stream"
        And I set header "Content-Type" to "application/json"
        And I send a POST request to "http://127.0.0.1:8080/tool-schema-broken-<n>/mcp" with body:
            """
            {"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"t","arguments":{"x":1}}}
            """
        Then the response status code should be 500
        When I send a GET request to "http://127.0.0.1:3021/calls"
        Then the JSON response field "total" should be 0

        When I delete the MCP proxy "tool-schema-broken-<n>-v1.0"
        Then the response should be successful

        Examples:
            | n  | case                     | tools                                                                                  |
            | 1  | schema is not JSON       | [{"name":"t","input":{"enabled":true,"schema":"{not json"}}]                           |
            | 2  | schema is not an object  | [{"name":"t","input":{"enabled":true,"schema":"[1,2]"}}]                               |
            | 3  | remote $ref              | [{"name":"t","input":{"enabled":true,"schema":"{\"$ref\":\"https://example.com/s.json\"}"}}] |
            | 4  | invalid keyword value    | [{"name":"t","input":{"enabled":true,"schema":"{\"type\":\"banana\"}"}}]               |
            | 5  | enabled without schema   | [{"name":"t","input":{"enabled":true}}]                                                |
            | 6  | nothing enabled          | [{"name":"t","input":{"enabled":false,"schema":"{}"}}]                                 |
            | 7  | broken disabled schema   | [{"name":"t","input":{"enabled":true,"schema":"{}"},"output":{"schema":"{not json"}}]  |
            | 8  | duplicate tool name      | [{"name":"t","input":{"enabled":true,"schema":"{}"}},{"name":"t","output":{"enabled":true,"schema":"{}"}}] |
