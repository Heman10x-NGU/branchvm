# auth-demo

A small signup and login API on Postgres 16. branchvm branches run its test suite inside microVMs.

Endpoints: `POST /signup`, `POST /login`, `GET /me` (HMAC bearer token). It reads `DATABASE_URL`
(default `postgres://postgres@127.0.0.1:5432/authdemo?sslmode=disable`).

It is a separate Go module with vendored dependencies, so it builds offline and the repo root
`go test ./...` does not run it.

## The bug

`CreateUser` in `store.go` checks for the email with a `SELECT count(*)` and then runs `INSERT`.
Concurrent signups for one email all pass the check. The unique index on `users.email` keeps one
row, but the losers get a raw unique violation (SQLSTATE 23505) that the handler reports as a 500
instead of a 409. `TestSignupConcurrentDuplicate` catches this. The other 13 tests pass.

## Two fixes

- `fixes/branch2.patch`: `INSERT ... ON CONFLICT (email) DO NOTHING RETURNING id`, and no returned
  row means the email is taken.
- `fixes/branch3.patch`: keep the insert and map a `pq.Error` with code 23505 to `ErrEmailTaken`.

Either patch makes all 14 tests pass.

## Running

```
BRANCH_INDEX=1 ./demo-agents.sh   # fails on the race test
BRANCH_INDEX=2 ./demo-agents.sh   # 14/14 with the ON CONFLICT fix
BRANCH_INDEX=3 ./demo-agents.sh   # 14/14 with the 23505 mapping fix
```

Tests skip when Postgres is unreachable. The script makes no network calls and costs nothing.
With `BRANCHVM_USE_CLAUDE=1` and a `claude` binary on PATH, it prints the command a real agent
run would use and exits.
