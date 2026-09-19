package itest

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"aif/internal/harness"
)

func itoaF(f float64) string { return strconv.FormatInt(int64(f), 10) }

// uploadPost sends a multipart POST /api/files with the given file field(s), as the bearer token.
func uploadPost(t *testing.T, url, bearer string, files [][2]string) *harness.Resp {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, f := range files {
		part, err := mw.CreateFormFile("files", f[0])
		if err != nil {
			t.Fatal(err)
		}
		_, _ = part.Write([]byte(f[1]))
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST", url, &buf)
	if err != nil {
		t.Fatal(err)
	}
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

func fileID(t *testing.T, alice *harness.Client, mid float64) float64 {
	t.Helper()
	msg := alice.Get("/api/messages/" + itoaF(mid)).MustOK()
	fl, ok := msg.Field("fl").([]any)
	if !ok || len(fl) == 0 {
		t.Fatalf("message %v has no attached files: %s", mid, msg.Text())
	}
	return fl[0].(map[string]any)["i"].(float64)
}

func TestFilesUploadAttachRawDelete(t *testing.T) {
	r := harness.New(t, false)
	alice := r.Join("alice")
	tok := r.Tokens["alice"]

	// --- multipart upload (handleFilesUpload -> CreateUploads) ---
	up := uploadPost(t, r.URL("/api/files"), tok, [][2]string{{"hello.txt", "hello world"}}).MustOK()
	list, ok := up.Field("u").([]any)
	if !ok || len(list) == 0 {
		t.Fatalf("upload returned no items: %s", up.Text())
	}
	item := list[0].(map[string]any)
	key, _ := item["k"].(string)
	if key == "" || item["n"] != "hello.txt" || item["s"].(float64) != 11 {
		t.Fatalf("upload meta wrong: %v", item)
	}

	// unauthenticated upload is refused
	if noauth := uploadPost(t, r.URL("/api/files"), "", [][2]string{{"x.txt", "y"}}); noauth.Code != 401 {
		t.Errorf("unauthenticated upload = %d, want 401", noauth.Code)
	}

	// --- attach the pending upload to a message (op post with files=[{k}]) ---
	post := alice.Op("post", map[string]any{"subject": "File thread", "b": "see attachment", "files": []any{map[string]any{"k": key}}}).MustOK()
	mid := post.Field("i").(float64)
	fid := fileID(t, alice, mid)

	// --- op dl via REST meta (handleFileMeta) ---
	meta := alice.Get("/api/files/" + itoaF(fid) + "?text=1").MustOK()
	if meta.Field("i").(float64) != fid || meta.Str("n") != "hello.txt" || meta.Str("text") != "hello world" {
		t.Errorf("file meta wrong: %s", meta.Text())
	}

	// --- raw download (handleFileRaw): bytes + download headers ---
	raw := alice.Get("/api/files/" + itoaF(fid) + "/raw").MustOK()
	if raw.Text() != "hello world" {
		t.Errorf("raw body = %q", raw.Text())
	}
	if !strings.Contains(raw.Header.Get("Content-Disposition"), "attachment") {
		t.Errorf("raw missing attachment disposition: %q", raw.Header.Get("Content-Disposition"))
	}
	// .txt is not in the detect map so it stays octet-stream; the sha must be the real digest.
	if raw.Header.Get("X-Sha256") != "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9" {
		t.Errorf("raw X-Sha256 = %q, want sha256('hello world')", raw.Header.Get("X-Sha256"))
	}
	if raw.Header.Get("Content-Type") == "" {
		t.Errorf("raw missing content type")
	}

	// --- op up (JSON path) then attach, then delete the file from the message ---
	upOp := alice.Op("up", map[string]any{"name": "extra.txt", "text": "extra data"}).MustOK()
	upKey, _ := upOp.Field("k").(string)
	if upKey == "" {
		t.Fatalf("op up returned no key: %s", upOp.Text())
	}
	mid2 := alice.Op("post", map[string]any{"b": "another file", "t": post.Field("t"), "files": []any{map[string]any{"k": upKey}}}).MustOK().Field("i").(float64)
	_ = fileID(t, alice, mid2)

	if del := alice.Delete("/api/messages/" + itoaF(mid2) + "/files/extra.txt").MustOK(); del.JSON() == nil {
		t.Error("file delete returned no body")
	}

	// --- unknown file id is a 404 no_file on the raw route ---
	if gone := alice.Get("/api/files/99999999/raw"); gone.Code != 404 || gone.Str("err") != "no_file" {
		t.Errorf("unknown raw file = %d %s, want 404 no_file", gone.Code, gone.Text())
	}
}
