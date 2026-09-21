package tokens

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/dshein-alt/aif/internal/config"
	"github.com/dshein-alt/aif/internal/db"
)

const TokenLen = 24

// LiveSQL is the one definition of "live" for a token row: not revoked and not expired.
const LiveSQL = "revoked IS NULL AND (exp = 0 OR exp > ?)"

func IsLive(row map[string]any, ts float64) bool {
	if row == nil {
		return false
	}
	if !db.IsNull(row, "revoked") && db.AsFloat(row, "revoked") != 0 {
		return false
	}
	exp := db.AsFloat(row, "exp")
	return exp == 0 || exp > ts
}

// DeriveToken returns the deterministic token for a (name, nonce) pair under this server's salt.
func DeriveToken(salt, name, nonce string) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%s\x00%s", salt, name, nonce))
	return "aif_" + hex.EncodeToString(sum[:])[:TokenLen]
}

func NewNonce() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func Lookup(ctx context.Context, d db.DB, token string) (map[string]any, error) {
	return db.QueryOne(ctx, d, "SELECT * FROM tokens WHERE self_token = ?", token)
}

type Issued struct {
	Token  string
	Name   string
	Parent string
	Root   string
	Exp    float64
	Nonce  string
}

// Issue creates a token row. issuer==nil means a tree root (parent==root==self).
func Issue(ctx context.Context, d db.DB, cfg *config.Config, issuer map[string]any, name, descr string, days float64) (*Issued, error) {
	nonce := NewNonce()
	token := DeriveToken(cfg.TokenSalt, name, nonce)
	now := db.Now()
	root, parent := token, token
	if issuer != nil {
		parent = db.AsString(issuer, "self_token")
		root = db.AsString(issuer, "root_token")
	}
	var exp float64
	if days != 0 {
		exp = now + days*86400
	} else if name == "" {
		exp = now + float64(cfg.InviteTTL)
	}
	_, err := db.Exec(ctx, d,
		`INSERT INTO tokens (name, low, root_token, parent_token, self_token, descr, created, exp, nonce)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		name, name, root, parent, token, descr, now, exp, nonce)
	if err != nil {
		return nil, err
	}
	return &Issued{Token: token, Name: name, Parent: parent, Root: root, Exp: exp, Nonce: nonce}, nil
}

// Claim binds name to a token row and rewrites self_token to the name-derived form; returns it.
func Claim(ctx context.Context, d db.DB, cfg *config.Config, row map[string]any, name string) (string, error) {
	final := DeriveToken(cfg.TokenSalt, name, db.AsString(row, "nonce"))
	now := db.Now()
	if db.AsString(row, "root_token") == db.AsString(row, "self_token") {
		if _, err := db.Exec(ctx, d,
			`UPDATE tokens SET name=?, low=?, self_token=?, claimed=?, root_token=?, parent_token=? WHERE self_token=?`,
			name, name, final, now, final, final, db.AsString(row, "self_token")); err != nil {
			return "", err
		}
	} else {
		if _, err := db.Exec(ctx, d,
			`UPDATE tokens SET name=?, low=?, self_token=?, claimed=? WHERE self_token=?`,
			name, name, final, now, db.AsString(row, "self_token")); err != nil {
			return "", err
		}
	}
	// An un-named invite never expires once claimed (the invite TTL was a claim window, not a life).
	if db.AsString(row, "name") == "" {
		if _, err := db.Exec(ctx, d, "UPDATE tokens SET exp = 0 WHERE self_token = ?", final); err != nil {
			return "", err
		}
	}
	return final, nil
}

// Subtree returns the node plus every descendant, following parent_token downward.
func Subtree(ctx context.Context, d db.DB, selfToken string) ([]map[string]any, error) {
	me, err := Lookup(ctx, d, selfToken)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]any{}
	if me != nil {
		out[selfToken] = me
	}
	frontier := []string{selfToken}
	for len(frontier) > 0 {
		batch := frontier
		frontier = nil
		rows, err := db.QueryRows(ctx, d,
			fmt.Sprintf("SELECT * FROM tokens WHERE parent_token IN (%s)", db.Marks(len(batch))),
			toArgs(batch)...)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			st := db.AsString(r, "self_token")
			if _, ok := out[st]; ok {
				continue
			}
			out[st] = r
			frontier = append(frontier, st)
		}
	}
	var res []map[string]any
	for _, v := range out {
		res = append(res, v)
	}
	return res, nil
}

// IsAncestor walks target's parent chain to its root; true if ancestorToken appears on the way.
func IsAncestor(ctx context.Context, d db.DB, ancestorToken string, target map[string]any) (bool, error) {
	seen := map[string]bool{}
	current := target
	for {
		if db.AsString(current, "self_token") == ancestorToken {
			return true, nil
		}
		if db.AsString(current, "parent_token") == db.AsString(current, "self_token") || seen[db.AsString(current, "self_token")] {
			return false, nil
		}
		seen[db.AsString(current, "self_token")] = true
		parent, err := Lookup(ctx, d, db.AsString(current, "parent_token"))
		if err != nil {
			return false, err
		}
		if parent == nil {
			return false, nil
		}
		current = parent
	}
}

// RevokeSubtree marks the target and its whole subtree revoked; returns affected self_tokens.
func RevokeSubtree(ctx context.Context, d db.DB, target map[string]any) ([]string, error) {
	nodes, err := Subtree(ctx, d, db.AsString(target, "self_token"))
	if err != nil {
		return nil, err
	}
	now := db.Now()
	affected := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if db.IsNull(n, "revoked") {
			if _, err := db.Exec(ctx, d, "UPDATE tokens SET revoked = ? WHERE self_token = ?", now, db.AsString(n, "self_token")); err != nil {
				return nil, err
			}
		}
		affected = append(affected, db.AsString(n, "self_token"))
	}
	return affected, nil
}

func toArgs(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}
