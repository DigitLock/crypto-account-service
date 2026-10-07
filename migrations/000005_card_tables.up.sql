-- Card tables: cards of SRS - Core §2.4.1 and the tables of SRS - Card Spend §2.4.
-- Base-unit amounts are NUMERIC(78,0): every uint256 fits.

CREATE TABLE cards (
    id            UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID          NOT NULL REFERENCES tenants (id),
    card_ref      TEXT          NOT NULL,
    owner_ref     TEXT          NOT NULL,
    -- RESTRICT is the second line of EC-114: a connection with cards cannot be deleted.
    connection_id UUID          NOT NULL REFERENCES connections (id) ON DELETE RESTRICT,
    status        TEXT          NOT NULL DEFAULT 'ACTIVE',
    daily_limit   NUMERIC(78, 0) NOT NULL,
    created_at    TIMESTAMPTZ   NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ   NOT NULL DEFAULT now(),
    CONSTRAINT cards_tenant_card_ref_key UNIQUE (tenant_id, card_ref),
    CONSTRAINT cards_status_check CHECK (status IN ('ACTIVE', 'FROZEN')),
    CONSTRAINT cards_daily_limit_check CHECK (daily_limit >= 0)
);

CREATE INDEX cards_tenant_created_idx ON cards (tenant_id, created_at, card_ref);
CREATE INDEX cards_connection_id_idx ON cards (connection_id);

CREATE TABLE authorizations (
    id              UUID           PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID           NOT NULL REFERENCES tenants (id),
    auth_id         TEXT           NOT NULL,
    chain_auth_id   BYTEA          NOT NULL,
    parent_auth_id  TEXT,
    card_id         UUID           REFERENCES cards (id),
    request_hash    BYTEA,
    fiat_amount     NUMERIC(18, 4),
    fiat_currency   CHAR(3),
    rate            NUMERIC(20, 10),
    buffer_bps      INTEGER,
    token           TEXT,
    token_amount    NUMERIC(78, 0),
    debited_amount  NUMERIC(78, 0) NOT NULL DEFAULT 0,
    returned_amount NUMERIC(78, 0) NOT NULL DEFAULT 0,
    wallet_address  BYTEA,
    chain_id        BIGINT,
    status          TEXT           NOT NULL,
    decline_reason  TEXT,
    merchant        JSONB,
    received_at     TIMESTAMPTZ    NOT NULL,
    deadline_at     TIMESTAMPTZ,
    valid_until     TIMESTAMPTZ,
    decided_at      TIMESTAMPTZ,
    CONSTRAINT authorizations_tenant_auth_id_key UNIQUE (tenant_id, auth_id),
    CONSTRAINT authorizations_chain_auth_id_key UNIQUE (chain_auth_id),
    CONSTRAINT authorizations_chain_auth_id_check CHECK (octet_length(chain_auth_id) = 32),
    CONSTRAINT authorizations_wallet_address_check CHECK (octet_length(wallet_address) = 20),
    CONSTRAINT authorizations_fiat_amount_check CHECK (fiat_amount > 0),
    CONSTRAINT authorizations_rate_check CHECK (rate > 0),
    CONSTRAINT authorizations_buffer_bps_check CHECK (buffer_bps >= 0),
    CONSTRAINT authorizations_token_amount_check CHECK (token_amount >= 0),
    CONSTRAINT authorizations_debited_amount_check CHECK (debited_amount >= 0),
    CONSTRAINT authorizations_returned_amount_check CHECK (returned_amount >= 0),
    CONSTRAINT authorizations_status_check CHECK (status IN (
        'RECEIVED', 'DEBIT_SUBMITTED', 'APPROVED', 'DEBIT_CONFIRMED', 'DECLINED', 'TIMED_OUT', 'LATE_DEBIT',
        'LATE_DEBIT_REFUNDED', 'DEBIT_LOST'
    )),
    CONSTRAINT authorizations_decline_reason_check CHECK (decline_reason IN (
        'CARD_NOT_FOUND', 'CARD_FROZEN', 'PROGRAM_PAUSED', 'CURRENCY_NOT_SUPPORTED', 'RATE_UNAVAILABLE',
        'LIMIT_EXCEEDED', 'INSUFFICIENT_FUNDS', 'INSUFFICIENT_ALLOWANCE', 'CHAIN_UNAVAILABLE', 'DEBIT_REVERTED',
        'TIMEOUT', 'REVERSED_BEFORE_AUTH', 'INTERNAL_ERROR'
    ))
);

-- ListAuthorizations: received_at descending, auth_id.
CREATE INDEX authorizations_tenant_received_idx ON authorizations (tenant_id, received_at DESC, auth_id);
CREATE INDEX authorizations_card_id_idx ON authorizations (card_id);

