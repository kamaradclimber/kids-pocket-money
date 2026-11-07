# Design

## Overview

Single Go binary, no framework, stdlib `net/http` + pure-Go SQLite driver
(`modernc.org/sqlite`, works with `CGO_ENABLED=0`). Env-based config,
`slog` JSON logs, embedded templates, trusted `Remote-User`/`Remote-Groups`
headers.

## Authentication & roles

The app sits behind authelia (or any trusted reverse proxy) and trusts:

- `Remote-User`: username (mandatory, otherwise 401)
- `Remote-Groups`: comma-separated groups
- `Remote-Email`: optional display info

Two roles, both driven by `Remote-Groups`:

| Role | Group | Rights |
|------|-------|--------|
| Parent | `ADMIN_GROUP` (default `parents`) | see **all** kids at once, create kids, change monthly amounts, add/delete any transaction |
| Kid | `KIDS_GROUP` (default `kids`) | see **only their own** balance and history, record their own **spends** (adding money is a parent-only action) |

Global access rule, enforced by a middleware on every page: a request is
denied with **401** if `Remote-User` is empty, and with **403** if
`Remote-Groups` contains neither the parents nor the kids group. Only the
health probes (`/healthz`, `/readyz`) stay public. A kid username not yet
registered in the app gets 403. A parent who is also a kid gets the parent
view.

## Data model (SQLite)

```sql
CREATE TABLE kids (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  username TEXT NOT NULL UNIQUE,        -- matches Remote-User
  name TEXT NOT NULL,                   -- display name
  monthly_amount INTEGER NOT NULL,      -- cents, >= 0
  start_month TEXT NOT NULL,            -- "YYYY-MM", first credited month
  created_at TEXT NOT NULL,
  created_by TEXT NOT NULL
);

CREATE TABLE transactions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  kid_id INTEGER NOT NULL REFERENCES kids(id) ON DELETE CASCADE,
  amount INTEGER NOT NULL,              -- cents, signed (+credit / -debit)
  label TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('allowance','manual')),
  month TEXT NOT NULL,                  -- "YYYY-MM" the tx belongs to
  date TEXT NOT NULL DEFAULT '',        -- "YYYY-MM-DD", operation date
  created_name TEXT NOT NULL DEFAULT '', -- display name (Authelia Remote-Name)
  created_by TEXT NOT NULL,             -- Remote-User or "system"
  created_at TEXT NOT NULL               -- technical creation timestamp
);

-- one allowance per kid per month
CREATE UNIQUE INDEX idx_transactions_allowance
  ON transactions(kid_id, month) WHERE kind = 'allowance';
```

The `date` column is the **operation date** (what happened when), distinct
from `created_at` (when the row was recorded). The `created_name` column
stores the creator's **display name** (authelia's `Remote-Name` header) so
the history reads "Ajouté par Grégoire" instead of the raw username;
`created_by` keeps the username for the audit trail. Both were added after
the first deployed version: the migration adds them to existing databases
with ALTER TABLE and backfills them (first day of the credited month /
creation day for `date`; the kid's display name for `created_name` when the
creator was a registered kid, username fallback otherwise). The history is
ordered by `date DESC`.

All money is stored in **integer cents**; the UI displays `12,50 €`.
`balance(kid) = SUM(transactions.amount)`.

## Monthly allowance

Parents set `monthly_amount` per kid. Allowances are **materialized as
`allowance` transactions**, ensured lazily:

1. at startup,
2. before rendering any page.

`ensureMonthlyAllowances(now)` walks every month from each kid's
`start_month` to the current month and inserts the missing `allowance`
rows (`INSERT OR IGNORE` on the partial unique index makes it idempotent).
This keeps the balance a plain `SUM(amount)` and makes months that nobody
opened the app still count.

Consequences:

- changing a kid's monthly amount only affects **future** months;
- `allowance` rows are **not deletable** from the UI (a deleted row would be
  re-created by the next ensure pass). Parents correct with a `manual`
  transaction instead; manual transactions are deletable.

## Endpoints

| Method | Path | Who | Action |
|--------|------|-----|--------|
| GET | `/` | kid / parent | kid: own dashboard; parent: family dashboard |
| GET | `/kid/{id}` | parent / that kid | history + parent forms |
| POST | `/kid` | parent | create kid |
| POST | `/kid/{id}/monthly` | parent | change monthly amount |
| POST | `/kid/{id}/transaction` | parent / that kid | add manual transaction (optional operation date) |
| POST | `/transaction/{id}/delete` | parent | delete a manual transaction |
| GET | `/healthz`, `/readyz` | anyone | liveness/readiness |

All POSTs require the CSRF token (cookie + hidden field), are
`ParseForm`-limited and redirect (303) back.

## Security

- CSP: `default-src 'none'`, inline styles, `script-src 'nonce-…'`
- `X-Content-Type-Options`, `X-Frame-Options: DENY`, `Referrer-Policy: same-origin`
- `Cache-Control: no-store` on HTML
- CSRF: random 32-byte hex token in HttpOnly cookie, constant-time compared
- Amounts parsed from strings like `12,50`, validated `|amount| < 10 000 €`

## UI

French, a single-card layout in a small inline stylesheet
(`#285a48` buttons, badges, dark mode via `prefers-color-scheme`):

- **kid view**: big balance, "Tes opérations" list (label, date, amount
  colored green/red), form to record a dépense.
- **parent view**: one card per kid (name, monthly amount, balance),
  forms to create a kid, change a monthly amount, and per-kid page to add
  or delete transactions.

## Deployment

Docker image (`golang:1.27-alpine` → `alpine`, user 65532, `VOLUME /data`),
SQLite in `/data/pocket-money.db` (WAL) on a Kubernetes persistent volume.
Readiness probe on `/readyz`. Env: `LISTEN_ADDR`, `DATA_DIR`, `ADMIN_GROUP`,
`KIDS_GROUP`, `SECURE_COOKIES`. Deployment manifests live in the private
infrastructure repository.