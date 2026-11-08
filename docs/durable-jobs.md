# Durable transcription jobs

PostgreSQL is the source of truth. The existing chats allowlist is preserved.
`bot_id` (from Telegram GetMe) namespaces the cursor, inbox, preferences and jobs.
The same database cannot run two pollers for one bot simultaneously. Telegram
itself still requires only one active polling instance for each token.

## Flow and recovery

1. The complete getUpdates batch and its next offset commit in one transaction.
   The next request acknowledges only that committed batch. A failed database
   session stops intake; Docker can restart the process without losing the batch.
2. A serial inbox processor applies membership/preferences and authorizes media
   in update order. Job insertion and inbox completion commit together. Raw update
   payload is then replaced with `{}`; deduplication keys remain. Duplicate
   update IDs and duplicate chat/message pairs cannot create additional jobs.
3. Jobs persist source IDs, immutable Telegram file ID/format, resolved preset,
   `transcriber-bot:<bot_id>:<update_id>` request ID and the status message receipt.
   State transitions are `status → upload → poll → deliver → done`.
4. Interrupted uploads are downloaded again using the same file ID and submitted
   with the same options/request ID and `retain_until_ack=true`. Lost Submit
   responses recover the backend job; request IDs are never reused for new audio.
5. Terminal results/errors and all output parts commit atomically to the local
   database. Only then may the bot Ack the backend. A lost Ack response retries;
   backend NOT_FOUND on Ack is safe because the local result already exists.
6. Each confirmed Telegram part has its receipt persisted separately. Delivery
   continues from local storage even if backend Ack is temporarily unavailable.
   `done` requires both backend acknowledgement and all Telegram receipts.

A deleted status message falls back to a new reply. Replies permit a missing
original message. A retry of an unchanged edit counts as success. Lecture files,
long multi-part text, terminal errors and cancellations use the same outbox.
Cancellation is persisted and replayed against the backend after restart. It
requires the owner and the stored job's chat. A race with completion may deliver
the already completed result rather than claim a successful cancellation.

### Quiet groups

Group/supergroup jobs persist `group_chat` from the Telegram chat type. Migration
`0003_quiet_groups.sql` backfills this flag for existing negative group chat IDs
and extends the delivery outbox with idempotent `delete` operations; existing
jobs, results and delivery receipts are preserved.

Before the initial group status, `GetAvailability` must succeed within three
seconds and report an enabled worker. Otherwise the job becomes locally terminal
without a Telegram message, download or backend upload. It does not resume when
workers return; the user must send that recording again. Busy workers still
accept queueing. Availability requires a report within 45 seconds and, in
RabbitMQ mode, valid consumer admission. It is advisory, not a reservation.

