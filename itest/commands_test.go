package itest

import (
	"fmt"
	"testing"
	"time"

	"github.com/dshein-alt/aif/internal/harness"
)

func TestConnectorCommandQueue(t *testing.T) {
	r := harness.New(t, false)
	issuer := r.Join("issuer")
	resident := r.Join("resident")
	stranger := r.Join("stranger")
	missing := issuer.Post("/api/commands", map[string]any{"target": "not_registered", "command": "RESET"})
	if missing.Code != 404 || missing.Field("err") != "unknown_agent" {
		t.Fatalf("unregistered target: %d %s", missing.Code, missing.Text())
	}
	made := issuer.Post("/api/commands", map[string]any{"target": "resident", "command": "RESET", "issuer": "stranger"}).MustOK()
	id := int64f(made.Field("id"))
	if made.Field("issuer") != "issuer" {
		t.Fatalf("issuer spoofed: %v", made.JSON())
	}
	path := fmt.Sprintf("/api/commands/%d", id)
	if got := stranger.Get(path); got.Code != 404 {
		t.Fatalf("stranger read command: %d %s", got.Code, got.Text())
	}
	if got := stranger.Post(path+"/ack", map[string]any{"status": "accepted", "expires": 1800}); got.Code != 404 {
		t.Fatalf("stranger ack: %d %s", got.Code, got.Text())
	}
	next := resident.Get("/api/commands").MustOK().Field("command").(map[string]any)
	if next["name"] != "RESET" || next["issuer"] != "issuer" {
		t.Fatalf("next: %v", next)
	}
	if next["createdAt"] == nil {
		t.Fatalf("missing createdAt: %v", next)
	}
	ack := resident.Post(path+"/ack", map[string]any{"status": "accepted", "expires": 1800}).MustOK()
	if ack.Field("status") != "accepted" || issuer.Get(path).Field("status") != "accepted" {
		t.Fatalf("ack: %v", ack.JSON())
	}
	if got := resident.Get("/api/commands").Field("command"); got != nil {
		t.Fatalf("accepted command redelivered: %v", got)
	}

	old := issuer.Post("/api/commands", map[string]any{"target": "resident", "command": "KILL"}).MustOK()
	oldID := int64f(old.Field("id"))
	time.Sleep(2 * time.Second)
	expired := resident.Post(fmt.Sprintf("/api/commands/%d/ack", oldID), map[string]any{"status": "accepted", "expires": 1}).MustOK()
	if expired.Field("status") != "rejected" || expired.Field("reason") != "command_expired" {
		t.Fatalf("expired: %v", expired.JSON())
	}
}
