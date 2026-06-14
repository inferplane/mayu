package main

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/inferplane/inferplane/internal/audit"
)

// reportOpts configures a chargeback aggregation. by ∈ {"team","team,model"};
// since/until are RFC3339 (inclusive/exclusive); empty = unbounded.
type reportOpts struct {
	by    string
	since string
	until string
}

// reportKey is the exact aggregation key — a struct (not a concatenated
// string) so team/model values containing any byte can never collide.
type reportKey struct {
	team  string
	model string // "" when by == "team"
}

type reportRow struct {
	Team   string
	Model  string // "" when by == "team"
	Micros int64
}

// accumulate parses one complete line and folds a settled record into sums.
// Returns 1 if the line was skipped (malformed JSON or unparseable ts when a
// time filter is active), else 0.
func accumulate(line []byte, sums map[reportKey]*reportRow, byModel, haveSince, haveUntil bool, since, until time.Time) int {
	if len(line) == 0 {
		return 0
	}
	var rec audit.Record
	if e := json.Unmarshal(line, &rec); e != nil {
		fmt.Fprintf(os.Stderr, "report: skipping malformed line: %v\n", e)
		return 1
	}
	if rec.Event != "request_completed" || rec.Cost == nil {
		return 0 // only settled records carry billable cost
	}
	if haveSince || haveUntil {
		ts, e := time.Parse(time.RFC3339, rec.TS)
		if e != nil {
			fmt.Fprintf(os.Stderr, "report: skipping record with bad ts %q: %v\n", rec.TS, e)
			return 1
		}
		if haveSince && ts.Before(since) {
			return 0 // --since inclusive
		}
		if haveUntil && !ts.Before(until) {
			return 0 // --until exclusive
		}
	}
	model := ""
	if byModel {
		model = rec.Request.ModelResolved // the BILLED model
		if model == "" {
			model = rec.Request.ModelRequested
		}
	}
	key := reportKey{team: rec.Principal.Team, model: model}
	row := sums[key]
	if row == nil {
		row = &reportRow{Team: rec.Principal.Team, Model: model}
		sums[key] = row
	}
	row.Micros += rec.Cost.AmountUSDMicros
	return 0
}

// runReport aggregates settled cost (integer µUSD) from an audit JSONL stream
// and writes CSV. It returns the CSV bytes, the count of skipped (malformed /
// unparseable-timestamp) lines, and an error only on a fatal I/O/writer
// problem — never on a bad individual record (those are skipped + counted, so
// a single corrupt line can't deny a finance report). A partial trailing line
// (live file mid-write) is trimmed, not parsed.
func runReport(r io.Reader, opts reportOpts) (csvBytes []byte, skipped int, err error) {
	var since, until time.Time
	var haveSince, haveUntil bool
	if opts.since != "" {
		if since, err = time.Parse(time.RFC3339, opts.since); err != nil {
			return nil, 0, fmt.Errorf("report: --since: %w", err)
		}
		haveSince = true
	}
	if opts.until != "" {
		if until, err = time.Parse(time.RFC3339, opts.until); err != nil {
			return nil, 0, fmt.Errorf("report: --until: %w", err)
		}
		haveUntil = true
	}
	byModel := opts.by == "team,model"

	// Stream line by line (no whole-file buffer — audit logs are operator-scale,
	// can be large). ReadString('\n') returns nil err only for a complete,
	// newline-terminated line; a non-terminated remainder at EOF is the live
	// writer's partial tail and is dropped, never parsed. A real read error is
	// propagated (a failed read must NOT masquerade as an empty report).
	sums := map[reportKey]*reportRow{}
	br := bufio.NewReader(r)
	for {
		raw, e := br.ReadString('\n')
		if e == nil {
			skipped += accumulate(bytes.TrimSpace([]byte(raw)), sums, byModel, haveSince, haveUntil, since, until)
			continue
		}
		if e == io.EOF {
			break // drop any partial trailing remainder
		}
		return nil, skipped, fmt.Errorf("report: read: %w", e)
	}

	rows := make([]*reportRow, 0, len(sums))
	for _, r := range sums {
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Team != rows[j].Team {
			return rows[i].Team < rows[j].Team
		}
		return rows[i].Model < rows[j].Model
	})

	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if byModel {
		_ = w.Write([]string{"team", "model", "micro_usd", "usd"})
		for _, r := range rows {
			_ = w.Write([]string{r.Team, r.Model, strconv.FormatInt(r.Micros, 10), formatUSDFromMicros(r.Micros)})
		}
	} else {
		_ = w.Write([]string{"team", "micro_usd", "usd"})
		for _, r := range rows {
			_ = w.Write([]string{r.Team, strconv.FormatInt(r.Micros, 10), formatUSDFromMicros(r.Micros)})
		}
	}
	w.Flush()
	if e := w.Error(); e != nil {
		return nil, skipped, fmt.Errorf("report: csv: %w", e)
	}
	return buf.Bytes(), skipped, nil
}

// formatUSDFromMicros renders integer µUSD as a dollar string WITHOUT float
// arithmetic: whole = micros/1e6, fraction = micros%1e6 zero-padded to 6
// digits. Sign-aware. Exact at int64 extremes (a billing product must never
// show float-rounded money).
func formatUSDFromMicros(micros int64) string {
	sign := ""
	// Take the magnitude as uint64 so math.MinInt64 (which has no positive
	// int64 negation) is exact — a billing figure must never overflow.
	var mag uint64
	if micros < 0 {
		sign = "-"
		mag = uint64(-(micros + 1)) + 1
	} else {
		mag = uint64(micros)
	}
	return fmt.Sprintf("%s$%d.%06d", sign, mag/1_000_000, mag%1_000_000)
}

// reportCmd implements `inferplane report --file <path> [--since] [--until]
// [--by team|team,model]`. Returns the process exit code.
func reportCmd(args []string) int {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	file := fs.String("file", "", "path to the JSONL audit log (required)")
	by := fs.String("by", "team", "grouping: team | team,model")
	since := fs.String("since", "", "include records with ts >= this RFC3339 time")
	until := fs.String("until", "", "include records with ts < this RFC3339 time")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *file == "" {
		fmt.Fprintln(os.Stderr, "report: --file is required")
		return 2
	}
	if *by != "team" && *by != "team,model" {
		fmt.Fprintln(os.Stderr, `report: --by must be "team" or "team,model"`)
		return 2
	}
	f, err := os.Open(*file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer f.Close()
	out, skipped, err := runReport(f, reportOpts{by: *by, since: *since, until: *until})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	os.Stdout.Write(out)
	if skipped > 0 {
		fmt.Fprintf(os.Stderr, "report: %d line(s) skipped\n", skipped)
	}
	return 0
}
