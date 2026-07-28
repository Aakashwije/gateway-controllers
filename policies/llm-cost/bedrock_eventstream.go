/*
 * Copyright (c) 2026, WSO2 LLC. (http://www.wso2.org) All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package llmcost

import (
	"encoding/binary"
	"encoding/json"
)

const (
	bedrockEventStreamPreludeLen = 12
	bedrockEventStreamOverhead   = 16
	bedrockEventStreamMaxFrame   = 16 * 1024 * 1024
)

type bedrockEventStreamFrame struct {
	eventType string
	payload   []byte
}

// bedrockConverseStreamMetadata extracts the JSON payload from the metadata
// event in a complete Amazon event-stream response. The trailing and prelude
// CRC fields are framing bytes here; integrity is already handled by the
// upstream transport.
func bedrockConverseStreamMetadata(data []byte) ([]byte, bool) {
	var metadata []byte
	frames, ok := bedrockEventStreamFrames(data)
	if !ok {
		return nil, false
	}
	for _, frame := range frames {
		if frame.eventType == "metadata" {
			if !json.Valid(frame.payload) {
				return nil, false
			}
			metadata = append(metadata[:0], frame.payload...)
		}
	}

	return metadata, metadata != nil
}

// bedrockInvokeStreamResponse extracts and merges the model-native JSON objects
// carried by chunk events in an InvokeModelWithResponseStream response.
//
// On the Amazon event-stream wire, a PayloadPart is normally the raw model JSON
// payload. Some intermediaries serialize the SDK union member instead, producing
// either {"bytes":"<base64>"} or {"chunk":{"bytes":"<base64>"}}. Accept all
// three forms so normalization works for direct and serialized Bedrock streams.
func bedrockInvokeStreamResponse(data []byte) ([]byte, bool) {
	frames, ok := bedrockEventStreamFrames(data)
	if !ok {
		return nil, false
	}

	payloads := make([][]byte, 0, len(frames))
	for _, frame := range frames {
		if frame.eventType != "chunk" {
			continue
		}
		payload, ok := bedrockInvokeChunkPayload(frame.payload)
		if !ok {
			return nil, false
		}
		payloads = append(payloads, payload)
	}
	if len(payloads) == 0 {
		return nil, false
	}

	merged, err := mergeJSONEvents(payloads)
	return merged, err == nil
}

func bedrockInvokeChunkPayload(payload []byte) ([]byte, bool) {
	if !json.Valid(payload) {
		return nil, false
	}

	var envelope struct {
		Bytes []byte `json:"bytes"`
		Chunk *struct {
			Bytes []byte `json:"bytes"`
		} `json:"chunk"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, false
	}
	switch {
	case len(envelope.Bytes) != 0:
		payload = envelope.Bytes
	case envelope.Chunk != nil && len(envelope.Chunk.Bytes) != 0:
		payload = envelope.Chunk.Bytes
	}
	return payload, json.Valid(payload)
}

func bedrockEventStreamFrames(data []byte) ([]bedrockEventStreamFrame, bool) {
	var frames []bedrockEventStreamFrame
	for offset := 0; offset < len(data); {
		remaining := data[offset:]
		if len(remaining) < bedrockEventStreamPreludeLen {
			return nil, false
		}

		totalLen := int(binary.BigEndian.Uint32(remaining[0:4]))
		headersLen := int(binary.BigEndian.Uint32(remaining[4:8]))
		if totalLen < bedrockEventStreamOverhead ||
			totalLen > bedrockEventStreamMaxFrame ||
			headersLen > totalLen-bedrockEventStreamOverhead ||
			totalLen > len(remaining) {
			return nil, false
		}

		headersEnd := bedrockEventStreamPreludeLen + headersLen
		eventType, ok := bedrockEventStreamEventType(remaining[bedrockEventStreamPreludeLen:headersEnd])
		if !ok {
			return nil, false
		}
		frames = append(frames, bedrockEventStreamFrame{
			eventType: eventType,
			payload:   remaining[headersEnd : totalLen-4],
		})
		offset += totalLen
	}
	return frames, len(frames) != 0
}

// bedrockEventStreamEventType walks the packed Amazon event-stream headers and
// returns the :event-type string. Unknown or truncated header encodings make
// the frame invalid rather than risking an out-of-bounds read.
func bedrockEventStreamEventType(data []byte) (string, bool) {
	var eventType string
	for offset := 0; offset < len(data); {
		nameLen := int(data[offset])
		offset++
		if offset+nameLen >= len(data) {
			return "", false
		}
		name := string(data[offset : offset+nameLen])
		offset += nameLen
		valueType := data[offset]
		offset++

		var valueLen int
		var valueOffset int
		switch valueType {
		case 0, 1: // true, false
		case 2: // byte
			valueLen = 1
		case 3: // short
			valueLen = 2
		case 4: // integer
			valueLen = 4
		case 5, 8: // long, timestamp
			valueLen = 8
		case 9: // UUID
			valueLen = 16
		case 6, 7: // byte array, string
			if offset+2 > len(data) {
				return "", false
			}
			valueLen = int(binary.BigEndian.Uint16(data[offset : offset+2]))
			valueOffset = 2
		default:
			return "", false
		}
		if offset+valueOffset+valueLen > len(data) {
			return "", false
		}
		if name == ":event-type" && valueType == 7 {
			eventType = string(data[offset+valueOffset : offset+valueOffset+valueLen])
		}
		offset += valueOffset + valueLen
	}
	return eventType, true
}
