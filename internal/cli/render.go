// Copyright Bootstrapping.org 2026
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"io"
	"strings"

	bs "github.com/bootstrapping-engine/bootstrapping"
)

var label = map[bs.Action]string{
	bs.ActInsert:     "insert",
	bs.ActUpdate:     "update",
	bs.ActDelete:     "delete",
	bs.ActRetire:     "retire",
	bs.ActUnretire:   "unretire",
	bs.ActRename:     "rename",
	bs.ActKeepEdit:   "keep edit",
	bs.ActKeepDelete: "keep delete",
	bs.ActConflict:   "CONFLICT",
	bs.ActRelease:    "release",
	bs.ActSkip:       "skip",
	bs.ActRecord:     "record",
}

func val(v *bs.Value) string {
	if v == nil {
		return "(none)"
	}
	return bs.Display(*v)
}

// Detail is the explanation printed after a change's key and column.
func Detail(c bs.Change) string {
	switch c.Action {
	case bs.ActUpdate:
		s := val(c.Live) + " -> " + val(c.Seed)
		if c.Reason != "" {
			s += "  (" + c.Reason + ")"
		}
		return s
	case bs.ActKeepEdit:
		return "local " + val(c.Live) + "; the seed still says " + val(c.Seed)
	case bs.ActConflict:
		var s string
		switch c.Conflict {
		case bs.BothChanged:
			s = "seed " + val(c.Base) + " -> " + val(c.Seed) + ", local " + val(c.Live)
		case bs.ExistsAlready:
			s = "seed " + val(c.Seed) + ", already in the table as " + val(c.Live)
		default:
			s = c.Reason
		}
		switch c.Resolved {
		case bs.TakeSeed:
			s += "  (settled: the seed's value)"
		case bs.TakeLocal:
			s += "  (settled: the local value)"
		}
		return s
	}
	return c.Reason
}

// RenderPlan prints a plan as aligned lines grouped by table, then a summary.
func RenderPlan(w io.Writer, p *bs.Plan, verbose bool) {
	var shown []bs.Change
	for _, c := range p.Changes {
		if c.Action != bs.ActRecord || verbose {
			shown = append(shown, c)
		}
	}
	aw, kw, cw := 0, 0, 0
	for _, c := range shown {
		aw = max(aw, len(label[c.Action]))
		kw = max(kw, len(c.KeyText()))
		cw = max(cw, len(c.Column))
	}
	table := ""
	for _, c := range shown {
		if c.Table != table {
			table = c.Table
			fmt.Fprintln(w, table)
		}
		line := fmt.Sprintf("  %-*s  %-*s", aw, label[c.Action], kw, c.KeyText())
		if cw > 0 {
			line += fmt.Sprintf("  %-*s", cw, c.Column)
		}
		if d := Detail(c); d != "" {
			line += "  " + d
		}
		fmt.Fprintln(w, strings.TrimRight(line, " "))
	}
	if len(shown) > 0 {
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w, Summary(p))
}

// Summary is one line: what a plan would do, or what an apply did.
func Summary(p *bs.Plan) string {
	c := p.Counts()
	var parts []string
	add := func(n int, one, many string) {
		if n == 1 {
			parts = append(parts, "1 "+one)
		} else if n > 1 {
			parts = append(parts, fmt.Sprintf("%d %s", n, many))
		}
	}
	add(c.Inserts, "insert", "inserts")
	add(c.Updates, "update", "updates")
	add(c.Renames, "rename", "renames")
	add(c.Deletes, "delete", "deletes")
	add(c.Retires, "retire", "retires")
	add(c.Unretires, "unretire", "unretires")
	add(c.Resolved, "settled conflict", "settled conflicts")
	add(c.Conflicts-c.Resolved, "conflict", "conflicts")
	add(c.KeptEdits, "local edit kept", "local edits kept")
	add(c.KeptDeletes, "local delete kept", "local deletes kept")
	add(c.Released, "row released", "rows released")
	add(c.Skipped, "skipped", "skipped")
	add(c.Recorded, "row to record", "rows to record")
	what := strings.Join(parts, ", ")
	switch {
	case p.Applied:
		return fmt.Sprintf("Applied as run %d: %s.", p.Run, strings.Replace(what, "to record", "recorded", 1))
	case p.Blocked():
		return "Blocked: " + what + "."
	case !p.Pending():
		if what == "" {
			return "In step with the seed."
		}
		return "In step with the seed (" + what + ")."
	}
	return "Pending: " + what + "."
}

