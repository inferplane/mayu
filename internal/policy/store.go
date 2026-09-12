package policy

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"sync/atomic"
	"time"

	v1alpha1 "github.com/inferplane/inferplane/api/v1alpha1"
)

// LocalWatchInterval is the mtime-poll cadence for locally loaded policy
// files: save → applied within ~2s, the workstation "edit and it's live"
// loop. Distinct from DefaultPolicySyncInterval, which paces the
// control-plane reconcile poll.
const LocalWatchInterval = 2 * time.Second

// TeamLimits is the merged, enforceable budget/rate view — covering both the
// calendar-month and calendar-day budget windows — of every team-subject
// policy matching one team, in the units the governance pipeline consumes
// (µUSD). Zero means "unlimited" on that dimension, mirroring
// governance.TeamPolicy.
type TeamLimits struct {
	RPM                  int64
	TPM                  int64
	BudgetMicrosPerMonth int64
	// BudgetHard reports whether the binding budget rule is a hard cap:
	// the caller maps it to on_exceeded=block (else warn).
	BudgetHard bool
	// AdminContact carries the binding budget rule's contact hint through
	// verbatim (see api/v1alpha1.BudgetRule.AdminContact); empty if unset.
	AdminContact string
	// BudgetMicrosPerDay/BudgetDayHard/AdminContactDay are the CALENDAR-DAY
	// counterparts, folded from period: CalendarDay rules. They are their own
	// fields rather than a reuse of the month ones because the two windows are
	// two independent RULES: each carries its own limit, its own hardCap and
	// its own adminContact, and most-restrictive-wins is computed per window.
	BudgetMicrosPerDay int64
	BudgetDayHard      bool
	AdminContactDay    string
	// RoutingBudgetMicrosPerMonth/Day are finite budget limits referenced by
	// explicit strict routing tiers, in microUSD. They are accounting thresholds,
	// NOT admission caps and do not contribute to either BudgetHard flag.
	// Zero means no routing-only threshold. Multiple thresholds fold to the
	// smallest nonzero value per window; tier activation still uses each rule's
	// own budget and percentage. Callers may install warn-only accounting from
	// these fields only when no actual config/DB/policy admission limit exists.
	RoutingBudgetMicrosPerMonth int64
	RoutingBudgetMicrosPerDay   int64
}

// UserLimits is the merged, enforceable BUDGET view — both calendar windows —
// of every user-subject policy matching one (team, user), in the µUSD units
// the governance pipeline consumes. Zero means "unlimited" on that dimension,
// mirroring TeamLimits.
//
// There is deliberately no RPM/TPM here: a user-subject `rate` rule is still
// refused by checkEnforceable, because a per-user rate limit needs a rate
// SHARE model this build does not have. Only `budget` was unblocked (ADR-042
// Phase 3).
type UserLimits struct {
	BudgetMicrosPerMonth int64
	BudgetHard           bool
	AdminContact         string
	BudgetMicrosPerDay   int64
	BudgetDayHard        bool
	AdminContactDay      string
}

// Store holds the data plane's currently-loaded local policy set behind an
// atomic snapshot: lookups on the request path never lock, and a reload
// swaps the whole set at once (the same generation posture as live.Holder,
// ADR-006). A failed reload keeps the previous snapshot serving.
//
// Enforceability gate: team budget/rate, user budget and modelAccess retain
// their existing checks. Budget tiers and the sensitive/context routing forms
// are exposed to routing consumers; cache affinity and user rate remain rejected.
type Store struct {
	paths []string
	snap  atomic.Pointer[snapshot]

	// routedAndPriced validates a routing TARGET model
	// against this data plane's own topology (ADR-041 D1): the target must
	// be routed (live.State.Route) and priced (pricing.HasRate). nil means
	// skip the check — the control plane holds no topology of its own and
	// deliberately does not apply it (LoadWirePaths), the same posture it
	// already takes toward checkEnforceable.
	routedAndPriced   func(model string) error
	sharedEnforcement bool
}

