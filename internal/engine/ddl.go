package engine

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/integral0909/cairn/internal/btree"
	"github.com/integral0909/cairn/internal/sql"
	"github.com/integral0909/cairn/internal/value"
)

func (w *writer) nameTaken(name string) bool {
	return w.cat.Table(name) != nil || w.cat.Index(name) != nil
}

func (w *writer) putSchema(kind, name, table string, root uint32, def string) error {
	return btree.Put(w.tx, schemaRoot, schemaKey(name), schemaRecord(kind, name, table, root, def))
}

func (w *writer) createTable(st *sql.CreateTable) error {
	if w.cat.Table(st.Name) != nil {
		if st.IfNotExists {
			return nil
		}
		return fmt.Errorf("table %s already exists", st.Name)
	}
	if w.cat.Index(st.Name) != nil {
		return fmt.Errorf("there is already an index called %s", st.Name)
	}
	if strings.HasPrefix(strings.ToLower(st.Name), "cairn_") {
		return fmt.Errorf("table names starting with cairn_ are reserved")
	}
	t, uniques, err := tableFromAST(st)
	if err != nil {
		return err
	}
	if t.Root, err = btree.Create(w.tx); err != nil {
		return err
	}
	cat := w.cat.clone()
	for n, cols := range uniques {
		ix := &Index{Name: fmt.Sprintf("auto_%s_%d", strings.ToLower(t.Name), n+1), Table: t.Name, Columns: cols, Unique: true, Auto: true}
		if cat.Index(ix.Name) != nil {
			return fmt.Errorf("index name %s is taken", ix.Name)
		}
		if ix.Root, err = btree.Create(w.tx); err != nil {
			return err
		}
		t.Indexes = append(t.Indexes, ix)
		cat.indexes[strings.ToLower(ix.Name)] = ix
		if err := w.putSchema("autoindex", ix.Name, t.Name, ix.Root, IndexSQL(t, ix)); err != nil {
			return err
		}
	}
	if err := w.putSchema("table", t.Name, t.Name, t.Root, TableSQL(t)); err != nil {
		return err
	}
	cat.tables[strings.ToLower(t.Name)] = t
	w.cat = cat
	return nil
}

func (w *writer) dropTable(st *sql.DropTable) error {
	t := w.cat.Table(st.Name)
	if t == nil {
		if st.IfExists {
			return nil
		}
		return fmt.Errorf("no such table: %s", st.Name)
	}
	cat := w.cat.clone()
	for _, ix := range t.Indexes {
		if err := btree.Drop(w.tx, ix.Root); err != nil {
			return err
		}
		if _, err := btree.Delete(w.tx, schemaRoot, schemaKey(ix.Name)); err != nil {
			return err
		}
		delete(cat.indexes, strings.ToLower(ix.Name))
	}
	if err := btree.Drop(w.tx, t.Root); err != nil {
		return err
	}
	if _, err := btree.Delete(w.tx, schemaRoot, schemaKey(t.Name)); err != nil {
		return err
	}
	delete(cat.tables, strings.ToLower(t.Name))
	w.cat = cat
	return nil
}

func (w *writer) createIndex(st *sql.CreateIndex) error {
	if w.cat.Index(st.Name) != nil {
		if st.IfNotExists {
			return nil
		}
		return fmt.Errorf("index %s already exists", st.Name)
	}
	if w.cat.Table(st.Name) != nil {
		return fmt.Errorf("there is already a table called %s", st.Name)
	}
	t, err := w.table(st.Table)
	if err != nil {
		return err
	}
	ix := &Index{Name: st.Name, Table: t.Name, Unique: st.Unique}
	seen := map[int]bool{}
	for _, name := range st.Columns {
		c := t.column(name)
		if c < 0 {
			return fmt.Errorf("table %s has no column %s", t.Name, name)
		}
		if seen[c] {
			return fmt.Errorf("column %s appears twice in the index", name)
		}
		seen[c] = true
		ix.Columns = append(ix.Columns, c)
	}
	if ix.Root, err = btree.Create(w.tx); err != nil {
		return err
	}
	// Fill it from the existing rows. For a unique index, sort the entries
	// so equal values sit next to each other; entries with a NULL never
	// clash.
	type entry struct {
		key     []byte
		hasNull bool
	}
	var entries []entry
	c := btree.NewCursor(w.tx, t.Root)
	for c.First(); c.Valid(); c.Next() {
		raw, err := c.Value()
		if err != nil {
			return err
		}
		key := append([]byte(nil), c.Key()...)
		row, err := tableRow(t, key, raw, w.ctx)
		if err != nil {
			return err
		}
		k, hasNull, err := indexKey(ix, row, value.RowidFromKey(key))
		if err != nil {
			return err
		}
		entries = append(entries, entry{k, hasNull})
	}
	if err := c.Err(); err != nil {
		return err
	}
	if ix.Unique {
		sort.Slice(entries, func(i, j int) bool { return bytes.Compare(entries[i].key, entries[j].key) < 0 })
		for i := 1; i < len(entries); i++ {
			a, b := entries[i-1], entries[i]
			if !a.hasNull && !b.hasNull && bytes.Equal(a.key[:len(a.key)-8], b.key[:len(b.key)-8]) {
				return fmt.Errorf("cannot create unique index %s: duplicate values in %s", ix.Name, constraintName(t, ix.Columns))
			}
		}
	}
	for _, e := range entries {
		if err := btree.Put(w.tx, ix.Root, e.key, nil); err != nil {
			return err
		}
	}
	nt := t.clone()
	nt.Indexes = append(nt.Indexes, ix)
	cat := w.cat.clone()
	cat.tables[strings.ToLower(t.Name)] = nt
	cat.indexes[strings.ToLower(ix.Name)] = ix
	if err := w.putSchema("index", ix.Name, t.Name, ix.Root, IndexSQL(t, ix)); err != nil {
		return err
	}
	w.cat = cat
	return nil
}

