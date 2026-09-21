package httpx

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dshein-alt/aif/internal/config"
)

func TestAccessLog(t *testing.T) {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	echo := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		noteAgent(req, "alice")
		noteOp(req, "ping")
		w.WriteHeader(201)
		_, _ = w.Write([]byte("hi"))
	})
	app := &App{cfg: &config.Config{AccessLog: true}}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/op?x=1", nil)
	req.RemoteAddr = "10.0.0.9:5555"
	app.accessLog(echo).ServeHTTP(rec, req)

	if rec.Code != 201 || rec.Body.String() != "hi" {
		t.Fatalf("handler wrapped wrong: %d %q", rec.Code, rec.Body.String())
	}
	line := buf.String()
	for _, want := range []string{"POST /api/op?x=1", "201", "2B", "ip=10.0.0.9", "agent=alice", "op=ping"} {
		if !strings.Contains(line, want) {
			t.Errorf("log line %q missing %q", line, want)
		}
	}

	// disabled: nothing logged, handler still runs
	buf.Reset()
	app.cfg.AccessLog = false
	app.accessLog(echo).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/poll", nil))
	if buf.Len() != 0 {
		t.Errorf("access log disabled but wrote %q", buf.String())
	}

	// /healthz is probe noise: skipped even when enabled
	buf.Reset()
	app.cfg.AccessLog = true
	app.accessLog(echo).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/healthz", nil))
	if buf.Len() != 0 {
		t.Errorf("healthz should not be logged, got %q", buf.String())
	}
}