// SetSharedEnforcement is a startup-only capability declaration. It must only
// be enabled when the assembled gateway has a real shared admission authority.
func (s *Store) SetSharedEnforcement(enabled bool) { s.sharedEnforcement = enabled }

// SetRoutedAndPriced installs the apply-time target check for budgetTiers,
// sensitiveData.internalModels, and routing.context targets. A data plane must call this before Reload/ApplyWire so
// a rule naming an unrouted or unpriced target is rejected and reported
// upstream instead of silently billing 0 (ADR-030) once it activates.
func (s *Store) SetRoutedAndPriced(f func(model string) error) {
	s.routedAndPriced = f
}

type snapshot struct {
	generation string
	policies   []*Policy
	// rejectedRouting retains affected subjects after rejected mandatory privacy
	// OR strict budget-routing documents, until a valid correction arrives.
	rejectedRouting map[string][]Subject
	files           map[string]time.Time // watched file → mtime at load
	teams           map[string]TeamLimits
	users           map[userKey]UserLimits
}

// NewStore loads the given paths (files or directories) and fails on any
// parse, validation, or enforceability error — a data plane must not start
// while claiming to enforce a policy set it can't. Boot-time posture; use
// Reload/Watch afterwards.
func NewStore(paths ...string) (*Store, error) {
	s := &Store{paths: paths}
	if err := s.Reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// NewEmptyStore builds a store with no policies, to be fed by a control
// plane via ApplyWire (ADR-034). It has no file paths — Reload/Watch are
// meaningless on it and must not be used.
func NewEmptyStore() *Store {
	s := &Store{}
	s.snap.Store(&snapshot{teams: map[string]TeamLimits{}, users: map[userKey]UserLimits{}})
	return s
}

// ApplyWire replaces the policy set from control-plane-distributed wire
// documents. Unlike the all-or-nothing file path (an operator's local edit
// should fail fast and whole), distribution applies PER DOCUMENT: schema
// errors and rules this build cannot enforce reject that one document, the
// rest apply, and every rejection is returned for the next heartbeat's
// report — refused loudly upstream, never silently dropped (the version-skew
// stance). Atomic swap: readers see either the old set or the new one.
func (s *Store) ApplyWire(docs []v1alpha1.GovernancePolicy) []Rejection {
	var accepted []*Policy
	var rejected []Rejection
	seen := make(map[string]bool, len(docs))
	counts := make(map[string]int, len(docs))
	for i := range docs {
		counts[docs[i].Metadata.Name]++
	}
	previous := s.snap.Load()
	pending := make(map[string][]Subject)
	if previous != nil {
		for name, subjects := range previous.rejectedRouting {
			pending[name] = append([]Subject(nil), subjects...)
		}
	}
	rememberRejection := func(doc *v1alpha1.GovernancePolicy) {
		name := doc.Metadata.Name
		subjects := pending[name]
		add := func(sub Subject) {
			for _, old := range subjects {
				if old == sub {
					return
				}
			}
			subjects = append(subjects, sub)
		}
		for _, r := range doc.Spec.Rules {
			if r.TokenQuota != nil || r.SensitiveData != nil || (r.Routing != nil && r.Routing.BudgetTiers != nil && r.Routing.BudgetTiers.EnforceTargets) {
				add(Subject{Team: doc.Spec.Subject.Team, User: doc.Spec.Subject.User})
				break
			}
		}
		if previous != nil {
			for _, p := range previous.policies {
				if p.Name == name && hasMandatoryRouting(p) {
					add(p.Subject)
				}
			}
		}
		// A duplicate can reject a mandatory document accepted earlier in this
		// same distribution; preserve legacy first-document acceptance, but gate it.
		for _, p := range accepted {
			if p.Name == name && hasMandatoryRouting(p) {
				add(p.Subject)
			}
		}
		if len(subjects) > 0 {
			pending[name] = subjects
		}
	}
	for i := range docs {
		name := docs[i].Metadata.Name
		if name == "" || seen[name] {
			rememberRejection(&docs[i])
			rejected = append(rejected, Rejection{Policy: name, Reason: "metadata.name missing or duplicate in distributed set"})
			continue
		}
		seen[name] = true
		p, err := FromV1Alpha1(&docs[i])
		if err == nil {
			err = checkEnforceable(p, s.routedAndPriced, s.sharedEnforcement)
		}
		if err != nil {
			rememberRejection(&docs[i])
			rej := Rejection{Policy: name, Reason: err.Error()}
			var ue *UnsupportedError
			if errors.As(err, &ue) {
				rej.Rule = ue.Rule
				rej.Reason = ue.Reason
			}
			rejected = append(rejected, rej)
			continue
		}
		if counts[name] == 1 {
			delete(pending, name)
		}
		accepted = append(accepted, p)
	}
	// A nameless rejected document has no stable identity to correct individually.
	// Only a nonempty, entirely valid distribution can correct that ambiguity.
	if len(rejected) == 0 && len(docs) > 0 {
		delete(pending, "")
	}
	s.snap.Store(&snapshot{
		generation:      GenerationOf(docs),
		rejectedRouting: pending,
		policies:        accepted,
		teams:           mergeTeamLimits(accepted),
		users:           mergeUserLimits(accepted),
	})
	return rejected
}

// Reload re-reads every configured path and atomically swaps the snapshot.
// On error the previous snapshot (if any) stays in force.
func (s *Store) Reload() error {
	// Runtime guard, not just a doc comment (PR #50 review finding): on a
	// control-plane-fed store (NewEmptyStore) a stray generic Reload —
	// e.g. from a future SIGHUP handler — would otherwise silently WIPE
	// the distributed policy set with an empty file scan.
	if len(s.paths) == 0 {
		return errors.New("policy store has no file paths (control-plane-fed): Reload/Watch do not apply")
	}
	policies, files, err := LoadPaths(s.paths...)
	if err != nil {
		return err
	}
	for _, p := range policies {
		if err := checkEnforceable(p, s.routedAndPriced, s.sharedEnforcement); err != nil {
			return err
		}
	}

	mtimes := make(map[string]time.Time, len(files))
	for _, f := range files {
		if info, err := os.Stat(f); err == nil {
			mtimes[f] = info.ModTime()
		}
	}
	s.snap.Store(&snapshot{
		policies: policies,
		files:    mtimes,
		teams:    mergeTeamLimits(policies),
		users:    mergeUserLimits(policies),
	})
	return nil
}

// checkEnforceable rejects rules this data plane build cannot enforce yet.
// routedAndPriced is the ADR-041 apply-time target check for budgetTiers
// substitution rules (nil skips it — see Store.routedAndPriced doc).
func checkEnforceable(p *Policy, routedAndPriced func(model string) error, shared bool) error {
	reject := func(rule, reason string) error {
		return &UnsupportedError{APIVersion: SupportedAPIVersions[0], Kind: "GovernancePolicy", Rule: rule, Reason: reason}
	}
	for _, r := range p.Rules {
		if r.TokenQuota != nil && !shared {
			return reject(r.Name, "tokenQuota requires shared Postgres governance")
		}
		if routedAndPriced != nil {
			var targets []string
			if r.SensitiveData != nil {
				targets = append(targets, r.SensitiveData.InternalModels...)
			}
			if r.Routing != nil && r.Routing.Context != nil {
				targets = append(targets, r.Routing.Context.SimpleModel, r.Routing.Context.ComplexModel)
				if r.Routing.Context.NormalModel != "" {
					targets = append(targets, r.Routing.Context.NormalModel)
				}
			}
			for _, target := range targets {
				if err := routedAndPriced(target); err != nil {
					return reject(r.Name, fmt.Sprintf("routing policy target %q: %v", target, err))
				}
			}
		}
		if r.Routing != nil && r.Routing.Affinity != nil {
			return reject(r.Name, "cache-affinity routing rules are not yet enforceable by this data plane build")
		}
		if r.Routing != nil && r.Routing.BudgetTiers != nil && r.Routing.BudgetTiers.EnforceTargets &&
			(p.Subject.Team == "" || p.Subject.User != "") && !shared {
			return reject(r.Name, "strict budget tiers require a team-only subject in this build")
		}
		if r.Routing != nil && r.Routing.BudgetTiers != nil && routedAndPriced != nil {
			for _, tier := range r.Routing.BudgetTiers.Tiers {
				for _, target := range tier.Substitute {
					if err := routedAndPriced(target); err != nil {
						return reject(r.Name, fmt.Sprintf("routing.budgetTiers substitution target %q: %v", target, err))
					}
				}
			}
		}
		// Rate enforcement is still team-keyed, so ANY user-scoped variant —
		// user-only or (team, user) — must be refused rather than
		// accepted-and-ignored: it would otherwise pass validation, be
		// skipped by both merge functions and enforce nothing, the exact
		// silent failure this gate exists to prevent. Budget no longer needs
		// this gate — mergeUserLimits + Store.UserLimits + the Governor's
		// user lookup enforce a user-subject budget rule as of ADR-042
		// Phase 3.
		if r.Rate != nil && (p.Subject.Team == "" || p.Subject.User != "") && !shared {
			return reject(r.Name, "rate rules require a team-only subject in this build (user-scoped rate is not yet enforceable; user subjects support budget and modelAccess)")
		}
	}
	return nil
}

// Watch polls the loaded files' mtimes every LocalWatchInterval and reloads
// on any change (including a file appearing in a watched directory — the
// directory listing is re-walked on every reload). Reload errors keep the
// old snapshot and are passed to onErr; they do not stop the watch. Returns
// when ctx is done.
func (s *Store) Watch(ctx context.Context, onErr func(error)) {
	if len(s.paths) == 0 {
		if onErr != nil {
			onErr(errors.New("policy store has no file paths (control-plane-fed): Watch does not apply"))
		}
		return
	}
	t := time.NewTicker(LocalWatchInterval)
	defer t.Stop()
	var lastErr string // a broken file stays broken across polls; log it once, not every 2s
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !s.changed() {
				continue
			}
			err := s.Reload()
			if err == nil {
				lastErr = ""
				continue
			}
			if msg := err.Error(); msg != lastErr {
				lastErr = msg
				if onErr != nil {
					onErr(fmt.Errorf("policy reload (keeping previous set): %w", err))
				}
			}
		}
	}
}

