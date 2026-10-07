package api

import "encoding/json"

// extraString reads a string check-in field from latest_extra, "" when absent or not a string.
func extraString(raw json.RawMessage, key string) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(v, &s) != nil {
		return ""
	}
	return s
}

// extraBoolField reads a boolean check-in field, false when absent or not a boolean.
func extraBoolField(raw json.RawMessage, key string) bool {
	if len(raw) == 0 {
		return false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	v, ok := m[key]
	if !ok {
		return false
	}
	var b bool
	return json.Unmarshal(v, &b) == nil && b
}

// extraInt64 reads a numeric check-in field as a number, 0 when absent or unparsable.
func extraInt64(raw json.RawMessage, key string) int64 {
	if len(raw) == 0 {
		return 0
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return 0
	}
	v, ok := m[key]
	if !ok {
		return 0
	}
	var n int64
	if json.Unmarshal(v, &n) != nil {
		return 0
	}
	return n
}

// extractBatteryTempC reads battery_temp_c as a float64, ok=false when absent or unparsable.
func extractBatteryTempC(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return 0, false
	}
	v, ok := m["battery_temp_c"]
	if !ok {
		return 0, false
	}
	var temp float64
	if json.Unmarshal(v, &temp) != nil {
		return 0, false
	}
	return temp, true
}
