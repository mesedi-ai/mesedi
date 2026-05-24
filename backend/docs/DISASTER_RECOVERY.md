# Disaster recovery runbook

Backups and restore procedure for the Mesedi production backend on
Fly.io. Refreshed: 2026-05-24 (#129).

## What gets backed up

The production database is SQLite on a Fly persistent volume at
`/data/mesedi.db`. Fly automatically snapshots this volume daily.
Snapshot retention is set to **14 days** (raised from the 5-day
default in #129). Each snapshot is a point-in-time copy of the
entire volume, taken atomically while the app is running.

Snapshots are stored by Fly in the same region as the volume (`iad`,
Ashburn VA) on independent infrastructure from the live volume.

When the Postgres migration ships (#128), this runbook gets a
parallel "Postgres branch" using Neon's PITR (point-in-time-restore)
plus periodic dumps to S3.

## Verify backups are still configured

Run periodically (monthly is fine):

```
fly volumes list -a mesedi-api
fly volumes show <vol_id> -a mesedi-api
```

Expected output includes:

- `Scheduled snapshots: true`
- `Snapshot retention: 14`

If either is wrong, fix immediately:

```
fly volumes update <vol_id> -a mesedi-api --snapshot-retention 14
```

## List available snapshots

```
fly volumes snapshots list <vol_id> -a mesedi-api
```

Returns each snapshot's ID, size, status, and creation time. Use
the snapshot ID for restore.

## Create a manual snapshot

Before any risky operation (Postgres cutover, schema migration,
bulk data delete), trigger a manual snapshot first:

```
fly volumes snapshots create <vol_id> -a mesedi-api
```

The snapshot is captured asynchronously; check status with the list
command above. Manual snapshots count against the 14-day retention
window the same as scheduled ones.

## Restore from a snapshot

There is no in-place restore for a Fly volume. The flow is:

1. Identify the snapshot ID you want to restore from
   (`fly volumes snapshots list ...`).
2. Create a NEW volume from that snapshot:

   ```
   fly volumes create mesedi_data_restore \
     --snapshot-id <snapshot_id> \
     --region iad \
     --size 1 \
     -a mesedi-api
   ```

3. Stop the current backend machine:

   ```
   fly machines stop <machine_id> -a mesedi-api
   ```

4. Detach the corrupted volume (do NOT destroy it yet, keep for
   forensics until the restore is confirmed healthy):

   ```
   fly machines update <machine_id> -a mesedi-api \
     --volume-name mesedi_data_restore:/data
   ```

   This swaps the mount; the corrupted volume becomes orphaned.

5. Restart and verify:

   ```
   fly machines start <machine_id> -a mesedi-api
   curl https://mesedi-api.fly.dev/health
   ```

   Then spot-check the data: open the dashboard, confirm executions
   and failure groups are visible.

6. Once verified healthy, destroy the orphaned corrupted volume:

   ```
   fly volumes destroy <old_vol_id> -a mesedi-api
   ```

Restore RTO target: under 15 minutes. RPO is at most 24 hours
(daily snapshot cadence). When Postgres lands, RPO drops to minutes
via PITR.

## What this does NOT cover

- **Application-level corruption** (a bug that writes wrong data).
  Snapshots will faithfully restore the bad data. Defense is good
  test coverage on writes, not backups.
- **Off-region disaster**. If all of `iad` goes dark, the snapshot
  is also offline. Postgres on Neon with the eu-central read replica
  is the upgrade path for this.
- **The Fly account itself**. If the Fly account is suspended or
  the org is deleted, the snapshots go with it. Out of scope for
  v0.1; the long-term answer is periodic dumps to an external bucket
  (S3 / R2) outside the Fly blast radius.

## Quick reference

| Command | Purpose |
|---|---|
| `fly volumes list -a mesedi-api` | List volumes, find the ID |
| `fly volumes show <vol_id> -a mesedi-api` | Confirm snapshot policy |
| `fly volumes snapshots list <vol_id> -a mesedi-api` | List available restores |
| `fly volumes snapshots create <vol_id> -a mesedi-api` | Manual snapshot now |
| `fly volumes create <name> --snapshot-id <id> --region iad --size 1 -a mesedi-api` | New volume from snapshot |
| `fly machines update <id> -a mesedi-api --volume-name <name>:/data` | Swap volume mount |

## Volume identifiers (as of 2026-05-24)

- Production: `vol_4y889xzpe693pg9r` (mesedi_data, 1GB, iad, encrypted)