// changed reports whether any watched file's mtime moved, a file vanished,
// or a watched directory gained a policy file.
func (s *Store) changed() bool {
	snap := s.snap.Load()
	files, err := enumerate(s.paths...)
	if err != nil || len(files) != len(snap.files) {
		return true
	}
	for _, f := range files {
		prev, ok := snap.files[f]
		if !ok {
			return true
		}
		info, err := os.Stat(f)
		if err != nil || !info.ModTime().Equal(prev) {
			return true
		}
	}
	return false
}

// Policies returns a deep copy of the current snapshot's policy set.
func (s *Store) Policies() []*Policy {
	policies := s.snap.Load().policies
	out := make([]*Policy, 0, len(policies))
	for _, p := range policies {
		out = append(out, clonePolicy(p))
	}
	return out
}

// TeamLimits returns the merged budget/rate limits of every team-subject
// policy matching team, and whether any matched.
func (s *Store) TeamLimits(team string) (TeamLimits, bool) {
	tl, ok := s.snap.Load().teams[team]
	return tl, ok
}

// UserLimits returns the merged budget limits of every user-subject policy
// matching (team, user), and whether any matched. Two entries can apply: a
// (team, user) subject and a user-only subject that matches this user in any
// team. Both fold, most-restrictive-wins per window.
func (s *Store) UserLimits(team, user string) (UserLimits, bool) {
	if user == "" {
		return UserLimits{}, false
	}
	m := s.snap.Load().users
	scoped, scopedOK := m[userKey{team: team, user: user}]
	global, globalOK := m[userKey{user: user}]
	switch {
	case scopedOK && globalOK:
		return scoped.narrow(global), true
	case scopedOK:
		return scoped, true
	case globalOK:
		return global, true
	}
	return UserLimits{}, false
}

