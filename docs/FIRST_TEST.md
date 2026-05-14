# NanoTDB – First Test

This is the **first test** that defines when NanoTDB is considered "working".
Do not add features before this test passes reliably.

---

## Goal

Prove that NanoTDB:
- survives crashes at arbitrary points
- never corrupts data
- correctly replays WAL

---

## Test: Crash Safety Smoke Test

### Setup

1. Create a new empty database directory
2. Start NanoTDB
3. Generate inserts for multiple values at different rates

Example (pseudo):
```
value A: 50 samples
value B: 5 samples
value C: 1 sample
```

---

### Execution Loop

Repeat this loop many times:

1. Insert a small batch of samples
2. Randomly sleep (0–20 ms)
3. Randomly crash the process (`kill -9`)
4. Restart the database
5. Query all values

---

### Assertions (these MUST hold)

- Database starts without error
- No panics during recovery
- No catalog corruption
- Pages never partially readable
- Returned data is consistent:
  - Missing recent samples is acceptable
  - Corrupted samples is NOT acceptable
- Per metric, timestamps are non-decreasing in query results
- Equal timestamps preserve append order
- WAL replay never violates metric type constraints
- WAL replay never creates cross-metric contamination
- WAL reset happens only after corresponding page data is flushed
- Metric identifier string -> `MetricID` mapping remains stable across restarts
- New metric identifiers receive new `MetricID`s (never reused)

---

## Acceptance Criteria

This test passes if:
- You can repeat the above loop hundreds of times
- At random crash points
- Without ever corrupting `.dat`, catalog, or WAL files

Once this test passes:
✅ WAL ordering is correct
✅ fsync boundaries are correct
✅ Recovery logic is correct

Only after this should new features be added.

---

## Automated Script

Use the crash-loop script:

```bash
python3 scripts/first_test_chaos.py \
  --iterations 200 \
  --listen 127.0.0.1:18428 \
  --data-dir ./devdata-first-test
```

What it does:
- starts `cmd/nanotdb`
- ingests multiline points via `/api/v1/import`
- waits a random short interval
- kills process (`SIGKILL`)
- restarts and verifies query responses are consistent (no corruption)

Notes:
- Missing latest samples after crash is tolerated.
- Corrupted samples, broken JSON responses, or inconsistent ordering fail the test.