func (w *writer) dropIndex(st *sql.DropIndex) error {
	ix := w.cat.Index(st.Name)
	if ix == nil {
		if st.IfExists {
			return nil
		}
		return fmt.Errorf("no such index: %s", st.Name)
	}
	if ix.Auto {
		return fmt.Errorf("index %s enforces a PRIMARY KEY or UNIQUE constraint and cannot be dropped", ix.Name)
	}
	t := w.cat.Table(ix.Table)
	if err := btree.Drop(w.tx, ix.Root); err != nil {
		return err
	}
	if _, err := btree.Delete(w.tx, schemaRoot, schemaKey(ix.Name)); err != nil {
		return err
	}
	nt := t.clone()
	nt.Indexes = nt.Indexes[:0]
	for _, other := range t.Indexes {
		if other != ix {
			nt.Indexes = append(nt.Indexes, other)
		}
	}
	cat := w.cat.clone()
	cat.tables[strings.ToLower(t.Name)] = nt
	delete(cat.indexes, strings.ToLower(ix.Name))
	w.cat = cat
	return nil
}

func (w *writer) alterTable(st *sql.AlterTable) error {
	t, err := w.table(st.Table)
	if err != nil {
		return err
	}
	cat := w.cat.clone()
	nt := t.clone()
	switch {
	case st.AddColumn != nil:
		cd := st.AddColumn
		if nt.column(cd.Name) >= 0 {
			return fmt.Errorf("table %s already has a column %s", t.Name, cd.Name)
		}
		if cd.PrimaryKey || cd.Unique {
			return fmt.Errorf("ADD COLUMN cannot add PRIMARY KEY or UNIQUE columns")
		}
		col := Column{Name: cd.Name, Type: cd.Type, NotNull: cd.NotNull, Default: cd.Default}
		if col.Default != nil {
			if err := checkConstant(col.Default); err != nil {
				return fmt.Errorf("default for %s: %w", cd.Name, err)
			}
			v, err := constEval(col.Default, w.ctx)
			if err != nil {
				return err
			}
			if _, err := coerce(v, col.Type, t.Name+"."+col.Name); err != nil {
				return err
			}
			if v.IsNull() && col.NotNull {
				return fmt.Errorf("a NOT NULL column needs a non-NULL default")
			}
		} else if col.NotNull {
			return fmt.Errorf("a NOT NULL column needs a default when added to an existing table")
		}
		// Existing rows are not rewritten: rows shorter than the table get
		// the default when read.
		nt.Columns = append(nt.Columns, col)
	case st.RenameTo != "":
		if w.nameTaken(st.RenameTo) && !strings.EqualFold(st.RenameTo, t.Name) {
			return fmt.Errorf("the name %s is already taken", st.RenameTo)
		}
		if _, err := btree.Delete(w.tx, schemaRoot, schemaKey(t.Name)); err != nil {
			return err
		}
		delete(cat.tables, strings.ToLower(t.Name))
		nt.Name = st.RenameTo
		nt.Indexes = nil
		for _, ix := range t.Indexes {
			nix := *ix
			nix.Table = nt.Name
			nt.Indexes = append(nt.Indexes, &nix)
			cat.indexes[strings.ToLower(nix.Name)] = &nix
		}
		for _, ix := range nt.Indexes {
			kind := "index"
			if ix.Auto {
				kind = "autoindex"
			}
			if err := w.putSchema(kind, ix.Name, nt.Name, ix.Root, IndexSQL(nt, ix)); err != nil {
				return err
			}
		}
	}
	if err := w.putSchema("table", nt.Name, nt.Name, nt.Root, TableSQL(nt)); err != nil {
		return err
	}
	cat.tables[strings.ToLower(nt.Name)] = nt
	w.cat = cat
	return nil
}