// ModelAllowed reports whether every modelAccess rule matching the request's
// team and user allows the (already-canonicalized) model. ALL matching rules
// must allow — most-restrictive-wins across team- and user-subject policies
// (ADR-032). No matching modelAccess rule means no policy restriction.
// Allow-list entries are canonicalized through canon before comparing, the
// same alias posture as Router.Allows (ADR-021); "*" allows every model.
func (s *Store) ModelAllowed(team, user, model string, canon func(string) string) bool {
	for _, p := range s.snap.Load().policies {
		if !p.Subject.matches(team, user) {
			continue
		}
		for _, r := range p.Rules {
			if r.ModelAccess == nil {
				continue
			}
			if !allowsModel(r.ModelAccess.Allow, model, canon) {
				return false
			}
		}
	}
	return true
}

// matches reports whether a subject selects the given request identity. A
// set selector must match; an empty one doesn't constrain. (Validation
// guarantees at least one selector is set.)
func (sub Subject) matches(team, user string) bool {
	if sub.Team != "" && sub.Team != team {
		return false
	}
	if sub.User != "" && (user == "" || sub.User != user) {
		return false
	}
	return true
}

func allowsModel(allow []string, model string, canon func(string) string) bool {
	for _, a := range allow {
		if a == "*" || a == model {
			return true
		}
		if canon != nil && canon(a) == model {
			return true
		}
	}
	return false
}

