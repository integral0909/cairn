# Changelog

## 0.1.0 (2026-11-21)

First release.

- Pager with a write-ahead log, crash recovery, snapshot reads, checkpoints,
  a free list and statement savepoints
- B+trees with overflow pages, merging on delete, and two-way cursors
- SQL parser for DDL, DML, queries, transactions and EXPLAIN
- Planner with condition pushdown, rowid and index access paths, and index
  nested loop, hash and nested loop joins
- Executor for joins, subqueries, aggregates, sorting, DISTINCT, LIMIT and
  set operations
- Strict column types, constraints, OR REPLACE / ON CONFLICT DO NOTHING,
  RETURNING, ALTER TABLE
- Integrity checker
- Go API and database/sql driver
- `cairn` shell, `exec`, `check` and a web console (`serve`)