-- Append-only status history.
CREATE TABLE authorization_events (
    id               BIGSERIAL   PRIMARY KEY,
    authorization_id UUID        NOT NULL REFERENCES authorizations (id),
    from_status      TEXT,
    to_status        TEXT        NOT NULL,
    reason           TEXT,
    details          JSONB,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT authorization_events_from_status_check CHECK (from_status IN (
        'RECEIVED', 'DEBIT_SUBMITTED', 'APPROVED', 'DEBIT_CONFIRMED', 'DECLINED', 'TIMED_OUT', 'LATE_DEBIT',
        'LATE_DEBIT_REFUNDED', 'DEBIT_LOST'
    )),
    CONSTRAINT authorization_events_to_status_check CHECK (to_status IN (
        'RECEIVED', 'DEBIT_SUBMITTED', 'APPROVED', 'DEBIT_CONFIRMED', 'DECLINED', 'TIMED_OUT', 'LATE_DEBIT',
        'LATE_DEBIT_REFUNDED', 'DEBIT_LOST'
    ))
);

CREATE INDEX authorization_events_authorization_idx ON authorization_events (authorization_id, id);

CREATE TABLE returns (
    id               UUID           PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        UUID           NOT NULL REFERENCES tenants (id),
    authorization_id UUID           NOT NULL REFERENCES authorizations (id),
    return_id        TEXT           NOT NULL,
    chain_refund_id  BYTEA          NOT NULL,
    type             TEXT           NOT NULL,
    request_hash     BYTEA,
    fiat_amount      NUMERIC(18, 4),
    token_amount     NUMERIC(78, 0) NOT NULL,
    status           TEXT           NOT NULL,
    attempts         INTEGER        NOT NULL DEFAULT 0,
    created_at       TIMESTAMPTZ    NOT NULL DEFAULT now(),
    CONSTRAINT returns_tenant_return_id_key UNIQUE (tenant_id, return_id),
    CONSTRAINT returns_chain_refund_id_key UNIQUE (chain_refund_id),
    CONSTRAINT returns_chain_refund_id_check CHECK (octet_length(chain_refund_id) = 32),
    CONSTRAINT returns_type_check CHECK (type IN ('REVERSAL', 'REFUND', 'LATE_DEBIT')),
    CONSTRAINT returns_status_check CHECK (status IN (
        'ACCEPTED', 'SUBMITTED', 'INCLUDED', 'CONFIRMED', 'RETRYING', 'NOTHING_TO_RETURN'
    )),
    CONSTRAINT returns_token_amount_check CHECK (token_amount >= 0),
    CONSTRAINT returns_attempts_check CHECK (attempts >= 0)
);

CREATE INDEX returns_authorization_idx ON returns (authorization_id, created_at);

CREATE TABLE operator_txs (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    chain_id         BIGINT      NOT NULL,
    operator_address BYTEA       NOT NULL,
    nonce            BIGINT      NOT NULL,
    purpose          TEXT        NOT NULL,
    authorization_id UUID        REFERENCES authorizations (id),
    return_row_id    UUID        REFERENCES returns (id),
    tx_hash          BYTEA,
    replaced_hashes  BYTEA[]     NOT NULL DEFAULT '{}',
    status           TEXT        NOT NULL,
    block_number     BIGINT,
    block_hash       BYTEA,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT operator_txs_nonce_key UNIQUE (chain_id, operator_address, nonce),
    CONSTRAINT operator_txs_operator_address_check CHECK (octet_length(operator_address) = 20),
    CONSTRAINT operator_txs_nonce_check CHECK (nonce >= 0),
    CONSTRAINT operator_txs_tx_hash_check CHECK (octet_length(tx_hash) = 32),
    CONSTRAINT operator_txs_block_hash_check CHECK (octet_length(block_hash) = 32),
    CONSTRAINT operator_txs_purpose_check CHECK (purpose IN ('DEBIT', 'REFUND', 'RELEASE')),
    CONSTRAINT operator_txs_status_check CHECK (status IN (
        'PLANNED', 'SENT', 'INCLUDED', 'CONFIRMED', 'REVERTED', 'RELEASED'
    ))
);

CREATE INDEX operator_txs_authorization_idx ON operator_txs (authorization_id);
CREATE INDEX operator_txs_return_row_idx ON operator_txs (return_row_id);

CREATE TABLE operator_accounts (
    chain_id   BIGINT NOT NULL,
    address    BYTEA  NOT NULL,
    next_nonce BIGINT NOT NULL,
    PRIMARY KEY (chain_id, address),
    CONSTRAINT operator_accounts_address_check CHECK (octet_length(address) = 20),
    CONSTRAINT operator_accounts_next_nonce_check CHECK (next_nonce >= 0)
);
