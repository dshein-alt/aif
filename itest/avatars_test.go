package itest

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"testing"

	"aif/internal/avatar"
	"aif/internal/config"
	"aif/internal/harness"
)

// --- pure (no DB) unit tests for the generator / validator ---

// TestAvatarPNGIsDeterministicValidAndSymmetric checks the generator contract from docs/ROADMAP.md
// Appendix A without a database: same name => identical bytes, the output is a real 128x128 PNG,
// and the image is mirrored left<->right about its vertical centre.
func TestAvatarPNGIsDeterministicValidAndSymmetric(t *testing.T) {
	a := avatar.PNG("scout")
	b := avatar.PNG("scout")
	if !bytes.Equal(a, b) {
		t.Fatal("PNG(\"scout\") is not deterministic across calls")
	}
	c := avatar.PNG("someone-else")
	if bytes.Equal(a, c) {
		t.Fatal("different names produced the same default avatar")
	}

	mime, err := avatar.Validate(a)
	if err != nil {
		t.Fatalf("generated default failed Validate: %v", err)
	}
	eqStr(t, mime, avatar.MIMEPNG, "generated default MIME")

	img, err := png.Decode(bytes.NewReader(a))
	if err != nil {
		t.Fatalf("generated default is not a decodable PNG: %v", err)
	}
	if bd := img.Bounds(); bd.Dx() != avatar.Size || bd.Dy() != avatar.Size {
		t.Fatalf("generated default is %dx%d, want %dx%d", bd.Dx(), bd.Dy(), avatar.Size, avatar.Size)
	}

	// Mirroring: for every pixel, the horizontally-mirrored pixel must match.
	bd := img.Bounds()
	w, h := bd.Dx(), bd.Dy()
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if img.At(x, y) != img.At(w-1-x, y) {
				t.Fatalf("avatar is not left/right symmetric at (%d,%d)", x, y)
			}
		}
	}
}

func TestAvatarValidateRejectsBadImages(t *testing.T) {
	if _, err := avatar.Validate(nil); err == nil {
		t.Fatal("Validate(nil) should fail")
	}
	if _, err := avatar.Validate([]byte("not an image at all")); err == nil {
		t.Fatal("Validate(non-image) should fail")
	}
	if _, err := avatar.Validate(pngOf(t, 64, 64)); err == nil {
		t.Fatal("Validate(wrong size) should fail")
	}
	for _, dims := range [][2]int{{128, 128}, {128, 128}} {
		if _, err := avatar.Validate(pngOf(t, dims[0], dims[1])); err != nil {
			t.Fatalf("Validate(valid 128x128) failed: %v", err)
		}
	}
}

// --- end-to-end (real server + Postgres) ---

func TestAvatarDefaultIsServedAndDeterministic(t *testing.T) {
	r := harness.New(t, false)
	r.Join("scout")

	first := r.Admin.Get("/api/avatar/scout")
	first.MustOK()
	eqStr(t, first.Header.Get("Content-Type"), "image/png", "default content-type")
	if _, err := png.Decode(bytes.NewReader(first.Body)); err != nil {
		t.Fatalf("served default is not a valid PNG: %v", err)
	}

	second := r.Admin.Get("/api/avatar/scout")
	second.MustOK()
	if !bytes.Equal(first.Body, second.Body) {
		t.Fatal("default avatar is not deterministic for the same agent")
	}

	// The default must equal the pure generator output for the same name.
	if !bytes.Equal(first.Body, avatar.PNG("scout")) {
		t.Fatal("served default does not match avatar.PNG(name)")
	}

	// Name lookup is case-insensitive (matches the /api/who resolution).
	ci := r.Admin.Get("/api/avatar/SCOUT")
	ci.MustOK()
	if !bytes.Equal(ci.Body, first.Body) {
		t.Fatal("case-insensitive avatar lookup differs")
	}

	// A request for an unknown agent is a 404.
	missing := r.Admin.Get("/api/avatar/nobody-here")
	eq(t, missing.Code, 404, "unknown agent avatar status")
}

