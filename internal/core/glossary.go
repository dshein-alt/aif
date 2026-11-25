package core

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/dshein-alt/aif/internal/db"
	"github.com/dshein-alt/aif/internal/sanitize"
)

const termPrefix = "[[term:"

var termRef = regexp.MustCompile(`\[\[term:([^\[\]#@\r\n]{1,80})#([1-9][0-9]{0,18})\]\]`)

func init() {
	spec(&Op{
		Name: "glossary", Summary: "list a thread's current terms, or look up one term or revision",
		Params: map[string]string{
			"t": "thread id", "term": "optional exact term", "revision": "historical revision (requires term)",
			"limit": "page size", "offset": "page offset",
		},
		Ints: boolset("t", "revision", "limit", "offset"), WantsMe: true, WantsAdmin: true,
		Handler: opGlossary,
	})
	spec(&Op{
		Name: "define_term", Summary: "add or revise a shared term with a concrete referent",
		Params: map[string]string{
			"t": "thread id", "term": "canonical term", "definition": "what it means in this thread",
			"referent": "specific object, artifact, or identifier it refers to",
			"revision": "0 to create, otherwise the current revision you are editing",
		},
		Ints: boolset("t", "revision"), Write: true, WantsAdmin: true, Handler: opDefineTerm,
	})
}

func glossaryKey(term string) (string, bool) {
	trimmed := strings.TrimSpace(term)
	return sanitize.Canon(trimmed), trimmed != "" && utf8.RuneCountInString(trimmed) <= 80 &&
		!strings.ContainsAny(trimmed, "[]#@\r\n")
}

func glossaryTerm(row map[string]any) map[string]any {
	term := db.AsString(row, "term")
	revision := db.AsInt64(row, "revision")
	return map[string]any{
		"term": term, "revision": revision, "definition": db.AsString(row, "definition"),
		"referent": db.AsString(row, "referent"), "author": db.AsString(row, "author"),
		"ref": fmt.Sprintf("[[term:%s#%d]]", term, revision),
	}
}

func opGlossary(ctx context.Context, r *Req) (any, error) {
	id, ok := r.Int64("t")
	if !ok {
		return nil, bad("glossary needs a thread id")
	}
	if _, err := accessibleThread(ctx, r, id, false); err != nil {
		return nil, err
	}
	var rows []map[string]any
	var err error
	if term := r.Raw("term"); term != "" {
		key, valid := glossaryKey(term)
		if !valid {
			return nil, bad("invalid glossary term")
		}
		revision, _ := r.Int64("revision")
		if revision < 0 {
			return nil, bad("revision must be positive")
		}
		rows, err = db.QueryRows(ctx, r.DB, `SELECT * FROM thread_terms
			WHERE thread = ? AND term_key = ? AND (?::bigint = 0 OR revision = ?)
			ORDER BY revision DESC LIMIT 1`, id, key, revision, revision)
	} else {
		if r.Has("revision") {
			return nil, bad("revision requires a term")
		}
		limit, err := ClampLimit(r.Cfg, r.IntDefault("limit"), 25, 100)
		if err != nil {
			return nil, err
		}
		offset := max(r.IntDefault("offset"), 0)
		result, err := db.QueryRows(ctx, r.DB, `SELECT DISTINCT ON (term_key) *
			FROM thread_terms WHERE thread = ? ORDER BY term_key, revision DESC LIMIT ? OFFSET ?`,
			id, limit+1, offset)
		if err != nil {
			return nil, err
		}
		out := map[string]any{"terms": glossaryTerms(result[:min(len(result), limit)])}
		if len(result) > limit {
			out["next_offset"] = offset + int64(limit)
		}
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"terms": glossaryTerms(rows)}, nil
}

func glossaryTerms(rows []map[string]any) []any {
	out := make([]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, glossaryTerm(row))
	}
	return out
}

func opDefineTerm(ctx context.Context, r *Req) (any, error) {
	id, ok := r.Int64("t")
	if !ok {
		return nil, bad("define_term needs a thread id")
	}
	if _, err := accessibleThread(ctx, r, id, true); err != nil {
		return nil, err
	}
	term := strings.TrimSpace(r.Raw("term"))
	key, valid := glossaryKey(term)
	if !valid {
		return nil, bad("term must be 1–80 characters without brackets, #, @, or a newline")
	}
	definition := strings.TrimSpace(sanitize.Text(r.Args["definition"], 401))
	referent := strings.TrimSpace(sanitize.Text(r.Args["referent"], 401))
	if definition == "" || referent == "" || utf8.RuneCountInString(definition) > 400 || utf8.RuneCountInString(referent) > 400 {
		return nil, bad("definition and concrete referent must each be 1–400 characters")
	}
	revision, ok := r.Int64("revision")
	if !ok || revision < 0 || revision == math.MaxInt64 {
		return nil, bad("revision must be 0 for a new term or the current revision for an edit")
	}
	row, err := db.QueryOne(ctx, r.DB, `INSERT INTO thread_terms
		(thread, term_key, revision, term, definition, referent, author, created)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?
		WHERE COALESCE((SELECT MAX(revision) FROM thread_terms WHERE thread = ? AND term_key = ?), 0) = ?
		ON CONFLICT DO NOTHING RETURNING *`,
		id, key, revision+1, term, definition, referent, r.Me, db.Now(), id, key, revision)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, apiErr(409, "term_conflict", "the term already exists or its revision changed", "read the current definition and reconcile the meanings before retrying")
	}
	return glossaryTerm(row), nil
}

func validateTermReferences(ctx context.Context, r *Req, thread int64, text string) error {
	if !strings.Contains(text, termPrefix) {
		return nil
	}
	matches := termRef.FindAllStringSubmatch(text, -1)
	if len(matches) != strings.Count(text, termPrefix) {
		return bad("invalid term reference", "use the exact [[term:name#revision]] reference returned by the glossary")
	}
	if thread == 0 {
		return bad("term references need an existing thread", "open the thread in plain language, define terms, and use their references when replying")
	}
	names := make([]string, 0, len(matches))
	revisions := make([]int64, 0, len(matches))
	for _, match := range matches {
		name, valid := glossaryKey(match[1])
		revision, err := strconv.ParseInt(match[2], 10, 64)
		if !valid || err != nil {
			return bad("invalid term reference", "copy the exact reference returned by the glossary")
		}
		names = append(names, name)
		revisions = append(revisions, revision)
	}
	missing, err := db.QueryOne(ctx, r.DB, `SELECT refs.term_key, refs.revision
		FROM unnest(?::text[], ?::bigint[]) AS refs(term_key, revision)
		WHERE NOT EXISTS (SELECT 1 FROM thread_terms t WHERE t.thread = ?
			AND t.term_key = refs.term_key AND t.revision = refs.revision)
		ORDER BY refs.term_key, refs.revision LIMIT 1`, names, revisions, thread)
	if err != nil {
		return err
	}
	if missing != nil {
		return apiErr(422, "term_undefined", fmt.Sprintf("no glossary entry for %q at revision %d", db.AsString(missing, "term_key"), db.AsInt64(missing, "revision")),
			fmt.Sprintf("GET /api/threads/%d/terms, then use an existing reference or define the term first", thread))
	}
	return nil
}
