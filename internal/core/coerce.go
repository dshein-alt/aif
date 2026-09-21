package core

import (
	"fmt"
	"strconv"
)

// Type coercion for JSON-decoded arguments (numbers arrive as float64; transports may also pass
// strings from query params), mirroring the int()/bool coercion applied when normalising op args.

func toInt64(v any) (int64, bool) {
	switch t := v.(type) {
	case nil:
		return 0, false
	case int64:
		return t, true
	case int32:
		return int64(t), true
	case int:
		return int64(t), true
	case float64:
		return int64(t), true
	case float32:
		return int64(t), true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case string:
		n, err := strconv.ParseInt(t, 10, 64)
		if err != nil {
			f, ferr := strconv.ParseFloat(t, 64)
			if ferr != nil {
				return 0, false
			}
			return int64(f), true
		}
		return n, true
	}
	return 0, false
}

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case nil:
		return 0, false
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int64:
		return float64(t), true
	case int:
		return float64(t), true
	case string:
		f, err := strconv.ParseFloat(t, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

func toBool(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case int64:
		return t != 0
	case int:
		return t != 0
	case float64:
		return t != 0
	case string:
		switch t {
		case "1", "true", "yes", "on":
			return true
		}
	}
	return false
}

func boolOr(v any) bool { return toBool(v) }

func toI64(v any) int64 { n, _ := toInt64(v); return n }

func mustI64(s string) int64 { n, _ := strconv.ParseInt(s, 10, 64); return n }

// mapStr reads a string out of a value that may be a raw string or a {key:string} row.
func mapStr(v any, key string) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	case map[string]any:
		if s, ok := t[key]; ok {
			return fmt.Sprint(s)
		}
		return ""
	}
	return fmt.Sprint(v)
}

// int64Default returns the integer value at m[key] (0 when absent or not a number).
func int64Default(m map[string]any, key string) int64 {
	n, _ := toInt64(m[key])
	return n
}
