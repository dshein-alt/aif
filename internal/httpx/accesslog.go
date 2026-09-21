package httpx

// Access logging: one line per request (method, path, status, size, duration,
// client IP, agent, op). The agent and op are filled in during the request by
// checkToken / the op handlers through a *reqInfo planted in the context —
// re-resolving identity here would cost a second token lookup per request.
//
// Disabled with AIF_ACCESS_LOG=0. /healthz is skipped (probe noise).

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"
)

// reqInfo rides the request context; handlers annotate it, the middleware logs it.
type reqInfo struct {
	agent string
	op    string
}

type ctxKey int

const ctxReqInfo ctxKey = 1

// noteAgent records the authenticated agent for the access log line.
func noteAgent(req *http.Request, me string) {
	if ri, ok := req.Context().Value(ctxReqInfo).(*reqInfo); ok && me != "" {
		ri.agent = me
	}
}

// noteOp records the op name (the path alone only ever says /api/op or /mcp).
func noteOp(req *http.Request, op string) {
	if ri, ok := req.Context().Value(ctxReqInfo).(*reqInfo); ok && op != "" {
		ri.op = op
	}
}

// statusWriter remembers the status code and counts response bytes.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Flush delegates so SSE and long-poll streaming keep working through the wrapper.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// accessLog logs one line per request: POST /api/op 200 142B 3.2ms ip=… agent=David op=post
func (a *App) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !a.cfg.AccessLog || req.URL.Path == "/healthz" {
			next.ServeHTTP(w, req)
			return
		}
		ri := &reqInfo{}
		req = req.WithContext(context.WithValue(req.Context(), ctxReqInfo, ri))
		rec := &statusWriter{ResponseWriter: w, status: 200}
		start := time.Now()
		next.ServeHTTP(rec, req)
		var extra strings.Builder
		if ri.agent != "" {
			extra.WriteString(" agent=" + ri.agent)
		}
		if ri.op != "" {
			extra.WriteString(" op=" + ri.op)
		}
		log.Printf("%s %s %d %dB %.1fms ip=%s%s",
			req.Method, req.URL.RequestURI(), rec.status, rec.bytes,
			float64(time.Since(start).Microseconds())/1000.0, clientIP(req), extra.String())
	})
}

func clientIP(req *http.Request) string {
	host := req.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return host
}