// mergeTeamLimits folds every team-subject budget/rate rule into one
// TeamLimits per team, most-restrictive-wins PER WINDOW: the day and month
// budget windows fold independently, the smallest non-zero limit binds each
// dimension within its own window, and a window's budget is hard if the
// binding (smallest) budget rule for that window — or any equal to it — is a
// hard cap. An unlimited rule touches neither window. A finite soft budget
// referenced by a strict routing tier contributes only to the separate routing
// accounting threshold for its window, never to an admission cap.
//
// A team gets an entry ONLY when some rule actually contributed a budget or
// rate: a modelAccess-only policy must not manufacture an all-zero (=
// unlimited) entry, which the caller's lookup chain would let shadow the
// team's DB-record/config limits.
func mergeTeamLimits(policies []*Policy) map[string]TeamLimits {
	out := make(map[string]TeamLimits)
	for _, p := range policies {
		if p.Subject.Team == "" || p.Subject.User != "" {
			// Pure team subjects only. A user-scoped budget rule is now
			// ENFORCEABLE (ADR-042 Phase 3) and is folded by mergeUserLimits
			// instead; letting one in here would charge one person's cap to
			// their whole team. A user-scoped rate rule is still rejected by
			// checkEnforceable.
			continue
		}
		tl, contributed := out[p.Subject.Team]
		for _, r := range p.Rules {
			if r.Rate != nil {
				contributed = true
				tl.RPM = minNonZero(tl.RPM, r.Rate.RPM)
				tl.TPM = minNonZero(tl.TPM, r.Rate.TPM)
			}
			if r.Budget != nil {
				contributed = true
				if IsRoutingOnlyBudget(p, r.Name) {
					if r.Budget.Period == v1alpha1.PeriodCalendarDay {
						tl.RoutingBudgetMicrosPerDay = minNonZero(tl.RoutingBudgetMicrosPerDay, r.Budget.LimitMicroUSD)
					} else {
						tl.RoutingBudgetMicrosPerMonth = minNonZero(tl.RoutingBudgetMicrosPerMonth, r.Budget.LimitMicroUSD)
					}
					continue
				}
				switch {
				case r.Budget.Unlimited:
					// An explicit "no cap" declaration must never narrow OR
					// widen a binding limit set by another rule — it only
					// prevents this team from implicitly falling through to
					// a config/DB default when it's the ONLY budget rule
					// (handled below: LimitMicroUSD stays 0 in that case,
					// same "no rule" sentinel every consumer already
					// understands). Unlike the branches below, it never
					// compares against or overwrites either window's limit.
					// It carries no period, so it is window-agnostic: one
					// unlimited rule cannot unlimit the day and leave the
					// month capped, or vice versa.
				case r.Budget.Period == v1alpha1.PeriodCalendarDay:
					switch {
					case tl.BudgetMicrosPerDay == 0 || r.Budget.LimitMicroUSD < tl.BudgetMicrosPerDay:
						tl.BudgetMicrosPerDay = r.Budget.LimitMicroUSD
						tl.BudgetDayHard = r.Budget.HardCap
						tl.AdminContactDay = r.Budget.AdminContact
					case r.Budget.LimitMicroUSD == tl.BudgetMicrosPerDay && r.Budget.HardCap:
						tl.BudgetDayHard = true
						if tl.AdminContactDay == "" {
							tl.AdminContactDay = r.Budget.AdminContact
						}
					}
				default:
					// CalendarMonth, including a Budget built directly in a
					// test with a zero-value Period — the month window is the
					// meaning of "no period stated" everywhere in this schema.
					switch {
					case tl.BudgetMicrosPerMonth == 0 || r.Budget.LimitMicroUSD < tl.BudgetMicrosPerMonth:
						tl.BudgetMicrosPerMonth = r.Budget.LimitMicroUSD
						tl.BudgetHard = r.Budget.HardCap
						tl.AdminContact = r.Budget.AdminContact
					case r.Budget.LimitMicroUSD == tl.BudgetMicrosPerMonth && r.Budget.HardCap:
						tl.BudgetHard = true
						if tl.AdminContact == "" {
							tl.AdminContact = r.Budget.AdminContact
						}
					}
				}
			}
		}
		if contributed {
			out[p.Subject.Team] = tl
		}
	}
	return out
}

