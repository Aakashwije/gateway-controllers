/*
 * Copyright (c) 2026, WSO2 LLC. (https://www.wso2.com).
 *
 * WSO2 LLC. licenses this file to you under the Apache License,
 * Version 2.0 (the "License"); you may not use this file except
 * in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package mcptoolschemavalidator

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// jsonMember is one object member. A slice of these keeps repeated names that a map would merge.
type jsonMember struct {
	name  string
	value json.RawMessage
}

// objectMembers returns an object's members in document order, duplicates included. ok is false
// for anything that is not exactly one well-formed object. Copied from mcp-spec-validation, which
// keeps it unexported.
func objectMembers(raw []byte) ([]jsonMember, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, false
	}
	if delim, isDelim := tok.(json.Delim); !isDelim || delim != '{' {
		return nil, false
	}
	var members []jsonMember
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, false
		}
		name, isString := tok.(string)
		if !isString {
			return nil, false
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, false
		}
		members = append(members, jsonMember{name: name, value: value})
	}
	// The closing brace, then end of input: trailing bytes make the body malformed.
	if _, err := dec.Token(); err != nil {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return members, true
}

// lookupMember finds a member the way mcp-spec-validation judges ambiguity. encoding/json matches
// names case-insensitively and keeps the last match, so a name given twice, or once in another
// letter case, may be read differently by the MCP server than by the gateway. ambiguous reports
// either; value is then meaningless.
func lookupMember(members []jsonMember, canonical string) (value json.RawMessage, found, ambiguous bool) {
	for _, member := range members {
		if !strings.EqualFold(member.name, canonical) {
			continue
		}
		if found || member.name != canonical {
			ambiguous = true
		}
		found = true
		value = member.value
	}
	return value, found, ambiguous
}

// allMembers returns every value whose name folds onto canonical.
func allMembers(members []jsonMember, canonical string) []json.RawMessage {
	var out []json.RawMessage
	for _, member := range members {
		if strings.EqualFold(member.name, canonical) {
			out = append(out, member.value)
		}
	}
	return out
}

// requestParse classifies a request body.
type requestParse int

const (
	// parseUnreadable is a body that is not a single JSON object: malformed, a batch or a scalar.
	// mcp-spec-validation owns those.
	parseUnreadable requestParse = iota
	// parseOther is a readable request that is not a tools/call naming a tool.
	parseOther
	// parseAmbiguous is a tools/call, or possibly one, naming a member the policy reads twice.
	parseAmbiguous
	// parseToolCall is a tools/call with a single string params.name.
	parseToolCall
)

// toolCall is what the policy reads from a tools/call request.
type toolCall struct {
	// id is the JSON token of the id as sent; "" for a notification, which carries none.
	id    string
	name  string
	hasID bool
	// arguments is params.arguments as sent; nil when absent.
	arguments json.RawMessage
	// argumentsAmbiguous is judged only when the tool validates its arguments.
	argumentsAmbiguous bool
}

// parseRequest reads a tools/call envelope strictly. Only the members this policy acts on are
// checked for ambiguity: method (is this a call at all), id (correlation), params and name (which
// tool) and arguments (what is validated).
func parseRequest(body []byte) (toolCall, requestParse) {
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return toolCall{}, parseUnreadable
	}
	members, ok := objectMembers(trimmed)
	if !ok {
		return toolCall{}, parseUnreadable
	}

	methodRaw, found, ambiguous := lookupMember(members, "method")
	if ambiguous {
		return toolCall{}, parseAmbiguous
	}
	var method string
	if !found || json.Unmarshal(methodRaw, &method) != nil || method != "tools/call" {
		return toolCall{}, parseOther
	}

	var call toolCall
	idRaw, hasID, ambiguous := lookupMember(members, "id")
	if ambiguous {
		return toolCall{}, parseAmbiguous
	}
	if hasID {
		call.hasID = true
		call.id = string(bytes.TrimSpace(idRaw))
	}

	paramsRaw, found, ambiguous := lookupMember(members, "params")
	if ambiguous {
		return toolCall{}, parseAmbiguous
	}
	if !found {
		return toolCall{}, parseOther
	}
	params, ok := objectMembers(paramsRaw)
	if !ok {
		return toolCall{}, parseOther
	}

	nameRaw, found, ambiguous := lookupMember(params, "name")
	if ambiguous {
		return toolCall{}, parseAmbiguous
	}
	if !found || json.Unmarshal(nameRaw, &call.name) != nil {
		// Missing or not a string: not a call this policy can attribute to a tool.
		return toolCall{}, parseOther
	}

	call.arguments, _, call.argumentsAmbiguous = lookupMember(params, "arguments")
	return call, parseToolCall
}

// responseMessage is what the policy reads from one JSON-RPC response.
type responseMessage struct {
	// ids is every id member, so a response naming two ids is still recognised as the answer.
	ids []json.RawMessage
	// ambiguous means a member the policy reads is named twice or in another letter case.
	ambiguous         bool
	hasError          bool
	hasResult         bool
	resultIsObject    bool
	isError           bool
	structuredContent json.RawMessage // nil when absent or null
}

// parseResponse reads a JSON-RPC response. ok is false for anything that is not an object.
func parseResponse(data []byte) (responseMessage, bool) {
	members, ok := objectMembers(bytes.TrimSpace(data))
	if !ok {
		return responseMessage{}, false
	}
	msg := responseMessage{ids: allMembers(members, "id")}
	if len(msg.ids) > 1 {
		msg.ambiguous = true
	}

	errRaw, hasErr, ambiguous := lookupMember(members, "error")
	msg.ambiguous = msg.ambiguous || ambiguous
	msg.hasError = hasErr && !isNull(errRaw)

	resultRaw, hasResult, ambiguous := lookupMember(members, "result")
	msg.ambiguous = msg.ambiguous || ambiguous
	msg.hasResult = hasResult
	if !hasResult {
		return msg, true
	}
	result, ok := objectMembers(resultRaw)
	if !ok {
		return msg, true
	}
	msg.resultIsObject = true

	isErrRaw, _, ambiguous := lookupMember(result, "isError")
	msg.ambiguous = msg.ambiguous || ambiguous
	msg.isError = string(bytes.TrimSpace(isErrRaw)) == "true"

	sc, found, ambiguous := lookupMember(result, "structuredContent")
	msg.ambiguous = msg.ambiguous || ambiguous
	if found && !isNull(sc) {
		msg.structuredContent = sc
	}
	return msg, true
}

// answers reports whether the response carries the request's id. Ids compare as compacted JSON,
// so "7" and 7 differ, as JSON-RPC requires.
func (m responseMessage) answers(requestID string) bool {
	want := compactJSON([]byte(requestID))
	for _, id := range m.ids {
		if bytes.Equal(compactJSON(id), want) {
			return true
		}
	}
	return false
}

func compactJSON(raw []byte) []byte {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return bytes.TrimSpace(raw)
	}
	return buf.Bytes()
}

func isNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

// sseEvent is one event of a text/event-stream body, located by byte offsets so every other event
// can be forwarded exactly as the server sent it.
type sseEvent struct {
	// start and end bound the event's lines, excluding the final line's terminator.
	start, end int
	// lines are the event's lines without terminators.
	lines []string
	// data is the event's data fields joined with "\n", per the SSE spec.
	data string
}

// parseEventStream splits an SSE body into events at blank lines. Lines end with LF or CRLF.
func parseEventStream(body []byte) []sseEvent {
	var events []sseEvent
	var current *sseEvent
	var dataLines []string

	flush := func() {
		if current == nil {
			return
		}
		current.data = strings.Join(dataLines, "\n")
		events = append(events, *current)
		current, dataLines = nil, nil
	}

	pos := 0
	for pos < len(body) {
		next := bytes.IndexByte(body[pos:], '\n')
		lineEnd := len(body)
		if next >= 0 {
			lineEnd = pos + next
		}
		contentEnd := lineEnd
		if contentEnd > pos && body[contentEnd-1] == '\r' {
			contentEnd--
		}
		line := string(body[pos:contentEnd])

		if line == "" {
			flush()
		} else {
			if current == nil {
				current = &sseEvent{start: pos}
			}
			current.end = contentEnd
			current.lines = append(current.lines, line)
			if value, isData := strings.CutPrefix(line, "data:"); isData {
				dataLines = append(dataLines, strings.TrimPrefix(value, " "))
			} else if line == "data" {
				dataLines = append(dataLines, "")
			}
		}
		if next < 0 {
			break
		}
		pos = lineEnd + 1
	}
	flush()
	return events
}

// replaceEventData swaps one event's data for message, keeping its other fields (event, id,
// retry, comments) and every byte of the stream outside it.
func replaceEventData(body []byte, event sseEvent, message []byte) []byte {
	var rebuilt []string
	written := false
	for _, line := range event.lines {
		if strings.HasPrefix(line, "data:") || line == "data" {
			if !written {
				rebuilt = append(rebuilt, "data: "+string(message))
				written = true
			}
			continue
		}
		rebuilt = append(rebuilt, line)
	}
	if !written {
		rebuilt = append(rebuilt, "data: "+string(message))
	}

	out := make([]byte, 0, len(body)+len(message))
	out = append(out, body[:event.start]...)
	out = append(out, strings.Join(rebuilt, "\n")...)
	out = append(out, body[event.end:]...)
	return out
}
