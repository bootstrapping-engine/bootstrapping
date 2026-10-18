// Copyright Bootstrapping.org 2026
// SPDX-License-Identifier: Apache-2.0

// Package cli is the bootstrapping command (and its short name, bst).
package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	bs "github.com/bootstrapping-engine/bootstrapping"
	_ "github.com/mattn/go-sqlite3" // the SQLite driver the command ships with
)

// Exit codes.
const (
	ExitOK      = 0 // done, or in step already
	ExitError   = 1 // something went wrong, including bad usage
	ExitPending = 2 // plan and status: an apply would write something
	ExitBlocked = 3 // a conflict nobody settled stops the apply
)

const usage = `Usage: %[1]s <command> [flags]

Keeps the reference data an application ships with in step with its SQLite
database, without undoing what admins changed.

Commands:
  plan      show what an apply would do (exit 0 in step, 2 pending, 3 blocked)
  apply     do it, in one transaction (exit 0 done, 3 blocked)
  status    one line: in step, pending or blocked (same exit codes as plan)
  explain   one row, value by value: last applied, seed, table, verdict
  history   the applies the ledger has recorded
  init      create the ledger tables (apply does this on its own)
  version   print the version

Flags, for every command:
  -db FILE      the SQLite database (or $BOOTSTRAPPING_DB)
  -seeds DIR    the seed files (or $BOOTSTRAPPING_SEEDS; default "seeds")
  -json         print JSON instead of text

apply:   -take seed|local, or -take TABLE[/KEY[/COLUMN]]=seed|local, repeatable
plan:    -v also lists rows that already match and only need recording
explain: %[1]s explain TABLE KEY, with KEY as the plan prints it ("editor, posts.publish")
history: -n N for the last N applies
`

// Main runs the command and returns its exit code. name is how it was
// called, bootstrapping or bst.
func Main(name string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintf(stderr, usage, name)
		return ExitError
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "-h", "-help", "--help", "help":
		fmt.Fprintf(stdout, usage, name)
		return ExitOK
	case "version", "-version", "--version":
		fmt.Fprintf(stdout, "%s %s\n", name, bs.Version)
		return ExitOK
	}
	c := &command{name: name, cmd: cmd, stdout: stdout, stderr: stderr}
	fs := flag.NewFlagSet(name+" "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&c.dbPath, "db", os.Getenv("BOOTSTRAPPING_DB"), "the SQLite database")
	seeds := os.Getenv("BOOTSTRAPPING_SEEDS")
	if seeds == "" {
		seeds = "seeds"
	}
	fs.StringVar(&c.seedDir, "seeds", seeds, "the seed directory")
	fs.BoolVar(&c.json, "json", false, "print JSON")
	fs.BoolVar(&c.verbose, "v", false, "list rows that only need recording")
	fs.IntVar(&c.n, "n", 0, "how many applies to list")
	fs.Func("take", "settle conflicts: seed, local, or TABLE[/KEY[/COLUMN]]=seed|local", func(s string) error {
		r, err := parseTake(s)
		if err == nil {
			c.take = append(c.take, r)
		}
		return err
	})
	fs.Usage = func() { fmt.Fprintf(stderr, usage, name) }
	if err := fs.Parse(rest); err != nil {
		return ExitError
	}
	c.args = fs.Args()
	switch cmd {
	case "plan", "status", "apply", "explain", "history", "init":
	default:
		fmt.Fprintf(stderr, "%s: unknown command %q\n\n", name, cmd)
		fmt.Fprintf(stderr, usage, name)
		return ExitError
	}
	code, err := c.run(context.Background())
	if err != nil {
		if c.json {
			json.NewEncoder(stdout).Encode(map[string]string{"error": err.Error()})
		} else {
			fmt.Fprintf(stderr, "%s: %v\n", name, err)
		}
	}
	return code
}

type command struct {
	name, cmd       string
	dbPath, seedDir string
	json, verbose   bool
	n               int
	take            []bs.Resolution
	args            []string
	stdout, stderr  io.Writer
}

func parseTake(s string) (bs.Resolution, error) {
	lhs, take, found := strings.Cut(s, "=")
	if !found {
		take, lhs = s, ""
	}
	r := bs.Resolution{Take: bs.Take(take)}
	if r.Take != bs.TakeSeed && r.Take != bs.TakeLocal {
		return r, fmt.Errorf("-take %q: expected seed or local, or TABLE[/KEY[/COLUMN]]=seed|local", s)
	}
	if lhs != "" {
		parts := strings.Split(lhs, "/")
		r.Table = parts[0]
		switch len(parts) {
		case 1:
		case 2:
			r.Key = parts[1]
		default:
			r.Key, r.Column = strings.Join(parts[1:len(parts)-1], "/"), parts[len(parts)-1]
		}
	}
	return r, nil
}

