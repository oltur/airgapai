-- The gateway's state: tool permissions, proposals, confirmations and the audit log. See ADR 0015.
-- Applied once to an empty database named gateway, by a superuser, with psql variables
-- gateway_password set. The gateway connects as gateway_app, which can add audit rows but never
-- change or delete them.

CREATE ROLE gateway_owner NOLOGIN;
CREATE ROLE gateway_app LOGIN PASSWORD :'gateway_password';
CREATE SCHEMA gateway AUTHORIZATION gateway_owner;
SET ROLE gateway_owner;

-- Which roles may use which tools. A tool that no row names is denied to everyone.
CREATE TABLE gateway.tool_permissions (
  role text NOT NULL,
  tool text NOT NULL,
  PRIMARY KEY (role, tool)
);

-- What a model asked for, in canonical form. Never executed directly.
CREATE TABLE gateway.proposals (
  id uuid PRIMARY KEY,
  user_id text NOT NULL,
  tool text NOT NULL,
  args text NOT NULL,          -- canonical JSON
  args_sha256 text NOT NULL,
  sources text NOT NULL,       -- canonical JSON of chunk ids and hashes, never chunk text
  created_at timestamptz NOT NULL,
  expires_at timestamptz NOT NULL
);

-- A user's confirmation of one proposal. Only the token's hash is stored. consumed_at is set once.
CREATE TABLE gateway.confirmations (
  token_sha256 text PRIMARY KEY,
  proposal_id uuid NOT NULL UNIQUE REFERENCES gateway.proposals (id),
  user_id text NOT NULL,
  created_at timestamptz NOT NULL,
  expires_at timestamptz NOT NULL,
  consumed_at timestamptz
);

-- Every decision, chained by hash: each row's hash covers the previous row's hash and its own fields.
CREATE TABLE gateway.audit (
  seq bigint PRIMARY KEY,
  at timestamptz NOT NULL,
  actor text NOT NULL,
  action text NOT NULL,
  decision text NOT NULL,
  detail text NOT NULL,       -- canonical JSON, as hashed; jsonb would reorder it
  prev_sha256 text NOT NULL,
  sha256 text NOT NULL
);

RESET ROLE;
GRANT USAGE ON SCHEMA gateway TO gateway_app;
GRANT SELECT, INSERT, DELETE ON gateway.tool_permissions TO gateway_app;
GRANT SELECT, INSERT ON gateway.proposals TO gateway_app;
GRANT SELECT, INSERT, UPDATE (consumed_at) ON gateway.confirmations TO gateway_app;
-- Append only: no UPDATE, DELETE or TRUNCATE on the audit log.
GRANT SELECT, INSERT ON gateway.audit TO gateway_app;
