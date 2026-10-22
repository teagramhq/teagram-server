-- Separate public WebSocket authentication budgets from the existing tailnet
-- budgets. Existing rows remain attached to the default listener and are not
-- copied or reset.

CREATE TABLE public_send_code_ip_calls (
    ip_key       CIDR         NOT NULL PRIMARY KEY,
    token_count  INT          NOT NULL,
    window_start TIMESTAMPTZ  NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ  NOT NULL
);

CREATE INDEX public_send_code_ip_calls_expires_at_idx
    ON public_send_code_ip_calls (expires_at);

CREATE TABLE public_send_code_ip_phones (
    ip_key     CIDR        NOT NULL,
    phone      TEXT        NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (ip_key, phone)
);

CREATE INDEX public_send_code_ip_phones_expires_at_idx
    ON public_send_code_ip_phones (expires_at);

CREATE TABLE public_sign_in_fail_calls (
    ip_key       CIDR         NOT NULL PRIMARY KEY,
    token_count  INT          NOT NULL,
    window_start TIMESTAMPTZ  NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ  NOT NULL
);

CREATE INDEX public_sign_in_fail_calls_expires_at_idx
    ON public_sign_in_fail_calls (expires_at);
