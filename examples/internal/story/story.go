// Copyright Bootstrapping.org 2026
// SPDX-License-Identifier: Apache-2.0

// Package story runs the case-study examples: a SQLite database made from
// the example's schema, seed versions applied one after another, the plans
// printed, and every result checked. An example is a list of steps; the
// first failed check stops it.
package story

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	bs "github.com/bootstrapping-engine/bootstrapping"
	"github.com/bootstrapping-engine/bootstrapping/internal/cli"
	_ "github.com/mattn/go-sqlite3"
)

// Story is one example's database and its seeds.
type Story struct {
	ctx    context.Context
	db     *sql.DB
	files  fs.FS
	out    io.Writer
	checks int
	step   int
}

type failure struct{ err error }

// Run makes a fresh database in dir (a temporary one if dir is empty) from
// files' schema.sql, runs the example's steps, and returns the first
// failure. Output goes to out.
func Run(files fs.FS, dir string, out io.Writer, title string, steps func(s *Story)) (err error) {
	if dir == "" {
		tmp, err := os.MkdirTemp("", "bootstrapping-example-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		dir = tmp
	}
	path := filepath.Join(dir, "app.db")
	os.Remove(path)
	db, err := sql.Open("sqlite3", "file:"+path+"?_foreign_keys=1&_busy_timeout=30000")
	if err != nil {
		return err
	}
	defer db.Close()
	s := &Story{ctx: context.Background(), db: db, files: files, out: out}
	defer func() {
		if r := recover(); r != nil {
			f, ok := r.(failure)
			if !ok {
				panic(r)
			}
			err = f.err
		}
	}()
	schema, err := fs.ReadFile(files, "schema.sql")
	s.must(err)
	s.Exec(string(schema))
	fmt.Fprintln(out, title)
	steps(s)
	fmt.Fprintf(out, "\nAll checks passed (%d).\n", s.checks)
	return nil
}

// Main runs an example as a command and exits with its result.
func Main(files fs.FS, title string, steps func(s *Story)) {
	if err := Run(files, "", os.Stdout, title, steps); err != nil {
		fmt.Fprintln(os.Stderr, "\nFAILED:", err)
		os.Exit(1)
	}
}

func (s *Story) must(err error) {
	if err != nil {
		panic(failure{err})
	}
}

// Step starts a numbered step with a few lines saying what happens.
func (s *Story) Step(lines ...string) {
	s.step++
	fmt.Fprintf(s.out, "\n%d. %s\n", s.step, lines[0])
	for _, l := range lines[1:] {
		fmt.Fprintln(s.out, "   "+l)
	}
}

// Say prints a line inside a step.
func (s *Story) Say(line string) { fmt.Fprintln(s.out, "   "+line) }

// Exec runs SQL, as the application or an admin would.
func (s *Story) Exec(q string, args ...any) {
	if _, err := s.db.ExecContext(s.ctx, q, args...); err != nil {
		s.must(fmt.Errorf("%s: %w", strings.TrimSpace(q), err))
	}
}

func (s *Story) seeds(version string) *bs.Set {
	sub, err := fs.Sub(s.files, "seeds/"+version)
	s.must(err)
	set, err := bs.LoadFS(sub, ".")
	s.must(err)
	return set
}

func opts(take []bs.Resolution) *bs.Options {
	return &bs.Options{Take: take, Now: func() time.Time { return time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC) }}
}

// Plan prints what applying a seed version would do, as bst plan prints it,
// and returns the plan.
func (s *Story) Plan(version string) *bs.Plan {
	p, err := bs.Preview(s.ctx, s.db, s.seeds(version), opts(nil))
	s.must(err)
	s.print(p, cli.RenderPlan)
	return p
}

// Markdown prints the plan for a seed version as bst plan -markdown does.
func (s *Story) Markdown(version string) {
	p, err := bs.Preview(s.ctx, s.db, s.seeds(version), opts(nil))
	s.must(err)
	s.print(p, cli.RenderMarkdown)
}

func (s *Story) print(p *bs.Plan, render func(io.Writer, *bs.Plan, bool)) {
	var b bytes.Buffer
	render(&b, p, false)
	fmt.Fprintln(s.out)
	for _, line := range strings.Split(strings.TrimRight(b.String(), "\n"), "\n") {
		if line == "" {
			fmt.Fprintln(s.out)
		} else {
			fmt.Fprintln(s.out, "   "+line)
		}
	}
	fmt.Fprintln(s.out)
}

// Apply applies a seed version and prints its summary. It fails the story
// if a conflict blocks the apply.
func (s *Story) Apply(version string, take ...bs.Resolution) *bs.Plan {
	p, err := bs.Apply(s.ctx, s.db, s.seeds(version), opts(take))
	s.must(err)
	s.Say(cli.Summary(p))
	return p
}

// Blocked applies a seed version that must be blocked by a conflict, and
// checks nothing was written.
func (s *Story) Blocked(version string) *bs.Plan {
	p, err := bs.Apply(s.ctx, s.db, s.seeds(version), opts(nil))
	s.checks++
	if !errors.Is(err, bs.ErrBlocked) {
		s.must(fmt.Errorf("applying %s should be blocked by a conflict, got %v", version, err))
	}
	s.Say(cli.Summary(p))
	return p
}

// InStep checks that the seed version has nothing left to apply.
func (s *Story) InStep(version string) {
	p, err := bs.Preview(s.ctx, s.db, s.seeds(version), opts(nil))
	s.must(err)
	s.checks++
	if p.Pending() || p.Blocked() {
		s.must(fmt.Errorf("after applying %s the plan should be empty: %s", version, cli.Summary(p)))
	}
}

// Want checks that a query's single value is want ("NULL" for NULL).
func (s *Story) Want(q, want string) {
	var v sql.NullString
	s.must(s.db.QueryRowContext(s.ctx, q).Scan(&v))
	got := "NULL"
	if v.Valid {
		got = v.String
	}
	s.checks++
	if got != want {
		s.must(fmt.Errorf("%s: got %q, want %q", strings.Join(strings.Fields(q), " "), got, want))
	}
}
