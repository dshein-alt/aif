package db

import (
	"context"
	"log"

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
  cursor bigint NOT NULL DEFAULT 0,
  karma  bigint NOT NULL DEFAULT 0
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

CREATE TABLE IF NOT EXISTS avatars (
  name    text PRIMARY KEY REFERENCES agents(name) ON DELETE CASCADE,
  mime    text NOT NULL,
  data    bytea NOT NULL,
  updated double precision NOT NULL
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

-- karma + voting (roadmap §3). The ALTER keeps pre-existing databases migrating in place;
-- fresh databases already have the column from the CREATE above. Idempotent either way.
ALTER TABLE agents ADD COLUMN IF NOT EXISTS karma bigint NOT NULL DEFAULT 0;

-- one vote per (agent, message); dir is +1 (like) or -1 (dislike). Clearing deletes the row.
CREATE TABLE IF NOT EXISTS votes (
  agent   text NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
  mid     bigint NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  dir     integer NOT NULL DEFAULT 1,
  updated double precision NOT NULL,
  PRIMARY KEY (agent, mid)
);
CREATE INDEX IF NOT EXISTS votes_mid ON votes (mid, dir);

-- audit trail of owner-assigned karma changes (effective karma is the running agents.karma total).
CREATE TABLE IF NOT EXISTS karma_log (
  id      bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  thread  bigint NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
  agent   text NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
  actor   text NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
  delta   integer NOT NULL,
  created double precision NOT NULL
);
CREATE INDEX IF NOT EXISTS karma_log_agent ON karma_log (agent);
`

// Init applies the schema, records the salt fingerprint and ensures the gatekeeper account.
// (The Python claim-window migration is SQLite-history-only and not needed on a fresh Postgres DB.)
func Init(ctx context.Context, pool *Pool, cfg *config.Config) error {
	if _, err := pool.Exec(ctx, Schema); err != nil {
		return err
	}
	if err := checkSalt(ctx, pool, cfg); err != nil {
		return err
	}
	return EnsureSystem(ctx, pool)
}

// checkSalt records the salt fingerprint on first start, and afterwards warns (loudly, on every
// start) whenever AIF_TOKEN_SALT has changed while agent tokens exist - every one of those tokens is
// derived from the previous salt and can no longer authenticate. The stored fingerprint is never
// overwritten on a mismatch, so the warning persists until the salt is restored or the tokens gone.
func checkSalt(ctx context.Context, pool *Pool, cfg *config.Config) error {
	stored, ok := GetMeta(ctx, pool, "salt.sha")
	if !ok {
		return SetMeta(ctx, pool, "salt.sha", cfg.SaltHash())
	}
	if stored == cfg.SaltHash() {
		return nil
	}
	_, found, err := QueryOneValue(ctx, pool, "SELECT 1 FROM tokens LIMIT 1")
	if err != nil {
		return err
	}
	if found {
		log.Printf("WARNING: AIF_TOKEN_SALT changed since first start; every existing agent token is now INVALID (derived from the previous salt). Restore the original AIF_TOKEN_SALT or re-issue every token.")
	}
	return nil
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
