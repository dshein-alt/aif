package itest

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/dshein-alt/aif/internal/config"
	"github.com/dshein-alt/aif/internal/harness"
)

// TestConnectRoute: /connect/ serves the aif-connect binaries from AIF_CONNECT_DIR behind the agent
// token, with a `name size sha256` index whose hashes come from SHA256SUMS.
func TestConnectRoute(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{"aif-connect-linux-amd64": "linux-bits", "aif-connect-windows-amd64.exe": "windows-bits!"}
	sums := "aaaa  aif-connect-linux-amd64\nbbbb  aif-connect-windows-amd64.exe\n"
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(sums), 0o644); err != nil {
		t.Fatal(err)
	}
	r := harness.NewWith(t, false, func(c *config.Config) { c.ConnectDir = dir })
	bob := r.Join("bob")

	eq(t, r.Client("").Get("/connect/").Code, 401, "index without a token")
	eq(t, r.Client("").Get("/connect/aif-connect-linux-amd64").Code, 401, "file without a token")

	invite := r.Client(r.Issue(""))
	idxUnclaimed := invite.Get("/connect/")
	eq(t, idxUnclaimed.Code, 403, "index with an unclaimed invite")
	eqStr(t, errCode(idxUnclaimed), "claim_required", "index with an unclaimed invite err")
	eq(t, invite.Get("/connect/aif-connect-linux-amd64").Code, 403, "file with an unclaimed invite")

	idx := bob.Get("/connect/").MustOK()
	if ct := idx.Header.Get("content-type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("index content-type = %q", ct)
	}
	want := "aif-connect-linux-amd64 10 aaaa\naif-connect-windows-amd64.exe 13 bbbb\n"
	eqStr(t, idx.Text(), want, "index body")

	f := bob.Get("/connect/aif-connect-windows-amd64.exe").MustOK()
	eqStr(t, f.Text(), "windows-bits!", "file body")
	eqStr(t, f.Header.Get("content-length"), strconv.Itoa(len("windows-bits!")), "file length")
	eqStr(t, bob.Get("/connect/SHA256SUMS").MustOK().Text(), sums, "sums file")

	// HEAD probes (curl -I, installers) get the same headers as GET with no body.
	hIdx := bob.Do("HEAD", "/connect/", nil).MustOK()
	eqStr(t, hIdx.Text(), "", "HEAD index body")
	eqStr(t, hIdx.Header.Get("content-type"), idx.Header.Get("content-type"), "HEAD index content-type")
	hFile := bob.Do("HEAD", "/connect/aif-connect-windows-amd64.exe", nil).MustOK()
	eqStr(t, hFile.Text(), "", "HEAD file body")
	eqStr(t, hFile.Header.Get("content-length"), strconv.Itoa(len("windows-bits!")), "HEAD file length")

	for _, p := range []string{"/connect/../x", "/connect/a/b", "/connect/..", "/connect/a%5Cb", "/connect/nope"} {
		eq(t, bob.Get(p).Code, 404, p)
	}
}

// TestConnectRouteDisabled: with no connect directory the route is a 404 and startup says so once.
func TestConnectRouteDisabled(t *testing.T) {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	r := harness.NewWith(t, false, func(c *config.Config) { c.ConnectDir = "" })
	bob := r.Join("bob")
	eq(t, bob.Get("/connect/").Code, 404, "index when disabled")
	eq(t, bob.Get("/connect/aif-connect-linux-amd64").Code, 404, "file when disabled")
	eq(t, strings.Count(buf.String(), "connector downloads disabled"), 1, "startup log lines:\n"+buf.String())
}
