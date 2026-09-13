# Postgres budget escrow

The policy store must initialize `policies` first. Use the same DSN and
`search_path` for both stores. This package uses the existing pgx dependency.

```go
func New(dsn string) (*Store, error)
func (s *Store) Initialize(ctx context.Context) error
func (s *Store) Close()
func (s *Store) Ready(ctx context.Context) error
func (s *Store) Sync(ctx context.Context, dataplane string, req policy.AuthorityRequest) (policy.SyncResponse, error)
```

`Ready` performs a live database ping with a two-second upper bound, including pool
acquisition, and honors a shorter caller deadline. It fails on a closed pool or
database outage; it does not rely on cached initialization state or change grants.

`Sync` returns a complete `policy.SyncResponse`: fresh validated policy documents,
their generation, current budget definitions, committed grants, acknowledgements,
denials, durable active tiers, and a heartbeat cadence. It never returns partial
results on error. The HTTP caller requires the machine heartbeat credential.
Within that trusted fleet, each node asserts its configured stable dataplane ID;
the shared token does not cryptographically bind a distinct node identity.
Report capabilities additionally bind grants to their original node/boot/request.
Compromised node operators are outside this enforcement boundary.

Every response also carries the complete, name-sorted policy set and generation
inside `response.Authority`. Inner and outer generations are identical. An empty
store returns non-nil empty `Authority.Policies` and `Authority.Budgets` slices,
encoded as `[]`, so clients can validate the bundle and explicitly clear policies.
No generation cache can omit a hard budget or suppress this empty-set update.

## Wire contract

- `Protocol` must be `policy.AuthorityProtocol` (`escrow-v1`). The request's
  `Instance` is the current boot. Report and meter `Instance` fields identify the
  **original** boot, including recovery reports.
- `RequestID` is a private, node-generated random capability of at least 32 bytes
  in its encoded form. Only its SHA-256 hash is stored. Errors never include the
  capability, connection string, or Postgres error detail.
- Grant identity is `(authenticated dataplane, Instance, RequestID)`. Its key,
  revision, window and requested amount are immutable, even following a denial.
  Reusing that identity with different parameters fails the entire batch.
- A new grant reserves `min(max(configured grant, WantMicroUSD), remaining)`.
  Remaining credit below `WantMicroUSD` is denied. A denial reserves nothing and
  the same immutable request can be retried after a proven refund. Once granted,
  replay returns its original ID, amount and expiry; expiry never re-arms it.
  A closed grant's request returns `closed_grant`.
- Reports require the original dataplane, original `Instance`, grant ID and
  capability. Sequence numbers start above zero and never decrease. Equal
  sequences require identical contents. Increasing sequences cannot decrease
  consumed or observed cost. Normal terminal reports cannot change their counters
  or flags; an identical terminal report may advance its sequence without
  refunding again.
- A report whose original `Instance` differs from the current request's instance
  can be recovering a stale journal. For an already-closed server grant, an
  unflagged terminal copy of settled consumption or a synthetic full-`Amount`
  burn does not recharge previously refunded escrow. Observed cost may increase
  only within the server's finalized consumption; the server retains its maximum
  observed value and sequence. It acknowledges the **incoming** sequence, even
  when older, so the restored outbox can retire that report. Authentication still
  requires the original dataplane, instance, grant ID and capability.
- An authenticated full burn from a previous boot also closes a server grant
  that is still OPEN, even when the restored sequence or observation is older.
  The server preserves its maximum sequence and observation, consumes the full
  grant without refunding it, and acknowledges the incoming sequence. Same-boot,
  partial and explicit-overrun reports retain the normal monotonicity checks.
- Report acknowledgements are
  `{ID: report.GrantID, Instance: report.Instance, Sequence: report.Sequence}`.
- Meters are cumulative per `(dataplane, original Instance, Key, WindowID)`.
  Meter acknowledgements are
  `{ID: meter.WindowID, Instance: meter.Instance, Sequence: meter.Sequence}`.
  The shared window ID includes the budget identity, so this distinguishes
  separate budgets in the same calendar period. Meters never mint hard credit.
  Old-boot recovery preserves the server maximum independently for sequence,
  consumed and observed values. Only a positive consumed delta adds liability;
  the incoming sequence is acknowledged so the restored outbox can retire.
  Current-boot conflicting or decreasing reports remain invalid.
- Active tiers carry both `Team` and optional `User` selectors. A user-only tier
  follows that opaque user across teams; a team+user tier requires both matches.
  Neither may become a team-wide substitution by dropping the user selector.
- `Consumed` and `Observed` must be nonnegative; normally `Observed <= Consumed`.
  A closed report with `Overrun` may exceed the grant and permanently freezes
  new grants in that window. It retains at least the full original grant, books
  `max(Consumed, Observed)`, and never produces a negative refund.
  A genuine overrun reported after normal closure adds only the difference from
  the already-finalized liability and freezes the window. It must satisfy the
  normal sequence and counter monotonicity checks; the recovery-burn exception
  never swallows an explicit overrun.
- Duplicate grant IDs, duplicate request capabilities, duplicate meter
  identities, invalid counters, unknown historical windows, arithmetic overflow
  or conflicting replays reject the entire batch. The batch limit is 1024 total
  requests/reports/meters.

## Accounting and locking

Each transaction uses READ COMMITTED, acquires `LOCK TABLE policies IN SHARE
MODE`, and parses the table again. Policy writers cannot interleave a change
between the read and grant commit. A mutex row serializes each dataplane's syncs;
all current and referenced historical accounts then lock in `(key, window)` order.
Historical grant reports resolve their original account independently of today's
policy set.

Database `clock_timestamp()` in UTC owns every window and expiry. Time is checked
again after account locks; a transaction that crossed a window boundary fails
retryably. Grant expiry is bounded by both its configured lease and window end.
The database never refunds for expiry, lost replies, node disappearance, or process
restart. Only a valid first closure returns proven unused authority.

Hard encumbrances, hard cumulative consumption and soft cumulative consumption
have separate durable columns. For hard admission, historical soft consumption
also remains a liability if a policy changes from soft to hard. Meter eligibility
is remembered for legitimate late soft reports. Hard tiers use encumbrances;
soft tiers use consumed accounting. Tier thresholds latch durably per routing
rule and owned budget window. Policy delete/re-add does not delete accounts,
request fingerprints, consumed amounts, freeze flags, or tier latches.

There is deliberately no pruning job in this implementation. Retention and
archival must preserve unresolved liabilities and replay identities.

## Validation

Set `INFERPLANE_TEST_PG_DSN` privately to the disposable local PostgreSQL 17 DSN,
then run:

```sh
go test -race -count=1 ./internal/authority/pgstore
go vet ./internal/authority/pgstore
```

Every database test creates a random schema and sets its connection's
`search_path`. Cleanup drops only that schema. Tests never wipe shared tables.
Without the environment variable, database tests skip and the default suite needs
no database or credentials.

The tests exercise independent concurrent issuer pools, real expiry, store
reopening, exact grant retries, conflicting intent, report ownership and sequences,
one-time refunds, atomic rollback after a report write, policy-writer blocking,
fresh policy cuts, delete/re-add, soft metering, tier persistence, overflow,
overrun freeze, historical rollover fixtures, and two real SQLite journals talking
to interchangeable Postgres stores across journal and control-plane restarts.
