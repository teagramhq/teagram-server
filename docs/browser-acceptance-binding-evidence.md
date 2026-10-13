# Browser acceptance binding evidence

The supported producer version is **1**. Apply
`20261013000083_browser_acceptance_binding_evidence.sql` before using this API.
The migration enrolls no account. It leaves the three existing probe functions
available; those functions cannot be combined to obtain an atomic evidence cut.

## Read contract

Call the following once for each cut, as `browser_acceptance_probe`, with a
bound bigint account parameter:

```sql
SELECT complete, evidence_version, epoch, additions, removals,
       last_added_created_at, bound_key_ids, bound_key_created_ats,
       pending_count, db_time
FROM public.browser_acceptance_binding_snapshot_v1($1::bigint);
```

The function always returns exactly one row. It uses one SELECT under the
caller's statement snapshot, is `LANGUAGE SQL STABLE SECURITY DEFINER`, and
pins `search_path = pg_catalog` with fully qualified relation names.

| Field | SQL type | Meaning when complete |
| --- | --- | --- |
| `complete` | `boolean` | Enrolled evidence at version 1 and both coverage triggers enabled always |
| `evidence_version` | `integer` | Pinned producer version, 1 |
| `epoch` | `bigint` | Enrollment generation, never reused by the running database |
| `additions` | `bigint` | Committed binding additions since enrollment |
| `removals` | `bigint` | Committed binding removals since enrollment |
| `last_added_created_at` | `timestamptz` | Last addition's `NEW.created_at`; NULL until an addition |
| `bound_key_ids` | `bigint[]` | Current bound key IDs, sorted by ID |
| `bound_key_created_ats` | `timestamptz[]` | Creation times aligned by array position with `bound_key_ids` |
| `pending_count` | `bigint` | Current rows with `pending_user_id` equal to this account |
| `db_time` | `timestamptz` | `statement_timestamp()` for this cut |

Both arrays are empty, non-NULL arrays when an enrolled account has no bound
keys. Preserve bigint and timestamp precision when decoding and comparing cuts.
Compare the zipped `(id, created_at)` pairs, including the timestamps.

An absent account, an unenrolled account, an unsupported evidence version, or a
missing or incorrectly enabled coverage trigger returns `complete = false`
and **NULL in every other field**. Incomplete cuts cannot establish acceptance.
The API checks each trigger's relation, function, event coverage and
`tgenabled = 'A'`; the bound-row trigger covers INSERT, UPDATE and DELETE, and
the separate statement trigger refuses TRUNCATE, including replication-role
writes.

The probe has EXECUTE only on this new read function. PUBLIC cannot execute any
of the new functions. The probe has no evidence table, sequence, enrollment,
trigger-function or write privileges. The owner retains maintenance authority.

Epoch, totals, witness and DB time are **memory-only consumer data**. Never
print them to stdout/stderr, serialize them into reports or persist them.
Failures must report the failed condition without those values. The API itself
does not log a cut or raise notices containing these values.

## Owner enrollment and reinitialization

Provision exactly one acceptance account using the migration owner role:

```sql
BEGIN;
SELECT public.browser_acceptance_enroll_binding_evidence($1::bigint);
COMMIT;
```

Run this as standalone maintenance, outside application binding transactions.
The owner-only, invoker-security helper first takes
`LOCK TABLE public.auth_keys IN SHARE MODE`. It waits for every earlier writer
to finish before deleting this account's previous evidence and inserting its
replacement in the same transaction. It draws the epoch from the owner-only,
non-cycling bigint sequence *after* taking the lock, sets both totals to zero,
and sets the witness to NULL. Existing bindings establish the baseline; no
history is inferred or backfilled. Failure rolls back the entire initialization.
Sequence values consumed by aborted attempts are skipped.

Calling the helper again for the same account performs delete/re-enroll and
changes the epoch. Calling it for another account while one is enrolled raises
SQLSTATE `23505`, including concurrent attempts, without replacing the enrolled
account. Evidence references `users(id) ON DELETE CASCADE`; successful account
deletion removes the evidence. Epoch exhaustion fails rather than recycling.

Direct owner maintenance must obey the same writer barrier before changing or
deleting evidence, draw a new sequence epoch for reinitialization, and invalidate
any outstanding cuts. There is no RPC, application enrollment path or probe
enrollment grant.

## Transitions and acceptance

The row trigger changes evidence inside the binding transaction. A changed
bound `(id, created_at, user_id)` tuple removes the old owner's tuple and adds
the new owner's tuple if that owner is enrolled. Metadata updates, touches,
unchanged rebindings and unenrolled owners create no totals. Pending login is
sampled; promotion to a binding counts an addition.

Each addition stores its creation-time witness in the same update as its total.
Removal leaves that witness intact, even if the key has disappeared. Ordinary
bigint addition refuses overflow. Evidence failure aborts the binding statement;
rollback advances neither total. Retention is limited to the account reference,
version, epoch, two totals and one timestamp. No key IDs, credentials, sessions
or event history are retained in evidence.

The lock order remains owner advisory lock where applicable, all affected
auth-key rows, the single evidence row, then SRP challenge rows. The evidence
trigger is AFTER ROW and its quoted `AA_` name precedes PostgreSQL's
`RI_ConstraintTrigger_*` cascade triggers.

For acceptance, both cuts must be complete at version 1 and have the same
epoch. Require exact addition/removal deltas `(1, 1)`, a non-NULL witness at or
after the pre-cut DB time, identical bound pairs before and after, and pending
login back at baseline or expired. A fresh key can bind and log out entirely
between the cuts. Two such keys produce `(2, 2)`; removing/restoring a baseline
key, rebinding an old unbound key, or reassigning a baseline key away/back leaves
an old witness. A new key still bound changes the final bound set. A transaction
whose key creation precedes the cut but commits afterward can only fail closed.

Owner/superuser tampering and backup restoration between cuts remain outside
this proof. Application rollback should leave this additive schema installed.
Removing the expansion is reversible with loss of feature-only evidence and its
epoch sequence, so it invalidates all prior cuts; there is no automatic down
migration. The expansion does no auth-key rewrite or history backfill. Its DDL
briefly locks `auth_keys` to install the triggers; enrollment's writer barrier
lasts through the maintenance transaction.