func minNonZero(a, b int64) int64 {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	case b < a:
		return b
	default:
		return a
	}
}

// userKey is mergeUserLimits's map key. A user-ONLY subject (no team) is
// stored under the zero team, which is what lets Store.UserLimits fold "this
// user anywhere" together with "this user in this team".
type userKey struct{ team, user string }

// narrow returns u narrowed by o, most-restrictive-wins PER WINDOW: the
// smaller non-zero limit binds each window independently, and a window is
// hard if the binding rule — or any rule equal to it — is a hard cap. This is
// mergeTeamLimits's ladder, factored out so the per-rule fold and the
// two-entry lookup fold cannot drift apart.
//
// THE `o.<window> == 0` GUARD IS LOAD-BEARING, and it is the one thing
// mergeTeamLimits does not need: mergeTeamLimits inspects a RULE, where the
// "no limit" case is `unlimited` and is skipped by the caller. narrow
// inspects an already-merged VIEW, which legitimately carries only one
// window. Without the guard, `u.month == 0 || o.month < u.month` is true for
// o.month == 0 whenever u.month > 0 — so folding a DAY-ONLY policy would
// ZERO OUT a month cap set by another policy, i.e. silently delete a money
// control. There is a test for exactly this.
func (u UserLimits) narrow(o UserLimits) UserLimits {
	switch {
	case o.BudgetMicrosPerMonth == 0:
		// contributes nothing to the month window
	case u.BudgetMicrosPerMonth == 0 || o.BudgetMicrosPerMonth < u.BudgetMicrosPerMonth:
		u.BudgetMicrosPerMonth, u.BudgetHard, u.AdminContact = o.BudgetMicrosPerMonth, o.BudgetHard, o.AdminContact
	case o.BudgetMicrosPerMonth == u.BudgetMicrosPerMonth && o.BudgetHard:
		u.BudgetHard = true
		if u.AdminContact == "" {
			u.AdminContact = o.AdminContact
		}
	}
	switch {
	case o.BudgetMicrosPerDay == 0:
		// contributes nothing to the day window
	case u.BudgetMicrosPerDay == 0 || o.BudgetMicrosPerDay < u.BudgetMicrosPerDay:
		u.BudgetMicrosPerDay, u.BudgetDayHard, u.AdminContactDay = o.BudgetMicrosPerDay, o.BudgetDayHard, o.AdminContactDay
	case o.BudgetMicrosPerDay == u.BudgetMicrosPerDay && o.BudgetDayHard:
		u.BudgetDayHard = true
		if u.AdminContactDay == "" {
			u.AdminContactDay = o.AdminContactDay
		}
	}
	return u
}

