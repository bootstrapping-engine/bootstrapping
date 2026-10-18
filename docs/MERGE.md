# The merge rules

This is the contract. The random-history test in `promise_test.go` checks
every apply against these rules, worked out from its own record of what was
applied, and scripts/record-tests.sh records the result.

Three things are compared for every row, by its natural key:

- **S**, the row in the new seed
- **L**, the row in the table now
- **B**, the row as last applied, from the ledger

## Rows

| In S | In L | In B | What happens |
|---|---|---|---|
| yes | no | no | insert |
| yes | yes | no | adopt: compare values with no B (below), then record |
| yes | yes | yes | merge the values (below) |
| yes | no | yes | deleted locally: stays deleted, or reinserted with `on_local_delete = "restore"` |
| no | yes | yes | dropped from the seed: `on_remove` decides (keep, delete, retire) |
| no | no | yes | gone from both: forgotten |
| no | yes | no | never managed: not touched |

A dropped row with `on_remove = "delete"` is a conflict instead of a delete
when any merge column differs from B, including columns B managed that S
no longer names. It is skipped instead of deleted while any row will still
refer to it after the apply (a row that stays, an admin's row, or another
row skipped for the same reason), and deleted on a later apply once none
does. Retiring never needs to ask, since it
destroys nothing. A row already retired is not touched again until it comes
back into the seed, when its retire column goes back to NULL.

## Values

For each managed column outside the key, with s, l and b its values in S,
L and B (b is NULL for a column B didn't manage):

| Ownership | Condition | What happens |
|---|---|---|
| any | l = s | nothing |
| `local` | l ≠ s | nothing; the column is the admin's |
| `seed` | l ≠ s | update to s |
| `merge` | no B (adopting a row) | conflict |
| `merge` | s = b | keep the local edit |
| `merge` | l = b | update to s |
| `merge` | otherwise | conflict |

Equal means equal as SQLite stores it: same storage class, same value.

## After an apply

The ledger records S as applied for every row in the seed, including rows
whose conflict was settled by keeping the local value. So a settled
conflict doesn't come back: next time s = b, and the local value counts as
a local edit.

Two exceptions keep the ledger honest. A value that wasn't written (a
conflict that blocked the apply, or a reference that couldn't be resolved)
keeps its old B. And a row that was skipped isn't recorded at all, so the
next apply tries again.

A blocked apply writes nothing at all, not even the parts that didn't
conflict.

## Order of writes

In one transaction, holding SQLite's write lock from the start:

1. Deletes, in reverse dependency order (links before what they link to),
   except rows something still points at before the apply.
2. For each table in dependency order: renames, inserts, other updates and
   retires, then any references from the table to itself.
3. The deletes held back in step 1, now that nothing points at them: a row
   held back holds back the rows it points at too. Within a table, a row
   goes before the rows it points at; rows that point at each other in a
   loop have those references set to NULL first.
4. The ledger, the change log and the run record.

So with foreign keys on, no statement in an apply breaks one.