type changeJSON struct {
	Table    string `json:"table"`
	Key      []any  `json:"key"`
	KeyText  string `json:"key_text"`
	Action   string `json:"action"`
	Column   string `json:"column,omitempty"`
	Base     any    `json:"base,omitempty"`
	Seed     any    `json:"seed,omitempty"`
	Live     any    `json:"live,omitempty"`
	Conflict string `json:"conflict,omitempty"`
	Resolved string `json:"resolved,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

type planJSON struct {
	Pending  bool         `json:"pending"`
	Blocked  bool         `json:"blocked"`
	Applied  bool         `json:"applied"`
	Run      int64        `json:"run,omitempty"`
	SeedHash string       `json:"seed_hash"`
	Counts   bs.Counts    `json:"counts"`
	Summary  string       `json:"summary"`
	Changes  []changeJSON `json:"changes"`
}

func jv(v *bs.Value) any {
	if v == nil {
		return nil
	}
	return map[string]any{"value": v.JSON(), "type": kindName(v.Kind)}
}

func kindName(k bs.Kind) string {
	return [...]string{"null", "integer", "real", "text", "blob"}[k]
}

// PlanJSON is the plan as the -json flag prints it.
func PlanJSON(p *bs.Plan) any {
	out := planJSON{Pending: p.Pending() && !p.Applied, Blocked: p.Blocked(), Applied: p.Applied, Run: p.Run,
		SeedHash: p.SeedHash, Counts: p.Counts(), Summary: Summary(p), Changes: []changeJSON{}}
	for _, c := range p.Changes {
		key := make([]any, len(c.Key))
		for i, k := range c.Key {
			key[i] = k.JSON()
		}
		out.Changes = append(out.Changes, changeJSON{Table: c.Table, Key: key, KeyText: c.KeyText(), Action: string(c.Action),
			Column: c.Column, Base: jv(c.Base), Seed: jv(c.Seed), Live: jv(c.Live), Conflict: c.Conflict,
			Resolved: string(c.Resolved), Reason: c.Reason})
	}
	return out
}

// RenderReport prints Explain's report.
func RenderReport(w io.Writer, r *bs.RowReport) {
	fmt.Fprintf(w, "%s %s\n", r.Table, r.Key)
	state := []string{}
	yes := func(b bool, y, n string) string {
		if b {
			return y
		}
		return n
	}
	state = append(state, yes(r.InSeed, "in the seed", "not in the seed"), yes(r.InTable, "in the table", "not in the table"),
		yes(r.Managed, "in the ledger", "not in the ledger"))
	if r.Retired {
		state = append(state, "retired")
	}
	fmt.Fprintf(w, "  %s\n", strings.Join(state, ", "))
	if len(r.Fields) > 0 {
		fmt.Fprintln(w)
		rows := [][]string{{"column", "own", "last applied", "seed", "table", "verdict"}}
		for _, f := range r.Fields {
			rows = append(rows, []string{f.Column, string(f.Own), val(f.Base), val(f.Seed), val(f.Live), f.Verdict})
		}
		widths := make([]int, len(rows[0]))
		for _, row := range rows {
			for i, cell := range row {
				widths[i] = max(widths[i], len(cell))
			}
		}
		for _, row := range rows {
			var b strings.Builder
			for i, cell := range row {
				fmt.Fprintf(&b, "  %-*s", widths[i], cell)
			}
			fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
		}
	}
	if len(r.Changes) > 0 {
		fmt.Fprintln(w, "\nAn apply would:")
		for _, c := range r.Changes {
			line := "  " + label[c.Action]
			if c.Column != "" {
				line += " " + c.Column
			}
			if d := Detail(c); d != "" {
				line += ": " + d
			}
			fmt.Fprintln(w, line)
		}
	}
	if len(r.History) > 0 {
		fmt.Fprintln(w, "\nHistory:")
		for _, h := range r.History {
			line := fmt.Sprintf("  run %d  %s  %s", h.Run, h.At, h.Action)
			if h.Column != "" {
				line += " " + h.Column
			}
			switch {
			case h.Action == string(bs.ActConflict):
				line += ": kept " + h.Old + " over the seed's " + h.New
				h.Note = ""
			case h.Old != "" || h.New != "":
				line += ": " + h.Old + " -> " + h.New
			}
			if h.Note != "" {
				line += "  (" + h.Note + ")"
			}
			fmt.Fprintln(w, line)
		}
	}
	if !r.InSeed && !r.InTable && !r.Managed {
		fmt.Fprintln(w, "  No seed row, table row or ledger entry has this key.")
	}
}

// ReportJSON is Explain's report as -json prints it.
func ReportJSON(r *bs.RowReport) any {
	type fieldJSON struct {
		Column  string `json:"column"`
		Own     string `json:"own"`
		Base    any    `json:"base"`
		Seed    any    `json:"seed"`
		Live    any    `json:"live"`
		Verdict string `json:"verdict"`
	}
	fields := []fieldJSON{}
	for _, f := range r.Fields {
		fields = append(fields, fieldJSON{f.Column, string(f.Own), jv(f.Base), jv(f.Seed), jv(f.Live), f.Verdict})
	}
	changes := []any{}
	for _, c := range r.Changes {
		changes = append(changes, map[string]any{"action": c.Action, "column": c.Column, "detail": Detail(c)})
	}
	hist := r.History
	if hist == nil {
		hist = []bs.LoggedChange{}
	}
	return map[string]any{"table": r.Table, "key": r.Key, "in_seed": r.InSeed, "in_table": r.InTable,
		"managed": r.Managed, "retired": r.Retired, "fields": fields, "changes": changes, "history": hist}
}