func TestAvatarSetClearAndPerAgent(t *testing.T) {
	r := harness.New(t, false)
	alice := r.Join("alice")
	r.Join("bob")

	def := r.Admin.Get("/api/avatar/alice")
	def.MustOK()

	custom := pngOf(t, 128, 128)
	b64 := base64.StdEncoding.EncodeToString(custom)

	set := alice.Op("avatar", map[string]any{"b64": b64})
	set.MustOK()
	eqStr(t, set.Str("mime"), "image/png", "op reports detected mime")
	eq(t, int64f(set.Field("bytes")), int64(len(custom)), "op reports stored byte count")

	got := r.Admin.Get("/api/avatar/alice")
	got.MustOK()
	eqStr(t, got.Header.Get("Content-Type"), "image/png", "custom content-type")
	if !bytes.Equal(got.Body, custom) {
		t.Fatal("served custom avatar does not match what was uploaded")
	}

	// Another agent keeps its own default (avatars are per-agent).
	other := r.Admin.Get("/api/avatar/bob")
	other.MustOK()
	if bytes.Equal(other.Body, custom) {
		t.Fatal("bob's avatar was clobbered by alice's upload")
	}

	// Re-setting replaces the blob (ON CONFLICT DO UPDATE).
	custom2 := pngOf(t, 128, 128)
	if bytes.Equal(custom2, custom) {
		custom2 = pngOf(t, 128, 128)
	}
	alice.Op("avatar", map[string]any{"b64": base64.StdEncoding.EncodeToString(custom2)}).MustOK()
	got2 := r.Admin.Get("/api/avatar/alice")
	got2.MustOK()
	if !bytes.Equal(got2.Body, custom2) {
		t.Fatal("re-setting a custom avatar did not replace the stored blob")
	}

	// clear=1 reverts to the deterministic default.
	clr := alice.Op("avatar", map[string]any{"clear": true})
	clr.MustOK()
	eq(t, int64f(clr.Field("cleared")), 1, "clear flag echoed")

	after := r.Admin.Get("/api/avatar/alice")
	after.MustOK()
	if !bytes.Equal(after.Body, def.Body) {
		t.Fatal("after clear, the served avatar is not the deterministic default")
	}
}

func TestAvatarValidationAndLimits(t *testing.T) {
	r := harness.New(t, false)
	bot := r.Join("bot")

	// Wrong dimensions -> 400 bad_avatar.
	bad := bot.Op("avatar", map[string]any{"b64": base64.StdEncoding.EncodeToString(pngOf(t, 64, 64))})
	eq(t, bad.Code, 400, "wrong-size avatar status")
	eqStr(t, errCode(bad), "bad_avatar", "wrong-size error code")

	// Not an image -> 400 bad_avatar.
	notImg := bot.Op("avatar", map[string]any{"b64": base64.StdEncoding.EncodeToString([]byte("just some text, not bytes of an image"))})
	eq(t, notImg.Code, 400, "non-image avatar status")
	eqStr(t, errCode(notImg), "bad_avatar", "non-image error code")

	// No image at all -> 400 need_image.
	none := bot.Op("avatar", map[string]any{})
	eq(t, none.Code, 400, "no-image avatar status")
	eqStr(t, errCode(none), "need_image", "no-image error code")

	// Data-URL prefix is tolerated and the image still validates.
	daturl := "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngOf(t, 128, 128))
	bot.Op("avatar", map[string]any{"b64": daturl}).MustOK()

	// Over the size cap -> 413 avatar_too_large. Uses a tiny cap and a noisy 128x128 PNG.
	r2 := harness.NewWith(t, false, func(c *config.Config) { c.AvatarMaxSize = 4096 })
	bot2 := r2.Join("bot")
	tooBig := bot2.Op("avatar", map[string]any{"b64": base64.StdEncoding.EncodeToString(noisyPNG(t, 128, 128))})
	eq(t, tooBig.Code, 413, "oversized avatar status")
	eqStr(t, errCode(tooBig), "avatar_too_large", "oversized error code")
}

// --- helpers ---

// pngOf encodes a w x h PNG with a cheap gradient so PNG output is small and deterministic.
func pngOf(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: uint8((x * y) % 256), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// noisyPNG produces a poorly-compressing PNG (pseudo-random pixels) so it reliably exceeds a small
// size cap while still being a valid 128x128 image.
func noisyPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	state := uint32(0x12345678)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			state = state*1103515245 + 12345
			img.SetRGBA(x, y, color.RGBA{R: uint8(state >> 16), G: uint8(state >> 8), B: uint8(state), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode noisy png: %v", err)
	}
	return buf.Bytes()
}
