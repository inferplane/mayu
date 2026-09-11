package policy

import (
	"time"

	"github.com/inferplane/inferplane/api/v1alpha1"
)

const AuthorityProtocol = "escrow-v1"

// AuthorityBudget is a database-clock-owned budget window. Key identifies the
// policy/rule/subject/period independently of edits or process restarts.
type AuthorityBudget struct {
	Key           string    `json:"key"`
	Policy        string    `json:"policy"`
	Rule          string    `json:"rule"`
	Team          string    `json:"team,omitempty"`
	User          string    `json:"user,omitempty"`
	Revision      string    `json:"revision"`
	WindowID      string    `json:"windowID"`
	WindowStart   time.Time `json:"windowStart"`
	WindowEnd     time.Time `json:"windowEnd"`
	LimitMicroUSD int64     `json:"limitMicroUSD"`
	GrantMicroUSD int64     `json:"grantMicroUSD"`
	LeaseSeconds  int       `json:"leaseSeconds"`
	HardCap       bool      `json:"hardCap"`
}

// RequestID is an unguessable node-generated idempotency/capability nonce.
// Keep it in the private journal; never emit it to metrics, logs or clients.
type AuthorityGrantRequest struct {
	RequestID    string `json:"requestID"`
	Key          string `json:"key"`
	Revision     string `json:"revision"`
	WindowID     string `json:"windowID"`
	WantMicroUSD int64  `json:"wantMicroUSD"`
}

type AuthorityGrant struct {
	ID        string    `json:"id"`
	RequestID string    `json:"requestID"`
	Key       string    `json:"key"`
	Revision  string    `json:"revision"`
	WindowID  string    `json:"windowID"`
	Amount    int64     `json:"amountMicroUSD"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// Consumed includes observed cost plus conservative authority retained for
// uncertain attempts. Closing returns only Amount-Consumed; expiry never refunds.
type AuthorityReport struct {
	Instance  string `json:"instance"`
	GrantID   string `json:"grantID"`
	RequestID string `json:"requestID"`
	Sequence  int64  `json:"sequence"`
	Consumed  int64  `json:"consumedMicroUSD"`
	Observed  int64  `json:"observedMicroUSD"`
	Closed    bool   `json:"closed"`
	Overrun   bool   `json:"overrun,omitempty"`
}

// Meter reports soft/accounting-only windows. It cannot authorize a hard-cap
// request. Its cumulative values and sequence are scoped to a unique boot.
type AuthorityMeter struct {
	Instance string `json:"instance"`
	Key      string `json:"key"`
	WindowID string `json:"windowID"`
	Sequence int64  `json:"sequence"`
	Consumed int64  `json:"consumedMicroUSD"`
	Observed int64  `json:"observedMicroUSD"`
}

type AuthorityRequest struct {
	Protocol string                  `json:"protocol"`
	Instance string                  `json:"instance"`
	Requests []AuthorityGrantRequest `json:"requests,omitempty"`
	Reports  []AuthorityReport       `json:"reports,omitempty"`
	Meters   []AuthorityMeter        `json:"meters,omitempty"`
}

type AuthorityAck struct {
	Instance string `json:"instance,omitempty"`
	ID       string `json:"id"`
	Sequence int64  `json:"sequence"`
}

type AuthorityDenial struct {
	RequestID string `json:"requestID"`
	Key       string `json:"key"`
	Reason    string `json:"reason"`
}

type AuthorityResponse struct {
	Protocol   string                      `json:"protocol"`
	ServerTime time.Time                   `json:"serverTime"`
	Generation string                      `json:"generation"`
	Policies   []v1alpha1.GovernancePolicy `json:"policies"`
	Budgets    []AuthorityBudget           `json:"budgets"`
	Grants     []AuthorityGrant            `json:"grants,omitempty"`
	ReportAcks []AuthorityAck              `json:"reportAcks,omitempty"`
	MeterAcks  []AuthorityAck              `json:"meterAcks,omitempty"`
	Denied     []AuthorityDenial           `json:"denied,omitempty"`
}
