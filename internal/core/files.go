package core

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"strings"

	"aif/internal/config"
	"aif/internal/db"
	"aif/internal/sanitize"
	"aif/internal/storage"
)

func init() {
	spec(&Op{
		Name:    "up",
		Summary: "upload one small file, returns its key; pass keys to post as files=[{\"k\":key}]",
		Params:  map[string]string{"name": "display file name", "text": "file content as text", "b64": "file content, base64", "type": "mime type"},
		Aliases: alias("n", "name", "content", "text", "file", "name"),
		Handler: opUp,
	})
	spec(&Op{
		Name:    "dl",
		Summary: "read an attached file: metadata by default; text=1 embeds the content (text or base64)",
		Params:  map[string]string{"id": "file id (message fl[].i)", "text": "1 = embed content", "b64": "1 = force base64 content"},
		Aliases: alias("i", "id", "file", "id"),
		Bools:   boolset("text", "b64"), Ints: boolset("id"),
		Handler: opDl,
	})
}

// CreateUploads stores inline/multipart items as pending blobs and returns their upload keys.
// Each item may carry: n (name), type, text, b64, or reader (io.Reader, set by the HTTP layer).
func CreateUploads(ctx context.Context, d db.DB, cfg *config.Config, items []map[string]any, ts float64) ([]map[string]any, error) {
	if len(items) > cfg.MaxFilesPerMessage {
		return nil, badHint(fmt.Sprintf("%d files: max %d per message", len(items), cfg.MaxFilesPerMessage), "split them across messages")
	}
	if ts == 0 {
		ts = db.Now()
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		name := storage.SanitizeName(firstRaw(item, "n", "name"), 200)
		if name == "" {
			name = "file"
		}
		ctype := sanitize.Oneline(firstRaw(item, "type", "content_type"), 120)
		_, hasText := item["text"]
		if ctype == "" {
			if hasText {
				ctype = "text/plain"
			} else {
				ctype = "application/octet-stream"
			}
		}
		var raw io.Reader
		if rd, ok := item["reader"]; ok && rd != nil {
			raw = rd.(io.Reader)
		} else {
			encoded, _ := item["b64"].(string)
			payload := item["text"]
			if !hasText && encoded == "" {
				return nil, bad(fmt.Sprintf("file %q needs one of text/b64/stream", name))
			}
			if encoded != "" {
				data, err := base64.StdEncoding.DecodeString(encoded)
				if err != nil {
					return nil, bad(fmt.Sprintf("file %q: b64 is not valid base64", name))
				}
				raw = bytes.NewReader(data)
			} else {
				raw = strings.NewReader(sanitize.Fold(payload))
			}
		}
		key := storage.NewKey()
		size, sha, err := storage.Save(cfg, raw, key, cfg.MaxFileSize)
		if err == storage.ErrTooLarge {
			return nil, apiErr(413, "too_large", fmt.Sprintf("file %q exceeds max_file_size=%d bytes", name, cfg.MaxFileSize), "send a smaller file (limit is set by AIF_MAX_FILE_SIZE)")
		}
		if err != nil {
			return nil, err
		}
		if _, err := db.Exec(ctx, d,
			"INSERT INTO files (key, mid, name, type, size, sha, created, exp) VALUES (?,NULL,?,?,?,?,?,?)",
			key, name, ctype, size, sha, ts, ts+float64(cfg.UploadTTL)); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"k": key, "n": name, "s": size, "sha": sha})
	}
	return out, nil
}

func PurgeUploads(ctx context.Context, d db.DB, cfg *config.Config, ts float64) (int, error) {
	rows, err := db.QueryRows(ctx, d, "SELECT key FROM files WHERE mid IS NULL AND exp < ?", ts)
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	if _, err := db.Exec(ctx, d, "DELETE FROM files WHERE mid IS NULL AND exp < ?", ts); err != nil {
		return 0, err
	}
	keys := make([]string, 0, len(rows))
	for _, r := range rows {
		keys = append(keys, db.AsString(r, "key"))
	}
	PurgeBlobs(cfg, keys)
	return len(rows), nil
}

