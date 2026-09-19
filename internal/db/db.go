package db

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DB is satisfied by both *pgxpool.Pool (autocommit reads) and pgx.Tx (write transactions).
type DB interface {
	Exec(ctx context.Context, query string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, query string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, query string, args ...any) pgx.Row
}

// Pool is the connection pool; a single value threaded through the app.
type Pool struct {
	*pgxpool.Pool
}

func Open(ctx context.Context, url string) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 16
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := p.Ping(ctx); err != nil {
		p.Close()
		return nil, err
	}
	return &Pool{Pool: p}, nil
}

// Now returns unix epoch seconds with millisecond precision (the wire format for all timestamps).
func Now() float64 {
	return float64(time.Now().UnixNano()) / 1e9
}

func Round3(v float64) float64 {
	return float64(int64(v*1000+0.5)) / 1000
}

func Round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

// dollarize rewrites `?` placeholders into $1..$n (left to right). Our SQL never contains a
// literal `?`, so a plain scan is safe; this lets op code read like the SQLite original.
func dollarize(sql string) string {
	if !strings.Contains(sql, "?") {
		return sql
	}
	var b strings.Builder
	n := 0
	for i := 0; i < len(sql); i++ {
		if sql[i] == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
		} else {
			b.WriteByte(sql[i])
		}
	}
	return b.String()
}

func Exec(ctx context.Context, d DB, sql string, args ...any) (pgconn.CommandTag, error) {
	return d.Exec(ctx, dollarize(sql), args...)
}

func Query(ctx context.Context, d DB, sql string, args ...any) (pgx.Rows, error) {
	return d.Query(ctx, dollarize(sql), args...)
}

// QueryRows returns a result set as []map[string]any (lowercased keys, like sqlite3.Row).
func QueryRows(ctx context.Context, d DB, sql string, args ...any) ([]map[string]any, error) {
	rows, err := Query(ctx, d, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAll(rows)
}

func QueryOne(ctx context.Context, d DB, sql string, args ...any) (map[string]any, error) {
	rows, err := Query(ctx, d, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	m, err := scanRow(rows)
	if err != nil {
		return nil, err
	}
	return m, rows.Err()
}

// QueryOneValue returns a single scalar (first column of first row); ok=false when no row.
func QueryOneValue(ctx context.Context, d DB, sql string, args ...any) (any, bool, error) {
	m, err := QueryOne(ctx, d, sql, args...)
	if err != nil || m == nil {
		return nil, false, err
	}
	for _, v := range m {
		return v, true, nil
	}
	return nil, false, nil
}

func scanAll(rows pgx.Rows) ([]map[string]any, error) {
	var out []map[string]any
	for rows.Next() {
		m, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func scanRow(rows pgx.Rows) (map[string]any, error) {
	fields := rows.FieldDescriptions()
	vals, err := rows.Values()
	if err != nil {
		return nil, err
	}
	m := make(map[string]any, len(fields))
	for i, f := range fields {
		m[f.Name] = vals[i]
	}
	return m, nil
}

// --- typed accessors over row maps --------------------------------------------------------

func AsString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	switch v := m[key].(type) {
	case nil:
		return ""
	case string:
		return v
	case []byte:
		return string(v)
	default:
		return fmt.Sprint(v)
	}
}

func AsFloat(m map[string]any, key string) float64 {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int64:
		return float64(v)
	case int32:
		return float64(v)
	case nil:
		return 0
	default:
		return 0
	}
}

func AsInt64(m map[string]any, key string) int64 {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case int64:
		return v
	case int32:
		return int64(v)
	case float64:
		return int64(v)
	case nil:
		return 0
	default:
		return 0
	}
}

// IsNull reports whether the key is present-and-nil (e.g. claimed / revoked / mid).
func IsNull(m map[string]any, key string) bool {
	v, ok := m[key]
	return ok && v == nil
}

// --- SQL composition helpers (the whitelisted dynamic-SQL surface) -------------------------

func Where(clauses []string) string {
	if len(clauses) == 0 {
		return ""
	}
	return "WHERE " + strings.Join(clauses, " AND ")
}

func Marks(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func Desc(flag bool) string {
	if flag {
		return "DESC"
	}
	return "ASC"
}

func SortExpr(table map[string]string, key string) (string, error) {
	v, ok := table[key]
	if !ok {
		return "", fmt.Errorf("unknown sort key %q", key)
	}
	return v, nil
}

// LikeArg escapes LIKE wildcards so user text matches literally (pair with ESCAPE '\').
func LikeArg(term string) string {
	term = strings.ReplaceAll(term, "\\", "\\\\")
	term = strings.ReplaceAll(term, "%", "\\%")
	term = strings.ReplaceAll(term, "_", "\\_")
	return "%" + term + "%"
}

// --- meta table ------------------------------------------------------------

func GetMeta(ctx context.Context, d DB, key string) (string, bool) {
	v, ok, err := QueryOneValue(ctx, d, "SELECT value FROM meta WHERE key = ?", key)
	if err != nil || !ok {
		return "", false
	}
	return AsString(map[string]any{"v": v}, "v"), true
}

func SetMeta(ctx context.Context, d DB, key, value string) error {
	_, err := Exec(ctx, d, "INSERT INTO meta (key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value = EXCLUDED.value", key, value)
	return err
}
