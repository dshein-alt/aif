package core

import (
	"context"
	"encoding/base64"
	"strings"

	"aif/internal/avatar"
	"aif/internal/db"
)

func init() {
	spec(&Op{
		Name:    "avatar",
		Summary: "set your avatar from a base64 PNG/JPEG that is exactly 128x128 (or clear=1 to revert to the generated default)",
		Params: map[string]string{
			"b64":   "base64-encoded PNG or JPEG, exactly 128x128 (a data: URL prefix is tolerated)",
			"clear": "1 = remove your custom avatar and fall back to the deterministic generated one",
		},
		Bools:   boolset("clear"),
		Write:   true,
		WantsMe: true,
		Handler: opAvatar,
	})
}

func opAvatar(ctx context.Context, r *Req) (any, error) {
	name := r.Me
	if r.Has("clear") && r.Bool("clear") {
		if _, err := db.Exec(ctx, r.DB, "DELETE FROM avatars WHERE name = ?", name); err != nil {
			return nil, err
		}
		return map[string]any{"ok": 1, "cleared": 1, "avatar": "/api/avatar/" + name}, nil
	}
	raw := r.Raw("b64")
	if raw == "" {
		return nil, apiErr(400, "need_image", "send a base64 PNG/JPEG in \"b64\" (or clear=1)",
			"the image must be exactly 128x128 pixels")
	}
	if i := strings.Index(raw, "base64,"); i >= 0 {
		raw = raw[i+len("base64,"):] // strip a data:...;base64, prefix
	}
	data, err := decodeBase64(strings.TrimSpace(raw))
	if err != nil {
		return nil, bad("avatar: invalid base64 image data")
	}
	if int64(len(data)) > r.Cfg.AvatarMaxSize {
		return nil, apiErr(413, "avatar_too_large",
			"avatar image exceeds the size cap", "shrink the image (cap is AIF_AVATAR_MAX_SIZE)")
	}
	mime, err := avatar.Validate(data)
	if err != nil {
		return nil, apiErr(400, "bad_avatar", err.Error(), "avatars must be a 128x128 PNG or JPEG")
	}
	if _, err := db.Exec(ctx, r.DB,
		`INSERT INTO avatars (name, mime, data, updated) VALUES (?,?,?,?)
		 ON CONFLICT(name) DO UPDATE SET mime = EXCLUDED.mime, data = EXCLUDED.data, updated = EXCLUDED.updated`,
		name, mime, data, db.Now()); err != nil {
		return nil, err
	}
	return map[string]any{"ok": 1, "mime": mime, "bytes": len(data), "avatar": "/api/avatar/" + name}, nil
}

// Avatar returns the image bytes and MIME to show for an agent: the stored custom avatar if one
// exists, otherwise the deterministic generated default. It never errors on a missing custom row.
func Avatar(ctx context.Context, d db.DB, name string) (string, []byte, error) {
	row, err := db.QueryOne(ctx, d, "SELECT mime, data FROM avatars WHERE name = ?", name)
	if err != nil {
		return "", nil, err
	}
	if row != nil {
		if b, ok := row["data"].([]byte); ok {
			return db.AsString(row, "mime"), b, nil
		}
	}
	return avatar.MIMEPNG, avatar.PNG(name), nil
}

// AvatarVersions returns each listed agent's custom-avatar update time (unix seconds) so the UI can
// version its <img> URLs. When an avatar is set or replaced the URL changes and browsers refetch it,
// which is what a re-seeded or freshly uploaded avatar needs. Agents with only the generated default
// are absent, so callers fall back to version 0.
func AvatarVersions(ctx context.Context, d db.DB, names []string) map[string]int64 {
	out := map[string]int64{}
	if len(names) == 0 {
		return out
	}
	args := make([]any, 0, len(names))
	for _, n := range names {
		args = append(args, n)
	}
	rows, err := db.QueryRows(ctx, d, "SELECT name, updated FROM avatars WHERE name IN ("+db.Marks(len(args))+")", args...)
	if err != nil {
		return out
	}
	for _, row := range rows {
		out[db.AsString(row, "name")] = db.AsInt64(row, "updated")
	}
	return out
}

// decodeBase64 accepts the common encodings so agents can paste whatever their client produced.
func decodeBase64(s string) ([]byte, error) {
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawURLEncoding.DecodeString(s)
}
