# kids-pocket-money

This program is a standalone app to manage our kids pocket money.
It replaces an old sheet.

Several objectives:
- kids should be able to see how much money they have at the moment
- kids should be able to see past transactions they've done
- parents should be able to write how much money each kids get every month
- it should integration with a simple authentication mechanism (in our case authelia which provides Remote-User and Remote-Groups headers)

## How it works

Single Go binary (stdlib `net/http`, no framework) with a SQLite database
(`modernc.org/sqlite`, pure Go, no cgo). The detailed design lives in
[DESIGN.md](DESIGN.md). In short:

- **Kids** open `/`, see their balance and their history, and can record
  their own operations (a dépense or an ajout, with a label).
- **Parents** (members of the `ADMIN_GROUP`, e.g. `parents`) get the family
  dashboard: one card per kid with balance and monthly amount, a form to add
  a kid, and a per-kid page to change the monthly amount, add or delete
  transactions.
- **Monthly allowance**: parents set the amount once; every month is then
  automatically credited as an `allowance` transaction (including months
  during which nobody opened the app, up to the current month). Changing
  the amount only affects future months.
- All amounts are stored in **integer cents** and displayed in euros.

## Authentication

The app is meant to run behind [authelia](https://www.authelia.com) (or any
reverse proxy that sets the same headers):

- `Remote-User` (mandatory, otherwise 401): the username,
- `Remote-Groups`: comma-separated groups; parents are detected through
  `ADMIN_GROUP` (default `parents`),
- `Remote-Email` (optional).

A kid account is recognised when `Remote-User` matches the `username` of a
kid created by a parent. Unknown users get 403.

## Configuration

| Env var | Default | Description |
|---------|---------|-------------|
| `LISTEN_ADDR` | `:8080` | HTTP listen address |
| `DATA_DIR` | `/data` | directory for the SQLite database (`$DATA_DIR/pocket-money.db`) |
| `ADMIN_GROUP` | `parents` | `Remote-Groups` group that grants the parent view |
| `KIDS_GROUP` | `kids` | `Remote-Groups` group that grants the kid view |
| `SECURE_COOKIES` | `true` | mark the CSRF cookie `Secure` (keep `true` behind TLS) |

Access is denied (401/403) unless `Remote-User` is set and `Remote-Groups`
contains `ADMIN_GROUP` or `KIDS_GROUP`; only `/healthz` and `/readyz` are
public. Kids only ever see their own money; only parents can add money to
an account.

## How to run

This is a go program which uses a sqlite database.

```sh
go build .
LISTEN_ADDR=:8080 DATA_DIR=/data ./kids-pocket-money
```

Docker:

```sh
docker build -t kids-pocket-money .
docker run --rm -p 8080:8080 -v pocket-money:/data kids-pocket-money
```

In production it runs on Kubernetes with a persistent volume for the
SQLite database, behind a reverse proxy that sets the `Remote-*` headers
(authelia).

## Development

```sh
go test ./...
```

The web UI is in French: server-rendered templates, no JavaScript
framework, dark mode support, CSP with nonces, CSRF protection.