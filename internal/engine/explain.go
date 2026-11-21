package engine

import (
	"fmt"
	"strings"
)

// explainPlan renders a plan as a tree, one line per node.
func explainPlan(p plan) []string {
	var out []string
	var walk func(p plan, prefix, childPrefix string)
	walk = func(p plan, prefix, childPrefix string) {
		label, children := describe(p)
		out = append(out, prefix+label)
		for i, ch := range children {
			if i == len(children)-1 {
				walk(ch, childPrefix+"└─ ", childPrefix+"   ")
			} else {
				walk(ch, childPrefix+"├─ ", childPrefix+"│  ")
			}
		}
	}
	walk(p, "", "")
	return out
}

// describe names a plan node and lists its inputs.
func describe(p plan) (string, []plan) {
	switch x := p.(type) {
	case *scanPlan:
		name := x.table.Name
		if x.alias != "" && !strings.EqualFold(x.alias, x.table.Name) {
			name += " AS " + x.alias
		}
		var label string
		switch {
		case x.eq != nil:
			label = fmt.Sprintf("Rowid lookup %s (%s = %s)", name, x.table.rowidName(), x.eqText)
		case x.lo != nil || x.hi != nil:
			label = fmt.Sprintf("Rowid range %s (%s)", name, rangeText(x.table.rowidName(), x.lo, x.hi))
		default:
			label = "Scan " + name
		}
		if x.filterText != "" && x.eq == nil {
			label += " filter " + x.filterText
		}
		return label, nil
	case *indexPlan:
		name := x.table.Name
		if x.alias != "" && !strings.EqualFold(x.alias, x.table.Name) {
			name += " AS " + x.alias
		}
		parts := append([]string(nil), x.eqText...)
		if x.lo != nil {
			parts = append(parts, x.lo.text)
		}
		if x.hi != nil {
			parts = append(parts, x.hi.text)
		}
		kind := "Index lookup"
		if x.lo != nil || x.hi != nil || len(x.eq) < len(x.index.Columns) {
			kind = "Index range"
		}
		label := fmt.Sprintf("%s %s using %s (%s)", kind, name, x.index.Name, strings.Join(parts, " AND "))
		if x.filterText != "" {
			label += " filter " + x.filterText
		}
		return label, nil
	case *filterPlan:
		return "Filter " + x.text, []plan{x.input}
	case *projectPlan:
		return "Project " + strings.Join(x.texts, ", "), []plan{x.input}
	case *nestedLoopPlan:
		kind := "Nested loop"
		if x.lookup {
			kind = "Index nested loop"
		}
		if x.leftJoin {
			kind += " left join"
		} else {
			kind += " join"
		}
		return kind, []plan{x.left, x.right}
	case *hashJoinPlan:
		kind := "Hash join"
		if x.leftJoin {
			kind = "Hash left join"
		}
		label := kind + " on " + strings.Join(x.keyText, " AND ")
		if x.residText != "" {
			label += " filter " + x.residText
		}
		return label, []plan{x.left, x.right}
	case *sortPlan:
		return "Sort by " + strings.Join(x.texts, ", "), []plan{x.input}
	case *limitPlan:
		label := "Limit"
		if x.limitText != "" {
			label += " " + x.limitText
		}
		if x.offsetText != "" {
			label += " offset " + x.offsetText
		}
		return label, []plan{x.input}
	case *distinctPlan:
		return "Distinct", []plan{x.input}
	case *compoundPlan:
		return strings.Title(strings.ToLower(x.op)), []plan{x.left, x.right}
	case *derivedPlan:
		return "Subquery AS " + x.alias, []plan{x.input}
	case *aggPlan:
		label := "Aggregate"
		if len(x.as.groupTexts) > 0 {
			label += " group by " + strings.Join(x.as.groupTexts, ", ")
		}
		var aggs []string
		for _, a := range x.as.aggs {
			aggs = append(aggs, a.text)
		}
		if len(aggs) > 0 {
			label += ": " + strings.Join(aggs, ", ")
		}
		return label, []plan{x.input}
	case *trimPlan:
		return describe(x.input)
	case *liftPlan:
		return describe(x.input)
	case oneRowPlan:
		return "Single row", nil
	}
	return fmt.Sprintf("%T", p), nil
}

func (t *Table) rowidName() string {
	if t.RowidCol >= 0 {
		return t.Columns[t.RowidCol].Name
	}
	return "rowid"
}

func rangeText(col string, lo, hi *bound) string {
	var parts []string
	if lo != nil {
		op := ">"
		if lo.inclusive {
			op = ">="
		}
		parts = append(parts, col+" "+op+" "+lo.text)
	}
	if hi != nil {
		op := "<"
		if hi.inclusive {
			op = "<="
		}
		parts = append(parts, col+" "+op+" "+hi.text)
	}
	return strings.Join(parts, " AND ")
}
