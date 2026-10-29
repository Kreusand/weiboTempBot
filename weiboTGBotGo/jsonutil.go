package main

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// Weibo responses are loosely typed, so they are decoded into generic values and read through
// these helpers. Numbers stay json.Number so that 16-digit status ids keep every digit.

type obj = map[string]any

func decodeJSON(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

func asObj(value any) obj {
	result, _ := value.(map[string]any)
	return result
}

func asList(value any) []any {
	result, _ := value.([]any)
	return result
}

func asStr(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case json.Number:
		return typed.String()
	case bool:
		return strconv.FormatBool(typed)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	}
	return ""
}

// truthy follows the rules the response format relies on: null, false, 0, "" and empty containers are false.
func truthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return typed != ""
	case json.Number:
		parsed, err := typed.Float64()
		return err != nil || parsed != 0
	case float64:
		return typed != 0
	case []any:
		return len(typed) > 0
	case map[string]any:
		return len(typed) > 0
	}
	return true
}

// firstTruthy returns the first truthy value, or nil.
func firstTruthy(values ...any) any {
	for _, value := range values {
		if truthy(value) {
			return value
		}
	}
	return nil
}

// firstStr is the string form of the first truthy value.
func firstStr(values ...any) string {
	return asStr(firstTruthy(values...))
}

// isZeroish reports whether a value is null, false, 0 or "0".
func isZeroish(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case bool:
		return !typed
	case string:
		return typed == "0"
	case json.Number:
		parsed, err := typed.Float64()
		return err == nil && parsed == 0
	case float64:
		return typed == 0
	}
	return false
}

func asInt(value any) int {
	switch typed := value.(type) {
	case json.Number:
		if parsed, err := typed.Int64(); err == nil {
			return int(parsed)
		}
		if parsed, err := typed.Float64(); err == nil {
			return int(parsed)
		}
	case string:
		if parsed, err := strconv.Atoi(strings.TrimSpace(typed)); err == nil {
			return parsed
		}
	case float64:
		return int(typed)
	case bool:
		if typed {
			return 1
		}
	}
	return 0
}

func isDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}
