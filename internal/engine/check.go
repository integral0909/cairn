package engine

import (
	"fmt"

	"github.com/integral0909/cairn/internal/btree"
	"github.com/integral0909/cairn/internal/value"
)

// Check verifies the database: every B-tree is well formed, every index
// entry points at a row and every row has its index entries, and every
// page is either in use or on the free list. It returns the problems found.
func (db *DB) Check() ([]string, error) {
	tx := db.p.BeginWrite() // a stable view; nothing is changed
	defer tx.Rollback()
	cat := db.catalog()
	var problems []string
	used := map[uint32]string{0: "header"}
	claim := func(pages []uint32, owner string) {
		for _, pg := range pages {
			if prev, dup := used[pg]; dup {
				problems = append(problems, fmt.Sprintf("page %d is used by both %s and %s", pg, prev, owner))
			}
			used[pg] = owner
		}
	}
	_, pages, err := btree.Check(tx, schemaRoot)
	if err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	claim(pages, "the schema")
	for _, t := range cat.Tables() {
		st, pages, err := btree.Check(tx, t.Root)
		if err != nil {
			problems = append(problems, fmt.Sprintf("table %s: %v", t.Name, err))
			continue
		}
		claim(pages, "table "+t.Name)
		expect := map[*Index]map[string]bool{}
		for _, ix := range t.Indexes {
			expect[ix] = map[string]bool{}
		}
		c := btree.NewCursor(tx, t.Root)
		for c.First(); c.Valid(); c.Next() {
			raw, err := c.Value()
			if err != nil {
				return nil, err
			}
			key := append([]byte(nil), c.Key()...)
			row, err := tableRow(t, key, raw, &execCtx{})
			if err != nil {
				problems = append(problems, fmt.Sprintf("table %s row %d: %v", t.Name, value.RowidFromKey(key), err))
				continue
			}
			for _, ix := range t.Indexes {
				k, _, err := indexKey(ix, row, value.RowidFromKey(key))
				if err != nil {
					problems = append(problems, err.Error())
					continue
				}
				expect[ix][string(k)] = true
			}
		}
		if err := c.Err(); err != nil {
			return nil, err
		}
		for _, ix := range t.Indexes {
			ist, pages, err := btree.Check(tx, ix.Root)
			if err != nil {
				problems = append(problems, fmt.Sprintf("index %s: %v", ix.Name, err))
				continue
			}
			claim(pages, "index "+ix.Name)
			ic := btree.NewCursor(tx, ix.Root)
			for ic.First(); ic.Valid(); ic.Next() {
				if !expect[ix][string(ic.Key())] {
					problems = append(problems, fmt.Sprintf("index %s has an entry for row %d that does not match the table", ix.Name, value.RowidFromKey(ic.Key())))
				}
			}
			if ist.Entries != st.Entries {
				problems = append(problems, fmt.Sprintf("index %s has %d entries but table %s has %d rows", ix.Name, ist.Entries, t.Name, st.Entries))
			}
		}
	}
	free, err := tx.FreePages()
	if err != nil {
		problems = append(problems, err.Error())
	}
	if total := int(tx.PageCount()); len(used)+free != total {
		problems = append(problems, fmt.Sprintf("%d pages: %d in use, %d free, %d unaccounted for", total, len(used), free, total-len(used)-free))
	}
	return problems, nil
}
