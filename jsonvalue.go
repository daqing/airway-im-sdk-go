package airwayim

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
)

// JSONValue is a decoded-any JSON value, used by the admin API where the
// payload shapes are operational and open-ended (mirrors the raw
// Hash/array returns of the Ruby and PHP SDKs). The zero value represents
// JSON null.
type JSONValue struct {
	inner any // nil | string | int64 | float64 | bool | []JSONValue | map[string]JSONValue
}

// UnmarshalJSON decodes any JSON document, preserving integer precision.
func (v *JSONValue) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	decoded, err := decodeJSONValue(decoder)
	if err != nil {
		return err
	}
	v.inner = decoded
	return nil
}

func decodeJSONValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch value := token.(type) {
	case json.Delim:
		switch value {
		case '{':
			object := map[string]JSONValue{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyToken.(string)
				if !ok {
					return nil, fmt.Errorf("json: non-string object key")
				}
				member, err := decodeJSONValue(decoder)
				if err != nil {
					return nil, err
				}
				object[key] = JSONValue{inner: member}
			}
			if _, err := decoder.Token(); err != nil { // consume '}'
				return nil, err
			}
			return object, nil
		case '[':
			var array []JSONValue
			for decoder.More() {
				element, err := decodeJSONValue(decoder)
				if err != nil {
					return nil, err
				}
				array = append(array, JSONValue{inner: element})
			}
			if _, err := decoder.Token(); err != nil { // consume ']'
				return nil, err
			}
			return array, nil
		default:
			return nil, fmt.Errorf("json: unexpected delimiter %q", value)
		}
	case json.Number:
		if integer, err := value.Int64(); err == nil {
			return integer, nil
		}
		float, err := value.Float64()
		if err != nil {
			return nil, err
		}
		return float, nil
	case string:
		return value, nil
	case bool:
		return value, nil
	default:
		return nil, nil // json null
	}
}

// MarshalJSON re-encodes the value.
func (v JSONValue) MarshalJSON() ([]byte, error) {
	encoded, err := json.Marshal(encodeJSONValue(v))
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

func encodeJSONValue(v JSONValue) any {
	switch value := v.inner.(type) {
	case []JSONValue:
		array := make([]any, len(value))
		for i, element := range value {
			array[i] = encodeJSONValue(element)
		}
		return array
	case map[string]JSONValue:
		object := make(map[string]any, len(value))
		for key, member := range value {
			object[key] = encodeJSONValue(member)
		}
		return object
	default:
		return value
	}
}

// String renders the value for debugging: compact JSON for structured
// values, the raw literal for scalars.
func (v JSONValue) String() string {
	switch value := v.inner.(type) {
	case nil:
		return "null"
	case string:
		return strconv.Quote(value)
	case int64:
		return strconv.FormatInt(value, 10)
	case float64:
		if value == math.Trunc(value) && math.Abs(value) < 1e15 {
			return strconv.FormatFloat(value, 'f', -1, 64)
		}
		return strconv.FormatFloat(value, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(value)
	default:
		encoded, err := v.MarshalJSON()
		if err != nil {
			return "<invalid>"
		}
		return string(encoded)
	}
}

// Get returns the object member for key, or the zero value (null) when
// the value is not an object or the key is missing.
func (v JSONValue) Get(key string) JSONValue {
	if object, ok := v.inner.(map[string]JSONValue); ok {
		return object[key]
	}
	return JSONValue{}
}

// Index returns the array element at i, or the zero value (null) when the
// value is not an array or i is out of range.
func (v JSONValue) Index(i int) JSONValue {
	if array, ok := v.inner.([]JSONValue); ok && i >= 0 && i < len(array) {
		return array[i]
	}
	return JSONValue{}
}

// Keys returns the object keys in sorted order, or nil for non-objects.
func (v JSONValue) Keys() []string {
	object, ok := v.inner.(map[string]JSONValue)
	if !ok {
		return nil
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// IsNull reports whether the value is JSON null (or never decoded).
func (v JSONValue) IsNull() bool { return v.inner == nil }

// AsString returns the string value.
func (v JSONValue) AsString() (string, bool) {
	value, ok := v.inner.(string)
	return value, ok
}

// AsInt64 returns the integer value; floats only when they are integral.
func (v JSONValue) AsInt64() (int64, bool) {
	switch value := v.inner.(type) {
	case int64:
		return value, true
	case float64:
		if value == math.Trunc(value) {
			return int64(value), true
		}
	}
	return 0, false
}

// AsFloat64 returns the numeric value.
func (v JSONValue) AsFloat64() (float64, bool) {
	switch value := v.inner.(type) {
	case int64:
		return float64(value), true
	case float64:
		return value, true
	}
	return 0, false
}

// AsBool returns the boolean value.
func (v JSONValue) AsBool() (bool, bool) {
	value, ok := v.inner.(bool)
	return value, ok
}

// AsArray returns the array elements.
func (v JSONValue) AsArray() ([]JSONValue, bool) {
	array, ok := v.inner.([]JSONValue)
	return array, ok
}

// AsObject returns the object members.
func (v JSONValue) AsObject() (map[string]JSONValue, bool) {
	object, ok := v.inner.(map[string]JSONValue)
	return object, ok
}
