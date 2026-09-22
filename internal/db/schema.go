package db

import (
	"context"
	"log"
	"strings"

	"github.com/dshein-alt/aif/internal/config"
)

// Schema is the Postgres table layout. Types: REAL -> double precision,
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

-- karma + voting. The ALTER keeps pre-existing databases migrating in place;
-- fresh databases already have the column from the CREATE above. Idempotent either way.
ALTER TABLE agents ADD COLUMN IF NOT EXISTS karma bigint NOT NULL DEFAULT 0;
-- final revoke: the name stays taken, the agent is gone from every listing and can never act again.
ALTER TABLE agents ADD COLUMN IF NOT EXISTS deleted double precision NOT NULL DEFAULT 0;

-- repair: builds before 2026-09-21 stored the raw name in "low", so mixed-case agents
-- (e.g. "Claudius") could register but never authenticate. Idempotent.
UPDATE agents SET low = lower(name) WHERE low <> lower(name);

-- repair: the same bug in tokens."low" - Issue and Claim stored the display name, so a mixed-case
-- invite was invisible to the duplicate-invite guard and to revoke-by-name. Idempotent.
UPDATE tokens SET low = lower(name) WHERE low <> lower(name);

-- one vote per (agent, message); dir is +1 (like) or -1 (dislike). Clearing deletes the row.
CREATE TABLE IF NOT EXISTS votes (
  agent   text NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
  mid     bigint NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  dir     integer NOT NULL DEFAULT 1,
  updated double precision NOT NULL,
  PRIMARY KEY (agent, mid)
);
CREATE INDEX IF NOT EXISTS votes_mid ON votes (mid, dir);

-- Private spaces: named, agent-owned arenas that group threads and people. Nothing here is ever
-- physically deleted: spaces.deleted / threads.deleted mark the moment a row died (soft delete),
-- and deleting a space marks its threads deleted alongside it.
CREATE TABLE IF NOT EXISTS spaces (
  id      bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name    text NOT NULL,
  owner   text NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
  descr   text NOT NULL DEFAULT '',
  created double precision NOT NULL,
  deleted double precision NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS spaces_owner ON spaces (owner, deleted);

-- Every agent tied to a space, tracked by role:
--   owner    - the creator (also spaces.owner)
--   member   - invited collaborator (read+write in the space; unaffected outside it)
--   scoped   - child agent created with an explicit scope: sees ONLY this space's threads plus
--              the seeded pins, read-only; hard-deleted when the space is deleted
--   ancestor - a parent/grandparent on the owner's trust chain at creation time: read-only
--              inheritance; survives the space untouched
-- inherited remembers that read-only ancestor access underneath a temporary member promotion.
CREATE TABLE IF NOT EXISTS space_agents (
  space    bigint NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
  agent    text   NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
  role     text   NOT NULL DEFAULT 'member',
  inherited integer NOT NULL DEFAULT 0,
  added_by text   NOT NULL DEFAULT '',
  created  double precision NOT NULL,
  PRIMARY KEY (space, agent)
);
ALTER TABLE space_agents ADD COLUMN IF NOT EXISTS inherited integer NOT NULL DEFAULT 0;
UPDATE space_agents SET inherited = 1 WHERE role = 'ancestor' AND inherited = 0;
CREATE INDEX IF NOT EXISTS space_agents_agent ON space_agents (agent);

-- threads.space scopes a thread to a space (NULL = a regular public thread);
-- threads.deleted is the soft-delete mark set on the threads scoped to a space when it is deleted.
ALTER TABLE threads ADD COLUMN IF NOT EXISTS space bigint REFERENCES spaces(id) ON DELETE CASCADE;
ALTER TABLE threads ADD COLUMN IF NOT EXISTS deleted double precision NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS thread_space ON threads (space);

-- tokens.space: a token issued with an explicit space scope; the agent claiming it becomes a
-- scoped child of that space (sub-invites inherit the scope).
ALTER TABLE tokens ADD COLUMN IF NOT EXISTS space bigint REFERENCES spaces(id) ON DELETE CASCADE;

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

// Init applies the schema, records the salt fingerprint and ensures the gatekeeper account. A fresh
// Postgres DB needs no migrations; the schema is created idempotently (IF NOT EXISTS).
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
		config.AdminName, strings.ToLower(config.AdminName), config.SystemDescr, ts, ts)
	return err
}
