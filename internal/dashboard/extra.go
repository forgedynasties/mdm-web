package dashboard

import (
	"encoding/json"
	"fmt"
	_ "image/gif"
	_ "image/jpeg"
	"strconv"
	"strings"
)

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
	if err := json.Unmarshal(v, &s); err != nil {
		// try number
		return strings.Trim(string(v), "\"")
	}
	// Android getSSID() wraps SSID in quotes — strip them
	s = strings.Trim(s, "\"")
	return s
}

func extraFloat(raw json.RawMessage, key string) string {
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
	var f float64
	if err := json.Unmarshal(v, &f); err != nil {
		return ""
	}
	return fmt.Sprintf("%.1f", f)
}

func extraInt(raw json.RawMessage, key string) string {
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
	var n int
	if err := json.Unmarshal(v, &n); err != nil {
		return ""
	}
	return strconv.Itoa(n)
}

// extraNested reads a field one object deep (extra.host_app.package). "" when any
// step is missing — a device that never reported it, or an older client.
func extraNested(raw json.RawMessage, obj, key string) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	inner, ok := m[obj]
	if !ok {
		return ""
	}
	var im map[string]any
	if err := json.Unmarshal(inner, &im); err != nil {
		return ""
	}
	sv, _ := im[key].(string)
	return sv
}

// extraNestedInt64 reads a number one object deep (extra.host_app.version_code).
func extraNestedInt64(raw json.RawMessage, obj, key string) int64 {
	if len(raw) == 0 {
		return 0
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return 0
	}
	inner, ok := m[obj]
	if !ok {
		return 0
	}
	var im map[string]json.RawMessage
	if err := json.Unmarshal(inner, &im); err != nil {
		return 0
	}
	v, ok := im[key]
	if !ok {
		return 0
	}
	var n int64
	if err := json.Unmarshal(v, &n); err != nil {
		return 0
	}
	return n
}

// extraInt64 reads a numeric check-in field as a number rather than a string, for
// comparisons (agent_version_code). 0 when absent or unparsable — which reads as
// "older than anything", the safe answer for a device that predates the field.
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
	if err := json.Unmarshal(v, &n); err != nil {
		return 0
	}
	return n
}

// extraBoolAsInt reads a boolean check-in field ("charging") as "1"/"0" rather than
// "true"/"false", matching wlc_status/battery_pct's plain-numeric CSV convention.
func extraBoolAsInt(raw json.RawMessage, key string) string {
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
	var b bool
	if err := json.Unmarshal(v, &b); err != nil {
		return ""
	}
	if b {
		return "1"
	}
	return "0"
}

func extraRamField(raw json.RawMessage, field string) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	v, ok := m["ram_usage_mb"]
	if !ok {
		return ""
	}
	var ram map[string]int
	if err := json.Unmarshal(v, &ram); err != nil {
		return ""
	}
	val, ok := ram[field]
	if !ok {
		return ""
	}
	return strconv.Itoa(val)
}
