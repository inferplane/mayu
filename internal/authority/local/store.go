// Package local implements durable, node-local budget escrow. Admission only
// touches a private SQLite journal; the caller owns background synchronization.
package local

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/policy"
	_ "modernc.org/sqlite"
)

// Store serializes journal transactions and keeps only monotonic deadlines in
// memory. The durable owner is checked inside every SQLite write transaction,
// including reads used to construct sync requests.
type Store struct {
	mu        sync.Mutex
	db        *sql.DB
	instance  string
	wake      chan struct{}
	closed    bool
	broken    error
	now       func() time.Time
	deadlines map[string]time.Time
	windows   map[string]time.Time
}

var _ governance.BudgetAuthority = (*Store)(nil)

// The journal is one versioned snapshot in a SQLite row. Every transition reads
// it under BEGIN IMMEDIATE and replaces it at FULL durability before returning.
// This deliberately favors simple atomic multi-budget accounting over partial
// table updates. Only pending liabilities and bounded retry receipts survive
// collection; acknowledged historical windows do not grow this row forever.
type journal struct {
	Version    int
	Ready      bool
	Poisoned   bool
	ServerTime time.Time
	Budgets    map[string]policy.AuthorityBudget
	Grants     map[string]*grant
	Requests   map[string]*grantRequest
	Permits    map[string]*permit
	Meters     map[string]*meter
	// Each stream rotates independently so active scopes cannot starve an
	// unacknowledged backlog, including after a restart.
	RequestCursor    string
	ReportCursor     string
	MeterCursor      string
	ObsoleteRequests []string
	Completed        []string
	Receipts         map[string]replayReceipt
	ReceiptOrder     []string
}

type grant struct {
	Wire                         policy.AuthorityGrant
	Instance                     string
	Consumed, Observed, Reserved int64
	Pending                      int64
	Closed, Overrun              bool
	Sequence, Ack, Sent          int64
}

type grantRequest struct {
	Wire         policy.AuthorityGrantRequest
	Budget       policy.AuthorityBudget
	Instance     string
	Sent         bool
	GrantID      string
	Denied       string
	Obsolete     bool
	PendingReply bool
}

type permit struct {
	Instance     string
	Bound        int64
	Grants       []string
	Meters       []string
	Done         bool
	Complete     bool
	Actual       *int64
	Retired      bool
	InvalidUsage bool `json:",omitempty"`
}

type meter struct {
	Wire      policy.AuthorityMeter
	Ack       int64
	Sent      int64
	WindowEnd time.Time
}

func emptyJournal() *journal {
	return &journal{Version: 1, Budgets: map[string]policy.AuthorityBudget{},
		Grants: map[string]*grant{}, Requests: map[string]*grantRequest{},
		Permits: map[string]*permit{}, Meters: map[string]*meter{}}
}

// Open creates or opens a private, persistent journal and atomically fences any
// previous owner. Restart never recovers spending authority from an old boot.
func Open(path string) (*Store, error) {
	if path == "" || path == ":memory:" {
		return nil, invalid("a persistent journal path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("local authority path: %w", err)
	}
	if err := privateFile(absolute); err != nil {
		return nil, fmt.Errorf("local authority journal: %w", err)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(absolute + suffix); err == nil {
			if err := privateFile(absolute + suffix); err != nil {
				return nil, fmt.Errorf("local authority sidecar: %w", err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("local authority sidecar: %w", err)
		}
	}
	instance, err := nonce()
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: absolute}
	q := u.Query()
	q.Set("mode", "rw")
	q.Set("_txlock", "immediate")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(FULL)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("local authority open: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, instance: instance, wake: make(chan struct{}, 1),
		now: time.Now, deadlines: map[string]time.Time{}, windows: map[string]time.Time{}}
	if err := s.initialize(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("local authority initialize: %w", err)
	}
	s.signal()
	return s, nil
}

func privateFile(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if errors.Is(err, os.ErrExist) {
		fi, statErr := os.Lstat(path)
		if statErr != nil {
			return statErr
		}
		if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0600 {
			return invalid("journal and sidecars must be regular private 0600 files")
		}
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return err
	}
	// Persist creation of the directory entry as well as SQLite's later writes.
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *Store) initialize() error {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS local_escrow (
		id INTEGER PRIMARY KEY CHECK (id = 1), owner TEXT NOT NULL, state BLOB NOT NULL
	)`); err != nil {
		return err
	}
	var raw []byte
	err = tx.QueryRowContext(ctx, `SELECT state FROM local_escrow WHERE id = 1`).Scan(&raw)
	j := emptyJournal()
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		if err := decode(raw, j); err != nil {
			return err
		}
		if err := retire(j); err != nil {
			return err
		}
	}
	j.Ready = false
	j.ServerTime = time.Time{}
	j.Budgets = map[string]policy.AuthorityBudget{}
	collect(j, s.instance)
	raw, err = json.Marshal(j)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO local_escrow(id, owner, state)
		VALUES(1, ?, ?) ON CONFLICT(id) DO UPDATE SET owner=excluded.owner, state=excluded.state`,
		s.instance, raw); err != nil {
		return err
	}
	return tx.Commit()
}

func retire(j *journal) error {
	for _, p := range j.Permits {
		if p.Done {
			continue
		}
		for _, id := range p.Meters {
			m := j.Meters[id]
			var err error
			m.Wire.Consumed, err = add(m.Wire.Consumed, p.Bound)
			if err != nil {
				return err
			}
			if err := bump(&m.Wire.Sequence); err != nil {
				return err
			}
		}
		p.Done, p.Retired = true, true
	}
	for _, g := range j.Grants {
		// Every old grant is burned in full, including a closed tombstone.
		// A restored stale snapshot must not return credit used by a live copy.
		if g.Consumed < g.Wire.Amount {
			g.Consumed = g.Wire.Amount
		}
		g.Closed, g.Reserved, g.Pending = true, 0, 0
		if err := bump(&g.Sequence); err != nil {
			return err
		}
	}
	for _, r := range j.Requests {
		r.Obsolete = true
	}
	return nil
}

// Close releases this handle. It does not declare old authority refundable.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("local authority close: %w", err)
	}
	return nil
}

