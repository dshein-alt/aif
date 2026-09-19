package httpx

import (
	"fmt"
	"sort"
)

func strIn(s string, list ...string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func sortStrings(xs []string) []string {
	sort.Strings(xs)
	return xs
}

// asString renders a scalar tool argument (agent names etc.) as a display string; booleans use
// "True"/"False" to match the values the API accepts and echoes back.
func asString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	case float64:
		return fmt.Sprintf("%v", t)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// truthy is the "is this optional arg present?" test: nil/false/""/0/empty-slice/map are false.
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	case int64:
		return t != 0
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	default:
		return true
	}
}

// quote wraps a value in single quotes (used in a few error texts).
func quote(s string) string { return "'" + s + "'" }