// userLimitsFromBudget lifts ONE budget rule into a single-window UserLimits,
// so narrow is the only place the comparison ladder exists. An `unlimited`
// rule is the caller's business, not this function's.
func userLimitsFromBudget(b *Budget) UserLimits {
	if b.Period == v1alpha1.PeriodCalendarDay {
		return UserLimits{BudgetMicrosPerDay: b.LimitMicroUSD, BudgetDayHard: b.HardCap, AdminContactDay: b.AdminContact}
	}
	// CalendarMonth, INCLUDING a Budget built directly in a test with a
	// zero-value Period: "no period stated" means the month window everywhere
	// in this schema, so month must be the `default`, never an explicit
	// `== PeriodCalendarMonth` equality case.
	return UserLimits{BudgetMicrosPerMonth: b.LimitMicroUSD, BudgetHard: b.HardCap, AdminContact: b.AdminContact}
}

// mergeUserLimits folds every USER-subject budget rule into one UserLimits
// per (team, user), most-restrictive-wins per window (see narrow). Rate rules
// are ignored here: checkEnforceable still refuses a user-subject rate rule,
// so one cannot reach this function.
//
// A (team, user) entry appears ONLY when some rule actually contributed a
// budget — a modelAccess-only user policy must not manufacture an all-zero
// (= unlimited) entry, for exactly the reason mergeTeamLimits documents: the
// caller's lookup chain would let it shadow a real limit.
func mergeUserLimits(policies []*Policy) map[userKey]UserLimits {
	out := make(map[userKey]UserLimits)
	for _, p := range policies {
		if p.Subject.User == "" {
			continue // team-only subjects are mergeTeamLimits' business
		}
		k := userKey{team: p.Subject.Team, user: p.Subject.User}
		ul, contributed := out[k]
		for _, r := range p.Rules {
			if r.Budget == nil {
				continue
			}
			contributed = true
			if r.Budget.Unlimited {
				// Same posture as mergeTeamLimits: an explicit "no cap"
				// neither narrows nor widens another rule's limit; it only
				// stops this subject from being treated as having no rule at
				// all. It carries no period, so it is window-agnostic.
				continue
			}
			ul = ul.narrow(userLimitsFromBudget(r.Budget))
		}
		if contributed {
			out[k] = ul
		}
	}
	return out
}

// ErrRoutingPolicyRejected means mandatory privacy or strict budget routing
// cannot safely be enforced. Callers must deny before any upstream request.
var ErrRoutingPolicyRejected = errors.New("mandatory routing policy distribution rejected")