// Wake coalesces notifications for the caller's background sync loop.
func (s *Store) Wake() <-chan struct{} { return s.wake }

func (s *Store) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Store) transaction(ctx context.Context, fn func(*journal) error) error {
	if s.closed {
		return fmt.Errorf("%w: journal closed", governance.ErrAuthorityUnavailable)
	}
	if s.broken != nil {
		return s.broken
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return s.storageError(err)
	}
	defer tx.Rollback()
	var owner string
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT owner, state FROM local_escrow WHERE id = 1`).Scan(&owner, &raw); err != nil {
		return s.storageError(err)
	}
	if owner != s.instance {
		return fmt.Errorf("%w: journal owner fenced", governance.ErrAuthorityUnavailable)
	}
	j := emptyJournal()
	if err := decode(raw, j); err != nil {
		s.broken = err
		return err
	}
	if err := fn(j); err != nil {
		return err
	}
	collect(j, s.instance)
	raw, err = json.Marshal(j)
	if err != nil {
		return s.storageError(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE local_escrow SET state = ? WHERE id = 1 AND owner = ?`, raw, s.instance); err != nil {
		return s.storageError(err)
	}
	if err := tx.Commit(); err != nil {
		return s.storageError(err)
	}
	pruneDeadlines(s.deadlines, j)
	return nil
}

func (s *Store) storageError(err error) error {
	wrapped := fmt.Errorf("%w: local journal: %w", governance.ErrAuthorityInvalid, err)
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		s.broken = wrapped
	}
	return wrapped
}

func decode(raw []byte, j *journal) error {
	var decoded journal
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return fmt.Errorf("%w: decode journal: %w", governance.ErrAuthorityInvalid, err)
	}
	*j = decoded
	if j.Version != 1 || j.Grants == nil || j.Requests == nil || j.Permits == nil || j.Meters == nil || j.Budgets == nil {
		return invalid("unsupported or incomplete journal")
	}
	for id, g := range j.Grants {
		if g == nil || g.Wire.ID != id || g.Wire.Amount <= 0 || g.Consumed < 0 || g.Observed < 0 ||
			g.Reserved < 0 || g.Pending < 0 || g.Sequence <= 0 || g.Ack < 0 || g.Ack > g.Sent || g.Sent > g.Sequence {
			return invalid("corrupt grant journal")
		}
	}
	for _, r := range j.Requests {
		if r == nil {
			return invalid("corrupt grant request")
		}
	}
	for _, m := range j.Meters {
		if m == nil || m.Wire.Consumed < 0 || m.Wire.Observed < 0 || m.Wire.Sequence <= 0 ||
			m.Ack < 0 || m.Ack > m.Sent || m.Sent > m.Wire.Sequence {
			return invalid("corrupt meter")
		}
	}
	for _, p := range j.Permits {
		if p == nil || p.Bound < 0 {
			return invalid("corrupt permit")
		}
		for _, id := range p.Grants {
			if j.Grants[id] == nil {
				return invalid("missing permit grant")
			}
		}
		for _, id := range p.Meters {
			if j.Meters[id] == nil {
				return invalid("missing permit meter")
			}
		}
	}
	return nil
}

func nonce() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("local authority nonce: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func invalid(reason string) error {
	return fmt.Errorf("%w: %s", governance.ErrAuthorityInvalid, reason)
}

func add(a, b int64) (int64, error) {
	if a < 0 || b < 0 || a > math.MaxInt64-b {
		return 0, invalid("accounting overflow")
	}
	return a + b, nil
}

func bump(sequence *int64) error {
	n, err := add(*sequence, 1)
	if err == nil {
		*sequence = n
	}
	return err
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sameDefinition(a, b policy.AuthorityBudget) bool {
	return a.Key == b.Key && a.Revision == b.Revision && a.WindowID == b.WindowID
}