func (c *command) open() (*sql.DB, error) {
	if c.dbPath == "" {
		return nil, errors.New("no database: give -db FILE or set BOOTSTRAPPING_DB")
	}
	if _, err := os.Stat(c.dbPath); err != nil {
		return nil, fmt.Errorf("no database at %s", c.dbPath)
	}
	return sql.Open("sqlite3", "file:"+c.dbPath+"?_busy_timeout=30000")
}

func (c *command) run(ctx context.Context) (int, error) {
	db, err := c.open()
	if err != nil {
		return ExitError, err
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return ExitError, err
	}
	switch c.cmd {
	case "init":
		if err := bs.Init(ctx, db); err != nil {
			return ExitError, err
		}
		c.say(map[string]any{"ok": true}, "The ledger tables are ready.")
		return ExitOK, nil
	case "history":
		runs, err := bs.History(ctx, db, c.n)
		if err != nil {
			return ExitError, err
		}
		if c.json {
			if runs == nil {
				runs = []bs.Run{}
			}
			return ExitOK, c.emit(runs)
		}
		if len(runs) == 0 {
			fmt.Fprintln(c.stdout, "No applies recorded yet.")
		}
		for _, r := range runs {
			fmt.Fprintf(c.stdout, "run %-4d %s  %s  %d inserted, %d updated, %d deleted, %d retired, %d kept, %d settled  seeds %s\n",
				r.ID, r.At, r.Version, r.Inserts, r.Updates, r.Deletes, r.Retires, r.Kept, r.Resolved, short(r.SeedHash))
		}
		return ExitOK, nil
	}

	set, err := bs.LoadDir(c.seedDir)
	if err != nil {
		return ExitError, err
	}
	opts := &bs.Options{Take: c.take}
	switch c.cmd {
	case "plan", "status":
		p, err := bs.Preview(ctx, db, set, opts)
		if err != nil {
			return ExitError, err
		}
		code := planCode(p)
		if c.json {
			return code, c.emit(PlanJSON(p))
		}
		if c.cmd == "status" {
			fmt.Fprintln(c.stdout, Summary(p))
		} else {
			RenderPlan(c.stdout, p, c.verbose)
		}
		return code, nil
	case "apply":
		p, err := bs.Apply(ctx, db, set, opts)
		if errors.Is(err, bs.ErrBlocked) {
			if c.json {
				return ExitBlocked, c.emit(PlanJSON(p))
			}
			RenderPlan(c.stdout, p, false)
			fmt.Fprintf(c.stdout, "Nothing was written. Settle the conflicts with -take, such as -take local or -take %s, or with on_conflict in the seed file.\n", exampleTake(p))
			return ExitBlocked, nil
		}
		if err != nil {
			return ExitError, err
		}
		if c.json {
			return ExitOK, c.emit(PlanJSON(p))
		}
		RenderPlan(c.stdout, p, false)
		return ExitOK, nil
	case "explain":
		if len(c.args) != 2 {
			return ExitError, fmt.Errorf("usage: %s explain TABLE KEY", c.name)
		}
		rep, err := bs.Explain(ctx, db, set, c.args[0], c.args[1], opts)
		if err != nil {
			return ExitError, err
		}
		if c.json {
			return ExitOK, c.emit(ReportJSON(rep))
		}
		RenderReport(c.stdout, rep)
		return ExitOK, nil
	}
	return ExitError, fmt.Errorf("unknown command %q", c.cmd)
}

func planCode(p *bs.Plan) int {
	switch {
	case p.Blocked():
		return ExitBlocked
	case p.Pending():
		return ExitPending
	}
	return ExitOK
}

func exampleTake(p *bs.Plan) string {
	for _, ch := range p.Changes {
		if ch.Blocking() {
			s := ch.Table + "/" + ch.KeyText()
			if ch.Column != "" {
				s += "/" + ch.Column
			}
			s += "=local"
			if strings.ContainsAny(s, " ,") {
				s = "'" + s + "'"
			}
			return s
		}
	}
	return "local"
}

func (c *command) say(v any, text string) {
	if c.json {
		c.emit(v)
		return
	}
	fmt.Fprintln(c.stdout, text)
}

func (c *command) emit(v any) error {
	enc := json.NewEncoder(c.stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
