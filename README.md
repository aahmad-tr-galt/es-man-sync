# es_man_sync

A manual MySQL-to-Elasticsearch sync utility for TestRail Cloud. Used to rebuild or backfill the Elasticsearch index for a specific instance by reading directly from its MySQL database, bypassing the normal `es_sync` → Redis → `es_sync_consumer` pipeline.

## When to Use

- The automated pipeline has fallen behind or stopped for a specific instance and the gap needs to be closed immediately.
- `es_index_consumer`'s full dump failed (MySQL 8 compatibility issue) — the index was created but left empty.
- A specific instance's ES data needs to be rebuilt from MySQL without triggering a full pipeline reindex.

## What It Does

1. Connects to the `testrail_meta` Aurora DB to look up the instance's DB credentials.
2. Connects directly to the instance's MySQL database.
3. Iterates over all 7 tracked tables: `projects`, `cases`, `runs`, `milestones`, `suites`, `reports`, `cross_project_reports`.
4. For each table, uses cursor-based pagination (`WHERE id > lastID ORDER BY id ASC LIMIT 1000`) to read all rows.
5. Indexes each row individually into Elasticsearch with a **500ms sleep between documents** (throttle) and a **10s sleep between tables**.
6. Document IDs are in the format `{row_id}_{table_name}` — identical to what the live pipeline produces, so documents written here are compatible with and will be overwritten by the pipeline going forward.

## Configuration

The source contains two config blocks — production (active by default) and staging (commented out). To target staging, swap the comments in `main.go` before building:

```go
// Comment out the prod block and uncomment the stage block
```

## Building

Build for Linux (for deployment into a Kubernetes pod):

```bash
GOOS=linux GOARCH=amd64 go build -o es_man_sync .
```

### Build Variants

Because the `cases` table is typically the largest and most likely to cause a mid-run crash (see [Known Issue](#known-issue-connection-closed-on-large-tables) below), it is often useful to build targeted variants by modifying `main.go` before building:

| Variant | How to build | Purpose |
|---|---|---|
| `es_man_sync` | All tables in `TrTables` | Full sync for a fresh/empty index |
| `es_man_sync_no_case` | Remove `"cases"` from `TrTables` | Sync everything except cases; run first on large instances |
| `es_man_sync_only_case` | Keep only `"cases"` in `TrTables` | Sync cases in isolation after other tables are done |
| `es_man_sync_only_case_from_<N>` | Keep only `"cases"`, set `lastID = N` as the initial cursor | Resume a cases sync from a specific row ID after a crash |

The typical workflow for a large instance is:

```
1. Run es_man_sync_no_case   → syncs all tables except cases quickly
2. Run es_man_sync_only_case → syncs cases; if it crashes mid-way, note the last synced ID from the log
3. Run es_man_sync_only_case_from_<lastID>  → resumes cases from where it left off
```

## Deploying to a Pod

Copy the binary into a `testrail-meta` pod (it has network access to both Aurora and ES):

```bash
kubectl cp es_man_sync <namespace>/<pod-name>:/tmp/es_man_sync
kubectl exec -it -n <namespace> <pod-name> -- chmod +x /tmp/es_man_sync
```

## Running

Run in the background with output redirected to a log file:

```bash
kubectl exec -it -n <namespace> <pod-name> -- bash
/tmp/es_man_sync <instance_id> > /tmp/es_man_sync_<instance_id>.log 2>&1 &
```

Example:

```bash
/tmp/es_man_sync 1042 > /tmp/es_man_sync_1042.log 2>&1 &
```

Note the PID so you can confirm the process is still running (`kill -0 <pid>`).

## Monitoring Progress

Tail the log file from inside the pod:

```bash
tail -f /tmp/es_man_sync_1042.log
```

Each synced document produces a log line:

```
2024/01/15 10:23:01 Synced cases with ID 5821_cases
2024/01/15 10:23:02 Synced cases with ID 5822_cases
```

The tool completes silently (no final success message). If it exits without an error log line, it finished successfully.

## Known Issue: Connection Closed on Large Tables

The tool can crash mid-run on instances with large tables, typically `cases`. The error will look something like a MySQL connection closed or broken pipe error.

**Root cause:** The tool holds a MySQL result set open (`db.Query()`) while sleeping 500ms between each row. With batches of 1000 rows, a single batch can hold the connection open for up to ~500 seconds. MySQL's `wait_timeout` on Aurora (typically 60–120s) will close a connection that appears idle for that long, causing `rows.Scan()` or `rows.Next()` to fail mid-iteration.

**Workarounds:**

- **Re-run freely** — The tool is fully idempotent. Document IDs are deterministic, so re-running overwrites already-synced documents and continues the pagination from the start of the current batch.
- **Use the build variants workflow** — Sync all non-`cases` tables first with `es_man_sync_no_case`, then target `cases` alone. If the cases sync crashes, check the last `Synced cases with ID` line in the log, note the numeric ID, build a `_from_<id>` variant with that as the starting cursor, and resume from there.
- **Run during off-peak hours** — Aurora is less likely to aggressively reclaim connections when overall DB load is low.