If all workers disappear while a submitted group job is queued/running, or
status RPCs fail, cancellation is persisted before deleting the status. Backend cancellation,
terminal result storage and Ack retry after recovery, preventing abandoned
retained backend results. A group cancellation also hides the status if the
backend is temporarily unreachable. Group terminal errors use a durable delete
delivery instead of an error message; this also applies to old persisted error
delivery plans. Missing/already deleted messages count as successful deletion.
Other deletion failures retry without posting a replacement. Telegram deletion
permissions and the [48-hour API limit](https://core.telegram.org/bots/api#deletemessage) still apply.

Successful group results are unchanged. A persisted group cancellation suppresses
delivery even if completion raced with it. Private chats retain their original
queue/error/confirmation behavior. Short RPC deadlines are three seconds and
polling uses two seconds; upload and inference deadlines are not shortened.

## Guarantees and limitations

- Session advisory locks serialize each job's network stage across processes;
  database state is written through that very connection. Connection loss fences
  old writes. Locks release automatically on a crash. No transaction stays open
  while waiting for Telegram, downloads or inference.
  The dedicated poller session expires after 120 seconds of database inactivity
  (Telegram requests have a 70-second HTTP deadline). A replacement waits for
  existing ownership instead of entering a restart loop. TCP keepalives bound
  detection of disconnected database clients; worker sessions have no idle limit
  because downloading a large file can take much longer.
- Status and upload stages remain FIFO per chat. Two workers per stage are
  bounded; only two upload workers download/submit media. Polling and delivery
  can overtake an earlier running job. The database pool has at least twelve
  connections for these workers, admission and the dedicated poller lock.
  Bot/PostgreSQL container limits are 256 MiB and 0.5 CPU each (formerly
  64 MiB/0.1 CPU); the bot uses GOMEMLIMIT=192MiB. This allows the connection
  pool and transcript serialization while keeping per-stage concurrency bounded.
- Retry delays are 2–256 seconds. Telegram retry_after is respected up to one
  day. No attempt limit or three-hour timeout discards accepted work. Permanent
  Telegram access failures remain pending with backoff until access is restored.
- Missing retained backend jobs are reported through the durable outbox rather
  than silently waiting forever or creating a new operation. Recovery cannot
  reconstruct a result deleted outside the backend retention contract.
- Telegram has no idempotency key for sendMessage/sendDocument. An ambiguous
  response or a crash before storing its receipt may duplicate an initial status,
  replacement message, extra part or lecture file. Confirmed parts are not sent
  again. This is at-least-once delivery, not exactly-once external effects.
- Command UI replies and callback popup acknowledgements are best effort. Their
  persistent preference/cancellation effects survive restarts. A18 group policy
  changes and I02 job history UI are separate work.
- No automatic retention deletes jobs/results or inbox deduplication keys in this
  iteration. Include this database in protected offsite backups and monitor its
  size. The persisted media IDs and transcript content are private user data.
  Tests and ordinary logs do not print transcripts, tokens or database URLs.

## Migrations, rollout and rollback

Ordered SQL files live under `bot/store/sql/migrations`. A transaction and advisory
lock protect the migration ledger. Startup migration has a two-minute overall
budget, a 60-second per-statement timeout and a 30-second lock timeout; these
transaction-local settings do not change the five-second normal query budget. The baseline adopts the existing chats table;
new tables are additive. Applied migrations must not be edited. Startup applies
migrations to preserve this project's established deployment contract. Explicit
application is also available via `DATABASE_URL=... make migrate` or the running
release's `/bot migrate` command. That command does not authenticate to Telegram.

Before first rollout, let the old in-memory bot finish its pending uploads and
results, verify backend queue activity, and take a protected backup of the bot
PostgreSQL. There is no durable old job/chat mapping to import. Restart recovery
covers updates accepted by this version; it cannot recover mappings already lost
by an older process. Do not run a second poller as a migration method.

The backend must support request_id, retention, Ack and GetAvailability before
enabling this bot. Revision `57eab2b` adds availability; the earlier `e60cba0`
already supports durable delivery. Canonical protocol source/revision/hash are
pinned in `proto/source.json`; `make proto-sync` is an explicit authenticated
GitHub CLI operation. `make proto-gen` generates offline from the snapshot.
`make proto-check` verifies its hash, formatting, build and lint. Generation in
Docker uses the same pinned tools and Make target. No backend checkout is needed.

Rollback to the old image is safe only after intake is quiesced and no unhandled
inbox or pending jobs remain. The old image ignores the new durable tables and
must not be used to drain them. Preserve the additive schema and database rather
than reversing migrations or restoring an older database over newer jobs.

Read-only operational counts (no transcript or user identifiers):

```sql
SELECT state, count(*), min(created_at) FROM transcription_jobs GROUP BY state;
SELECT count(*) FROM bot_inbox WHERE NOT handled;
SELECT max(last_poll_at) AS last_successful_poll FROM bot_cursors;
SELECT count(*) FROM transcription_deliveries WHERE NOT delivered;
```

## Verification

`make check` runs generation, offline snapshot checks, Go tests/vet and Compose
validation. Database tests require a synthetic isolated PostgreSQL supplied via
`TRANSCRIBER_BOT_TEST_DATABASE_URL`. `make test-integration` fails if it is absent;
CI provisions the pinned image via `make test-db-up`, runs integration and race
tests, and always removes the fixture. Each test uses its own random schema.

Tests cover atomic intake/cursor, duplicate admission, old allowlist adoption,
job locking and stale-connection fencing, restarts at each stage, actual polling
shutdown/restart during downloads, lost Submit/Ack responses, persisted partial
text/file delivery, deleted status messages, not-modified edits, cancellation,
HTML escaping and later results overtaking earlier inference. Telegram and
inference are synthetic; no real messages, model downloads or production data
are used.
