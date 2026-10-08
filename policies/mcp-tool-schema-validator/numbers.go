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
	"errors"
	"math"
	"strconv"
	"strings"
)

// decodeInstance decodes the value to validate. On failure it returns the assessment to report
// instead, with invalidJSON as the message for a value that does not parse.
func decodeInstance(data []byte, invalidJSON string) (any, []assessmentError) {
	value, err := decodeJSON(data)
	if err != nil {
		return nil, []assessmentError{{Path: "", Message: invalidJSON}}
	}
	value, err = normalizeNumbers(value, "")
	if err != nil {
		path := ""
		if imprecise, ok := err.(*impreciseNumberError); ok {
			path = imprecise.path
		}
		return nil, []assessmentError{{Path: path, Message: "number cannot be validated precisely"}}
	}
	return value, nil
}

// decodeJSON decodes exactly one JSON value, keeping every number as its literal.
func decodeJSON(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err := ensureDecoderEOF(decoder); err != nil {
		return nil, err
	}
	return value, nil
}

// impreciseNumberError names a number that no Go numeric type represents faithfully enough to
// validate. path is its JSON Pointer; the literal itself is never kept.
type impreciseNumberError struct{ path string }

func (e *impreciseNumberError) Error() string {
	return "number at " + strconv.Quote(e.path) + " cannot be represented precisely"
}

// normalizeNumbers replaces each json.Number in a decoded value, in place, with a type
// google/jsonschema-go classifies correctly. The library sees a json.Number as a string, so one
// left in place would pass "type": "string" and fail "type": "integer".
//
// An integer that fits is an int64 or uint64 and compared exactly, which float64 cannot do past
// 2^53. Any other number is a float64, as the library would hold it. A number whose float64 would
// change what the schema sees is refused rather than rounded: one out of float64 range, or a
// fraction such as 0.99999999999999999999 that rounds to a whole number and would pass
// "type": "integer".
func normalizeNumbers(value any, path string) (any, error) {
	switch v := value.(type) {
	case json.Number:
		n, ok := exactNumber(string(v))
		if !ok {
			return nil, &impreciseNumberError{path: path}
		}
		return n, nil
	case map[string]any:
		for key, item := range v {
			n, err := normalizeNumbers(item, path+"/"+escapePointerToken(key))
			if err != nil {
				return nil, err
			}
			v[key] = n
		}
	case []any:
		for i, item := range v {
			n, err := normalizeNumbers(item, path+"/"+strconv.Itoa(i))
			if err != nil {
				return nil, err
			}
			v[i] = n
		}
	}
	return value, nil
}

// maxExactDigits bounds the integers rebuilt digit by digit: uint64 has at most 20 digits.
const maxExactDigits = 20

// exactNumber converts a JSON number literal, which the decoder has already checked for syntax.
func exactNumber(lit string) (any, bool) {
	if i, err := strconv.ParseInt(lit, 10, 64); err == nil {
		return i, true
	}
	if u, err := strconv.ParseUint(lit, 10, 64); err == nil {
		return u, true
	}

	// Split the literal into its significant digits and a power of ten.
	neg := strings.HasPrefix(lit, "-")
	mantissa, exponent := strings.TrimPrefix(lit, "-"), ""
	if i := strings.IndexAny(mantissa, "eE"); i >= 0 {
		mantissa, exponent = mantissa[:i], mantissa[i+1:]
	}
	whole, fraction, _ := strings.Cut(mantissa, ".")
	digits := strings.TrimLeft(whole+fraction, "0")
	if digits == "" {
		return int64(0), true
	}
	exp, err := strconv.ParseInt(exponent, 10, 32)
	if exponent == "" {
		exp = 0
	} else if err != nil {
		// Far beyond float64 range either way; the sign decides between overflow and underflow.
		exp = math.MaxInt32
		if strings.HasPrefix(exponent, "-") {
			exp = math.MinInt32
		}
	}
	trimmed := strings.TrimRight(digits, "0")
	exp += int64(len(digits)-len(trimmed)) - int64(len(fraction))
	digits = trimmed
	isInteger := exp >= 0

	if isInteger && int64(len(digits))+exp <= maxExactDigits {
		text := digits + strings.Repeat("0", int(exp))
		if neg {
			text = "-" + text
		}
		if i, err := strconv.ParseInt(text, 10, 64); err == nil {
			return i, true
		}
		if u, err := strconv.ParseUint(text, 10, 64); err == nil {
			return u, true
		}
	}

	f, err := strconv.ParseFloat(lit, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return nil, false
	}
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return nil, false
	}
	if _, frac := math.Modf(f); frac == 0 && !isInteger {
		return nil, false
	}
	return f, true
}