func Attach(ctx context.Context, d db.DB, cfg *config.Config, mid int64, keys []string) ([]map[string]any, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	if len(keys) > cfg.MaxFilesPerMessage {
		return nil, badHint(fmt.Sprintf("%d file keys: max %d per message", len(keys), cfg.MaxFilesPerMessage), "")
	}
	var attached []map[string]any
	for _, key := range keys {
		row, err := db.QueryOne(ctx, d, "SELECT * FROM files WHERE key = ?", key)
		if err != nil {
			return nil, err
		}
		if row == nil {
			return nil, apiErr(404, "unknown_upload", fmt.Sprintf("upload key %q is unknown or expired", key), "upload again (POST /api/files or op up)")
		}
		if !db.IsNull(row, "mid") {
			return nil, apiErr(409, "upload_attached", fmt.Sprintf("upload key %q is already attached to message %d", key, db.AsInt64(row, "mid")), "upload the file again to reuse it")
		}
		id := db.AsInt64(row, "id")
		if _, err := db.Exec(ctx, d, "UPDATE files SET mid = ?, exp = 0 WHERE id = ?", mid, id); err != nil {
			return nil, err
		}
		fresh, _ := db.QueryOne(ctx, d, "SELECT * FROM files WHERE id = ?", id)
		attached = append(attached, fresh)
	}
	return attached, nil
}

func PurgeBlobs(cfg *config.Config, keys []string) int {
	removed := 0
	for _, k := range keys {
		if storage.Remove(cfg, k) {
			removed++
		}
	}
	return removed
}

func opUp(ctx context.Context, r *Req) (any, error) {
	name := r.Raw("name")
	if name == "" {
		name = "file"
	}
	items := []map[string]any{{"n": name, "text": r.Args["text"], "b64": r.Args["b64"], "type": r.Raw("type")}}
	made, err := CreateUploads(ctx, r.DB, r.Cfg, items, 0)
	if err != nil {
		return nil, err
	}
	return made[0], nil
}

func opDl(ctx context.Context, r *Req) (any, error) {
	id, ok := r.Int64("id")
	if !ok {
		return nil, badHint("dl needs id", `dl {"id":7}`)
	}
	row, err := db.QueryOne(ctx, r.DB, "SELECT * FROM files WHERE id = ?", id)
	if err != nil {
		return nil, err
	}
	if row == nil || db.IsNull(row, "mid") {
		return nil, apiErr(404, "no_file", fmt.Sprintf("attached file %d is unknown or expired", id), "file ids come from message field fl[].i")
	}
	var threadID any
	th, _, _ := db.QueryOneValue(ctx, r.DB, "SELECT thread FROM messages WHERE id = ?", db.AsInt64(row, "mid"))
	if th != nil {
		threadID = toI64(th)
	}
	out := map[string]any{"i": db.AsInt64(row, "id"), "n": db.AsString(row, "name"), "s": db.AsInt64(row, "size"), "type": db.AsString(row, "type"), "sha": db.AsString(row, "sha"), "m": db.AsInt64(row, "mid"), "t": threadID}
	if !r.Bool("text") {
		return out, nil
	}
	fh, err := storage.Open(r.Cfg, db.AsString(row, "key"))
	if err != nil {
		return nil, apiErr(409, "blob_missing", err.Error(), "the blob is gone from disk; re-upload the file")
	}
	raw, rerr := io.ReadAll(fh)
	fh.Close()
	if rerr != nil {
		return nil, apiErr(409, "blob_missing", rerr.Error(), "could not read the blob")
	}
	if !r.Bool("b64") && LooksTextual(db.AsString(row, "type"), raw) {
		out["text"] = string(raw)
	} else {
		out["b64"] = base64.StdEncoding.EncodeToString(raw)
	}
	return out, nil
}

func LooksTextual(ctype string, raw []byte) bool {
	ctype = strings.ToLower(strings.TrimSpace(strings.Split(ctype, ";")[0]))
	for _, p := range Textual {
		if strings.HasPrefix(ctype, p) {
			return true
		}
	}
	for _, suf := range []string{"+json", "+xml", "+yaml", "+csv"} {
		if strings.HasSuffix(ctype, suf) {
			return true
		}
	}
	for i := 0; i < 1024 && i < len(raw); i++ {
		if raw[i] == 0 {
			return false
		}
	}
	return true
}

// firstRaw returns the first non-empty string value among the given keys of an item map.
func firstRaw(item map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := item[k]; ok && v != nil {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}
