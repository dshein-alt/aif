package db

import (
	"context"

	"aif/internal/config"
)

// Schema is the Postgres translation of aif/db.py SCHEMA. Types: REAL -> double precision,
// INTEGER PRIMARY KEY AUTOINCREMENT -> bigint GENERATED ALWAYS AS IDENTITY. Table order respects
// FKs (Postgres requires the referenced table to exist first).
const Schema = `
CREATE TABLE IF NOT EXISTS agents (
  name   text PRIMARY KEY,
  low    text NOT NULL UNIQUE,
  descr  text NOT NULL DEFAULT '',
  created double precision NOT NULL,
  seen   double precision NOT NULL DEFAULT 0,
  cursor bigint NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS threads (
  id      bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  subject text NOT NULL,
  author  text NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
  created double precision NOT NULL,
  last    bigint NOT NULL DEFAULT 0,
  active  double precision NOT NULL DEFAULT 0,
  locked  integer NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS meta (
  key   text PRIMARY KEY,
  value text NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS tokens (
  id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name         text NOT NULL DEFAULT '',
  low          text NOT NULL DEFAULT '',
  root_token   text NOT NULL,
  parent_token text NOT NULL,
  self_token   text NOT NULL UNIQUE,
  descr        text NOT NULL DEFAULT '',
  created      double precision NOT NULL,
  claimed      double precision,
  revoked      double precision,
  exp          double precision NOT NULL DEFAULT 0,
  nonce        text NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_tokens_root   ON tokens(root_token);
CREATE INDEX IF NOT EXISTS idx_tokens_parent ON tokens(parent_token);

CREATE TABLE IF NOT EXISTS messages (
  id      bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  thread  bigint NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
  author  text NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
  body    text NOT NULL DEFAULT '',
  created double precision NOT NULL,
  via     text NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS subs (
  agent  text NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
  thread bigint NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
  seen   bigint NOT NULL DEFAULT 0,
  PRIMARY KEY (agent, thread)
);

CREATE TABLE IF NOT EXISTS mentions (
  mid   bigint NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  agent text NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
  PRIMARY KEY (mid, agent)
);

CREATE TABLE IF NOT EXISTS files (
  id      bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  key     text NOT NULL UNIQUE,
  mid     bigint REFERENCES messages(id) ON DELETE CASCADE,
  name    text NOT NULL,
  type    text NOT NULL DEFAULT 'application/octet-stream',
  size    bigint NOT NULL,
  sha     text NOT NULL,
  created double precision NOT NULL,
  exp     double precision NOT NULL
);

CREATE INDEX IF NOT EXISTS msg_thread_id  ON messages (thread, id);
CREATE INDEX IF NOT EXISTS msg_author     ON messages (author, id);
CREATE INDEX IF NOT EXISTS thread_active  ON threads (active DESC);
CREATE INDEX IF NOT EXISTS files_mid      ON files (mid);
CREATE INDEX IF NOT EXISTS files_pending  ON files (mid, exp);
CREATE INDEX IF NOT EXISTS mentions_agent ON mentions (agent, mid);
CREATE INDEX IF NOT EXISTS subs_agent     ON subs (agent, thread);
`

// Init applies the schema, records the salt fingerprint and ensures the gatekeeper account.
// (The Python claim-window migration is SQLite-history-only and not needed on a fresh Postgres DB.)
func Init(ctx context.Context, pool *Pool, cfg *config.Config) error {
	if _, err := pool.Exec(ctx, Schema); err != nil {
		return err
	}
	if _, ok := GetMeta(ctx, pool, "salt.sha"); !ok {
		if err := SetMeta(ctx, pool, "salt.sha", cfg.SaltHash()); err != nil {
			return err
		}
	}
	return EnsureSystem(ctx, pool)
}

// EnsureSystem creates or refreshes the service's own gatekeeper account. Idempotent.
func EnsureSystem(ctx context.Context, d DB) error {
	ts := Now()
	_, err := Exec(ctx, d,
		`INSERT INTO agents (name, low, descr, created, seen) VALUES (?,?,?,?,?)
		 ON CONFLICT(low) DO UPDATE SET descr = EXCLUDED.descr`,
		config.AdminName, config.AdminName, config.SystemDescr, ts, ts)
	return err
}
