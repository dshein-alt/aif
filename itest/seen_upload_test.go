package itest

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"testing"

	"github.com/dshein-alt/aif/internal/harness"
)

// multipartRaw posts an arbitrary multipart form (value fields and/or files) to a URL as a bearer.
func multipartRaw(t *testing.T, url, bearer string, values map[string]string, files [][2]string) *harness.Resp {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range values {
		_ = mw.WriteField(k, v)
	}
	for _, f := range files {
		part, err := mw.CreateFormFile("files", f[0])
		if err != nil {
			t.Fatal(err)
		}
		_, _ = part.Write([]byte(f[1]))
	}
	_ = mw.Close()
	req, _ := http.NewRequest("POST", url, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return &harness.Resp{Code: res.StatusCode, Header: res.Header, Body: body}
}

// TestSeenOpModes exercises each mode of the seen op: global cursor (seq), per-thread mark (t and an
// explicit read value), mark-everything (all), and the no_thread guard for an unknown thread.
func TestSeenOpModes(t *testing.T) {
	r := harness.New(t, false)
	alice := r.Join("alice")
	tid := int64f(alice.Op("post", map[string]any{"subject": "Seen modes", "b": "one"}).MustOK().Field("t"))
	mid := int64f(alice.Op("post", map[string]any{"t": tid, "b": "two"}).MustOK().Field("i"))

	if out := alice.Op("seen", map[string]any{"seq": 0}).MustOK(); out.Field("cursor") == nil {
		t.Error("seen {seq:0} returned no cursor")
	}
	if out := alice.Op("seen", map[string]any{"t": tid}).MustOK(); out.Field("thread") == nil {
		t.Error("seen {t} returned no thread mark")
	}
	if out := alice.Op("seen", map[string]any{"t": tid, "read": mid}).MustOK().Field("thread"); out == nil {
		t.Error("seen {t,read} returned no thread mark")
	}
	if out := alice.Op("seen", map[string]any{"all": 1}).MustOK(); out.Field("all") != float64(1) {
		t.Error("seen {all:1} did not report all=1")
	}
	if g := alice.Op("seen", map[string]any{"t": 999999}); g.Code != 404 || g.Str("err") != "no_thread" {
		t.Errorf("seen unknown thread = %d %s, want 404 no_thread", g.Code, g.Text())
	}
}

// TestFilesUploadPayloadAndErrors covers the multipart upload paths beyond the plain "files" field:
// the inline JSON "payload" field, the no-files rejection, and the malformed-payload rejection.
func TestFilesUploadPayloadAndErrors(t *testing.T) {
	r := harness.New(t, false)
	r.Join("alice")
	tok := r.Tokens["alice"]

	// inline files carried as a JSON payload field
	ok := multipartRaw(t, r.URL("/api/files"), tok,
		map[string]string{"payload": `{"files":[{"n":"x.txt","text":"inline via multipart"}]}`}, nil).MustOK()
	if list, _ := ok.Field("u").([]any); len(list) == 0 || list[0].(map[string]any)["n"] != "x.txt" {
		t.Errorf("payload upload = %s, want one item named x.txt", ok.Text())
	}

	// a multipart body with neither files nor a payload is refused
	if g := multipartRaw(t, r.URL("/api/files"), tok, map[string]string{"stray": "value"}, nil); g.Code != 400 || g.Str("err") != "bad_request" || g.Str("msg") != "no files sent" {
		t.Errorf("empty upload = %d %s, want 400 no files", g.Code, g.Text())
	}

	// a payload that is not valid JSON is refused
	if g := multipartRaw(t, r.URL("/api/files"), tok, map[string]string{"payload": "not-json"}, nil); g.Code != 400 {
		t.Errorf("bad payload = %d %s, want 400", g.Code, g.Text())
	}
}
