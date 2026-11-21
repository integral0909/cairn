package engine

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/integral0909/cairn/internal/pager"
	"github.com/integral0909/cairn/internal/value"
)

func openFS(t *testing.T, fs *pager.MemFS, opts pager.Options) *DB {
	t.Helper()
	p, err := pager.Open(fs.Open("db"), fs.Open("wal"), opts)
	if err != nil {
		t.Fatal(err)
	}
	db, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// Random SQL transactions, a power cut at a random moment, then reopen:
// the database must hold exactly the committed transactions, with indexes
// that match their tables.
func TestCrashRecoveryWithSQL(t *testing.T) {
	ctx := context.Background()
	for seed := int64(1); seed <= 40; seed++ {
		r := rand.New(rand.NewSource(seed))
		fs := pager.NewMemFS()
		db := openFS(t, fs, pager.Options{CheckpointFrames: 5 + r.Intn(60)})
		c := db.Conn()
		mustRun := func(q string, args ...value.Value) {
			if _, err := c.Exec(ctx, q, args...); err != nil {
				t.Fatalf("seed %d: %s: %v", seed, q, err)
			}
		}
		mustRun(`CREATE TABLE t (id INTEGER PRIMARY KEY, k TEXT UNIQUE, v INTEGER)`)
		mustRun(`CREATE INDEX t_v ON t (v)`)
		model := map[int64]string{}
		next := int64(1)
		for txn := 0; txn < 1+r.Intn(25); txn++ {
			mustRun(`BEGIN`)
			pending := map[int64]string{}
			for k, v := range model {
				pending[k] = v
			}
			for op := 0; op < 1+r.Intn(20); op++ {
				if len(pending) > 0 && r.Intn(3) == 0 {
					for id := range pending {
						mustRun(`DELETE FROM t WHERE id = ?`, value.NewInt(id))
						delete(pending, id)
						break
					}
					continue
				}
				k := fmt.Sprintf("k%d-%s", next, strings.Repeat("p", r.Intn(400)))
				mustRun(`INSERT INTO t (id, k, v) VALUES (?, ?, ?)`, value.NewInt(next), value.NewText(k), value.NewInt(int64(r.Intn(10))))
				pending[next] = k
				next++
			}
			if r.Intn(4) == 0 {
				mustRun(`ROLLBACK`)
				continue
			}
			mustRun(`COMMIT`)
			model = pending
		}
		crashed := fs.Crash(r)
		db2 := openFS(t, crashed, pager.Options{})
		c2 := db2.Conn()
		rows, err := c2.Query(ctx, `SELECT id, k FROM t ORDER BY id`)
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		got := map[int64]string{}
		for {
			row, err := rows.Next()
			if err != nil {
				t.Fatal(err)
			}
			if row == nil {
				break
			}
			got[row[0].I] = row[1].S
		}
		if len(got) != len(model) {
			t.Fatalf("seed %d: %d rows after recovery, want %d", seed, len(got), len(model))
		}
		for id, k := range model {
			if got[id] != k {
				t.Fatalf("seed %d: row %d lost or changed", seed, id)
			}
		}
		problems, err := db2.Check()
		if err != nil || len(problems) > 0 {
			t.Fatalf("seed %d: integrity: %v %v", seed, err, problems)
		}
		db2.Close()
	}
}
