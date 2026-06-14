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

type reportRow struct {
	Team   string
	Model  string // "" when by == "team"
	Micros int64
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

	sums := map[string]*reportRow{}
	sc := bufio.NewScanner(completeLines(r))
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var rec audit.Record
		if e := json.Unmarshal(line, &rec); e != nil {
			skipped++
			fmt.Fprintf(os.Stderr, "report: skipping malformed line: %v\n", e)
			continue
		}
		if rec.Event != "request_completed" || rec.Cost == nil {
			continue // only settled records carry billable cost
		}
		if haveSince || haveUntil {
			ts, e := time.Parse(time.RFC3339, rec.TS)
			if e != nil {
				skipped++
				fmt.Fprintf(os.Stderr, "report: skipping record with bad ts %q: %v\n", rec.TS, e)
				continue
			}
			if haveSince && ts.Before(since) {
				continue // --since inclusive
			}
			if haveUntil && !ts.Before(until) {
				continue // --until exclusive
			}
		}
		model := ""
		if byModel {
			model = rec.Request.ModelResolved // the BILLED model
			if model == "" {
				model = rec.Request.ModelRequested
			}
		}
		key := rec.Principal.Team + "\x00" + model
		row := sums[key]
		if row == nil {
			row = &reportRow{Team: rec.Principal.Team, Model: model}
			sums[key] = row
		}
		row.Micros += rec.Cost.AmountUSDMicros
	}
	if e := sc.Err(); e != nil {
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
	if micros < 0 {
		sign = "-"
		micros = -micros
	}
	return fmt.Sprintf("%s$%d.%06d", sign, micros/1_000_000, micros%1_000_000)
}

// completeLines returns a reader that yields only the data up to the last
// newline, dropping any partial trailing line (a live audit file mid-write).
// It buffers the whole input (audit files for a report are operator-scale).
func completeLines(r io.Reader) io.Reader {
	data, err := io.ReadAll(r)
	if err != nil {
		return bytes.NewReader(nil)
	}
	if i := bytes.LastIndexByte(data, '\n'); i >= 0 {
		return bytes.NewReader(data[:i+1])
	}
	return bytes.NewReader(nil) // no complete line yet
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
