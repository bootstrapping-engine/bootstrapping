// Copyright Bootstrapping.org 2026
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"io"
	"strings"

	bs "github.com/bootstrapping-engine/bootstrapping"
)

// MarkdownMarker opens every Markdown plan, so a CI step can find the comment
// it posted last time and replace it instead of adding another.
const MarkdownMarker = "<!-- bootstrapping-plan -->"

// markdownLimit keeps a plan under GitHub's 65,536-character limit for a
// comment, with room for the lines around the table.
const markdownLimit = 60000

// RenderMarkdown prints a plan as GitHub-flavored Markdown, for a CI step to
// post on a pull request: the summary, what needs a decision, and a table of
// changes per seed table.
func RenderMarkdown(w io.Writer, p *bs.Plan, verbose bool) {
	var b strings.Builder
	b.WriteString(MarkdownMarker + "\n")
	if p.Applied {
		b.WriteString("### Seed apply\n\n")
	} else {
		b.WriteString("### Seed plan\n\n")
	}
	b.WriteString("**" + Summary(p) + "**\n")

	var shown, blocking []bs.Change
	for _, c := range p.Changes {
		if c.Action != bs.ActRecord || verbose {
			shown = append(shown, c)
		}
		if c.Blocking() {
			blocking = append(blocking, c)
		}
	}
	if len(blocking) > 0 {
		b.WriteString("\n> [!WARNING]\n")
		if len(blocking) == 1 {
			b.WriteString("> 1 conflict blocks the apply. Nothing is written until it is settled,\n")
		} else {
			fmt.Fprintf(&b, "> %d conflicts block the apply. Nothing is written until they are settled,\n", len(blocking))
		}
		fmt.Fprintf(&b, "> with `on_conflict` in the seed file or `-take` on the command, such as `-take %s`.\n", strings.Trim(exampleTake(p), "'"))
	}
	if len(shown) == 0 {
		fmt.Fprint(w, b.String())
		return
	}

	var body strings.Builder
	table, cut := "", 0
	for i, c := range shown {
		var row strings.Builder
		if c.Table != table {
			table = c.Table
			fmt.Fprintf(&row, "\n#### %s\n\n| change | key | column | detail |\n|---|---|---|---|\n", mdText(table))
		}
		action := label[c.Action]
		if c.Action == bs.ActConflict {
			action = "**conflict**"
			if c.Resolved != "" {
				action = "conflict, settled"
			}
		}
		fmt.Fprintf(&row, "| %s | %s | %s | %s |\n", action, mdCode(c.KeyText()), mdCodeOrEmpty(c.Column), mdDetail(c))
		if b.Len()+body.Len()+row.Len() > markdownLimit {
			cut = len(shown) - i
			break
		}
		body.WriteString(row.String())
	}
	b.WriteString(body.String())
	if cut > 0 {
		fmt.Fprintf(&b, "\n%d more not shown; run `bst plan` for the whole plan.\n", cut)
	}
	fmt.Fprint(w, b.String())
}

// mdDetail is a change's detail with values as code spans, so a value that
// looks like Markdown shows as itself.
func mdDetail(c bs.Change) string {
	v := func(x *bs.Value) string { return mdCode(val(x)) }
	switch c.Action {
	case bs.ActUpdate:
		s := v(c.Live) + " → " + v(c.Seed)
		if c.Reason != "" {
			s += " (" + mdText(c.Reason) + ")"
		}
		return s
	case bs.ActKeepEdit:
		return "local " + v(c.Live) + "; the seed still says " + v(c.Seed)
	case bs.ActConflict:
		var s string
		switch c.Conflict {
		case bs.BothChanged:
			s = "seed " + v(c.Base) + " → " + v(c.Seed) + ", local " + v(c.Live)
		case bs.ExistsAlready:
			s = "seed " + v(c.Seed) + ", already in the table as " + v(c.Live)
		default:
			s = mdText(c.Reason)
		}
		switch c.Resolved {
		case bs.TakeSeed:
			s += " (taking the seed's value)"
		case bs.TakeLocal:
			s += " (keeping the local value)"
		}
		return s
	}
	return mdText(c.Reason)
}

// mdCode is s as an inline code span that holds inside a table cell: the
// fence is longer than any run of backticks in s, and pipes are escaped.
func mdCode(s string) string {
	s = strings.NewReplacer("\r", " ", "\n", " ", "|", `\|`).Replace(s)
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", longest+1)
	if longest > 0 || strings.HasPrefix(s, " ") || strings.HasSuffix(s, " ") {
		return fence + " " + s + " " + fence
	}
	return fence + s + fence
}

func mdCodeOrEmpty(s string) string {
	if s == "" {
		return ""
	}
	return mdCode(s)
}

// mdText is plain text with the characters Markdown would act on escaped.
func mdText(s string) string {
	var b strings.Builder
	for _, r := range strings.NewReplacer("\r", " ", "\n", " ").Replace(s) {
		if strings.ContainsRune("\\`*_[]<>|~", r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
