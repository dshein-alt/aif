package itest

import (
	"bytes"
	"context"
	"os"
	"testing"

	"aif/internal/avatar"
	"aif/internal/config"
	"aif/internal/core"
	"aif/internal/db"
	"aif/internal/harness"
	"aif/internal/seed"
)

func rootRig(t *testing.T) (*harness.Rig, []byte) {
	t.Helper()
	r := harness.NewWith(t, false, func(c *config.Config) { c.AssetsDir = "../assets" })
	b, err := os.ReadFile("../assets/the_root.png")
	if err != nil {
		t.Fatalf("read shipped avatar: %v", err)
	}
	return r, b
}

// The founder's shipped avatar (assets/the_root.png) is applied into the bootstrap: EnsureRoot
// stores it so TheRoot shows its canonical portrait instead of the generated identicon default.
func TestEnsureRootSeedsFounderAvatar(t *testing.T) {
	r, want := rootRig(t)
	ctx := context.Background()

	if _, _, err := seed.EnsureRootOnPool(ctx, r.Pool(), r.Cfg); err != nil {
		t.Fatalf("EnsureRoot: %v", err)
	}

	row, err := db.QueryOne(ctx, r.Pool(), "SELECT mime, data FROM avatars WHERE name = ?", config.RootName)
	if err != nil || row == nil {
		t.Fatalf("expected a founder avatar row, got row=%v err=%v", row, err)
	}
	if got := db.AsString(row, "mime"); got != "image/png" {
		t.Errorf("avatar mime = %q, want image/png", got)
	}
	if b, _ := row["data"].([]byte); !bytes.Equal(b, want) {
		t.Errorf("stored avatar bytes differ from the shipped asset (%d vs %d)", len(b), len(want))
	}

	// The same bytes are what the serving path returns (not the generated default).
	mime, data, err := core.Avatar(ctx, r.Pool(), config.RootName)
	if err != nil {
		t.Fatalf("Avatar: %v", err)
	}
	if mime != "image/png" || !bytes.Equal(data, want) {
		t.Error("served avatar should be the shipped founder portrait, not the generated default")
	}
}

// Re-running the bootstrap is idempotent, and a later deliberate clear is respected (not silently
// re-applied on the next boot), after which the generated default is served again.
func TestFounderAvatarIdempotentAndRespectsClear(t *testing.T) {
	r, want := rootRig(t)
	ctx := context.Background()

	seed.EnsureRootOnPool(ctx, r.Pool(), r.Cfg)
	_, first, _ := core.Avatar(ctx, r.Pool(), config.RootName)

	// Second boot: unchanged (idempotent, no duplicate work).
	seed.EnsureRootOnPool(ctx, r.Pool(), r.Cfg)
	if _, again, _ := core.Avatar(ctx, r.Pool(), config.RootName); !bytes.Equal(again, first) {
		t.Error("re-running EnsureRoot must not alter the founder avatar")
	}

	// The operator clears the founder's avatar -> it falls back to the generated default, and a
	// subsequent bootstrap does NOT force the shipped image back (the meta gate records the seed).
	if _, err := db.Exec(ctx, r.Pool(), "DELETE FROM avatars WHERE name = ?", config.RootName); err != nil {
		t.Fatalf("clear avatar: %v", err)
	}
	seed.EnsureRootOnPool(ctx, r.Pool(), r.Cfg)
	mime, data, _ := core.Avatar(ctx, r.Pool(), config.RootName)
	if bytes.Equal(data, want) {
		t.Fatal("a deliberately cleared founder avatar must not be re-applied by the bootstrap")
	}
	if mime != avatar.MIMEPNG || !bytes.Equal(data, avatar.PNG(config.RootName)) {
		t.Error("after clearing, the generated default should be served")
	}
}

// If the asset is not present, the bootstrap must still succeed and simply keep the generated
// default (a missing avatar is not fatal).
func TestFounderAvatarMissingAssetIsNotFatal(t *testing.T) {
	r := harness.NewWith(t, false, func(c *config.Config) { c.AssetsDir = t.TempDir() }) // empty dir, no the_root.png
	ctx := context.Background()

	if _, _, err := seed.EnsureRootOnPool(ctx, r.Pool(), r.Cfg); err != nil {
		t.Fatalf("EnsureRoot with a missing avatar asset must not fail: %v", err)
	}
	if _, err := db.QueryOne(ctx, r.Pool(), "SELECT 1 FROM avatars WHERE name = ?", config.RootName); err != nil {
		t.Fatalf("query avatars: %v", err)
	}
	if _, data, _ := core.Avatar(ctx, r.Pool(), config.RootName); !bytes.Equal(data, avatar.PNG(config.RootName)) {
		t.Error("with no shipped asset, the generated default should be served")
	}
}
