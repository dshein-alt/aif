package driver

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// The replay fake harness: when GO_FAKE_HARNESS names a transcript, the test binary replays it
// instead of running tests. TestMain intercepts before flag parsing, so the fake accepts any argv
// (a driver passes its harness flags, e.g. `--mode rpc`, straight to it).
//
// Transcript lines (an optional first line `dynamic: key1,key2,...`):
//
//	in: <json>        the next stdin line must deep-equal it as JSON once the values of dynamic
//	                  keys (at any depth) are "*" on both sides; non-JSON is compared verbatim.
//	                  A request's "id" is captured. Mismatch: `MISMATCH expected=… got=…` on
//	                  stderr, exit 99.
//	out: <json>       printed to stdout, with the literal $id replaced by the last captured id as
//	                  raw JSON (write `"id":$id`, unquoted)
//	exit: <code>      exit with that code
//	env: NAME         print the env var's value to stdout
//	ignore-sigterm:   ignore SIGTERM from here on (no-op on Windows)
//
// Blank lines and lines starting with # are skipped. At the end of the transcript the fake
// reads stdin until EOF, then exits 0.
func TestMain(m *testing.M) {
	if path := os.Getenv("GO_FAKE_HARNESS"); path != "" {
		os.Exit(runFake(path))
	}
	os.Exit(m.Run())
}

// fakeHarness points GO_FAKE_HARNESS at transcriptPath for this test (so the child inherits it)
// and returns the binary to use as Launch.Bin.
func fakeHarness(t *testing.T, transcriptPath string) string {
	t.Helper()
	t.Setenv("GO_FAKE_HARNESS", transcriptPath)
	// under -race the child would otherwise sleep 1 s at exit
	t.Setenv("GORACE", strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
	return os.Args[0]
}

func runFake(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 98
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	dynamic := map[string]bool{}
	if rest, ok := strings.CutPrefix(lines[0], "dynamic:"); ok {
		for _, k := range strings.Split(rest, ",") {
			dynamic[strings.TrimSpace(k)] = true
		}
		lines = lines[1:]
	}
	stdin := bufio.NewReader(os.Stdin)
	id := ""
	for _, l := range lines {
		switch {
		case l == "" || strings.HasPrefix(l, "#"):
		case strings.HasPrefix(l, "in: "):
			want := l[len("in: "):]
			got, err := stdin.ReadString('\n')
			got = strings.TrimRight(got, "\r\n")
			if err != nil && got == "" {
				got = "<EOF>"
			}
			if !fakeMatch(want, got, dynamic) {
				fmt.Fprintf(os.Stderr, "MISMATCH expected=%s got=%s\n", want, got)
				return 99
			}
			var req map[string]json.RawMessage
			if json.Unmarshal([]byte(got), &req) == nil && req["id"] != nil {
				id = string(req["id"])
			}
		case strings.HasPrefix(l, "out: "):
			fmt.Println(strings.ReplaceAll(l[len("out: "):], "$id", id))
		case strings.HasPrefix(l, "exit: "):
			code, _ := strconv.Atoi(strings.TrimSpace(l[len("exit: "):]))
			return code
		case strings.HasPrefix(l, "env: "):
			fmt.Println(os.Getenv(strings.TrimSpace(l[len("env: "):])))
		case l == "ignore-sigterm:":
			signal.Ignore(syscall.SIGTERM)
		default:
			fmt.Fprintf(os.Stderr, "bad transcript line: %s\n", l)
			return 98
		}
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	return 0
}

func fakeMatch(want, got string, dynamic map[string]bool) bool {
	var w, g any
	if json.Unmarshal([]byte(want), &w) != nil {
		return want == got
	}
	if json.Unmarshal([]byte(got), &g) != nil {
		return false
	}
	return reflect.DeepEqual(starDynamic(w, dynamic), starDynamic(g, dynamic))
}

func starDynamic(v any, dynamic map[string]bool) any {
	switch v := v.(type) {
	case map[string]any:
		for k, x := range v {
			if dynamic[k] {
				v[k] = "*"
			} else {
				v[k] = starDynamic(x, dynamic)
			}
		}
	case []any:
		for i, x := range v {
			v[i] = starDynamic(x, dynamic)
		}
	}
	return v
}
