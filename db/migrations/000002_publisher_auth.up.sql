-- Publisher accounts, passkey credentials, linked proofs, and browser sessions.

CREATE TABLE IF NOT EXISTS publisher_accounts (
    id UUID PRIMARY KEY,
    display_name TEXT NOT NULL,
    identity_key TEXT NOT NULL UNIQUE,
    identity_private_key_hex TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

DROP TRIGGER IF EXISTS tr_publisher_accounts_updated_at ON publisher_accounts;
CREATE TRIGGER tr_publisher_accounts_updated_at
    BEFORE INSERT OR UPDATE ON publisher_accounts
    FOR EACH ROW
    EXECUTE PROCEDURE dp1_feed_set_updated_at();

CREATE TABLE IF NOT EXISTS publisher_credentials (
    id UUID PRIMARY KEY,
    publisher_account_id UUID NOT NULL REFERENCES publisher_accounts (id) ON DELETE CASCADE,
    credential_id TEXT NOT NULL UNIQUE,
    label TEXT NOT NULL,
    body JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_publisher_credentials_account ON publisher_credentials (publisher_account_id, created_at ASC);

CREATE TABLE IF NOT EXISTS publisher_proofs (
    id UUID PRIMARY KEY,
    publisher_account_id UUID NOT NULL REFERENCES publisher_accounts (id) ON DELETE CASCADE,
    proof_type TEXT NOT NULL,
    proof_value TEXT NOT NULL,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    verified_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT publisher_proofs_type_value_unique UNIQUE (proof_type, proof_value)
);

CREATE INDEX IF NOT EXISTS idx_publisher_proofs_account ON publisher_proofs (publisher_account_id, verified_at ASC);

CREATE TABLE IF NOT EXISTS publisher_sessions (
    id UUID PRIMARY KEY,
    publisher_account_id UUID NOT NULL REFERENCES publisher_accounts (id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    last_used_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_publisher_sessions_token_hash ON publisher_sessions (token_hash);

CREATE TABLE IF NOT EXISTS publisher_ceremonies (
    id UUID PRIMARY KEY,
    ceremony_type TEXT NOT NULL,
    token_hash TEXT NOT NULL UNIQUE,
    publisher_account_id UUID REFERENCES publisher_accounts (id) ON DELETE CASCADE,
    state JSONB NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_publisher_ceremonies_token_hash ON publisher_ceremonies (token_hash);