// ErrSensitivePolicyRejected preserves the original sentinel identity for
// existing callers. It now also covers rejected mandatory budget routing.
var ErrSensitivePolicyRejected = ErrRoutingPolicyRejected

// MatchingRoutingPolicies loads one snapshot and returns matching sensitiveData
// and routing.context rules, with policy names/generations and owned nested data.
// Rejections of current OR formerly mandatory privacy/strict-budget documents
// fail closed for all affected subjects until a valid replacement is applied.
// This gate applies even when no context/privacy rules remain to return. A
// missing subject on a rejected mandatory document matches everyone.
func (s *Store) MatchingRoutingPolicies(team, user string) ([]*Policy, error) {
	docs, _, err := s.RoutingSnapshot(team, user)
	return docs, err
}

// RoutingSnapshot binds a decision to the exact distributed bundle, including
// when no routing rules match. Shared admission compares this generation with
// the authoritative database before dispatch.
func (s *Store) RoutingSnapshot(team, user string) ([]*Policy, string, error) {
	snap := s.snap.Load()
	for _, subjects := range snap.rejectedRouting {
		for _, sub := range subjects {
			if sub.matches(team, user) {
				return nil, snap.generation, ErrRoutingPolicyRejected
			}
		}
	}
	var out []*Policy
	for _, p := range snap.policies {
		if !p.Subject.matches(team, user) {
			continue
		}
		cp := *p
		cp.Rules = nil
		for _, r := range p.Rules {
			if r.SensitiveData != nil || (r.Routing != nil && r.Routing.Context != nil) {
				cp.Rules = append(cp.Rules, cloneRule(r))
			}
		}
		if len(cp.Rules) > 0 {
			out = append(out, &cp)
		}
	}
	return out, snap.generation, nil
}

func hasMandatoryRouting(p *Policy) bool {
	for _, r := range p.Rules {
		if r.TokenQuota != nil || r.SensitiveData != nil || (r.Routing != nil && r.Routing.BudgetTiers != nil && r.Routing.BudgetTiers.EnforceTargets) {
			return true
		}
	}
	return false
}

func clonePolicy(p *Policy) *Policy {
	cp := *p
	cp.Rules = make([]Rule, len(p.Rules))
	for i, r := range p.Rules {
		cp.Rules[i] = cloneRule(r)
	}
	return &cp
}

func cloneRule(r Rule) Rule {
	if r.TokenQuota != nil {
		q := *r.TokenQuota
		r.TokenQuota = &q
	}
	if r.Budget != nil {
		b := *r.Budget
		r.Budget = &b
	}
	if r.Rate != nil {
		rate := *r.Rate
		r.Rate = &rate
	}
	if r.ModelAccess != nil {
		a := *r.ModelAccess
		a.Allow = append([]string(nil), a.Allow...)
		r.ModelAccess = &a
	}
	if r.SensitiveData != nil {
		sd := *r.SensitiveData
		sd.InternalModels = append([]string(nil), sd.InternalModels...)
		r.SensitiveData = &sd
	}
	if r.Routing != nil {
		rt := *r.Routing
		r.Routing = &rt
		if rt.Affinity != nil {
			a := *rt.Affinity
			rt.Affinity = &a
		}
		if rt.Context != nil {
			c := *rt.Context
			c.FromModels = append([]string(nil), c.FromModels...)
			c.ComplexKeywords = append([]string(nil), c.ComplexKeywords...)
			if c.Stability != nil {
				stability := *c.Stability
				c.Stability = &stability
			}
			rt.Context = &c
		}
		if rt.BudgetTiers != nil {
			bt := *rt.BudgetTiers
			source := bt.Tiers
			rt.BudgetTiers = &bt
			bt.Tiers = make([]BudgetTier, len(source))
			for i, t := range source {
				bt.Tiers[i] = BudgetTier{ThresholdPercent: t.ThresholdPercent, Substitute: maps.Clone(t.Substitute)}
			}
		}
	}
	return r
}
