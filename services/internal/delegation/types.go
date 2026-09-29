package delegation

import (
	"time"

	"exchange/pkg/decimal"
)

// Client-side role vocabulary (Task 12.3.11) — distinct from the §8.2
// venue-admin roles; a principal holding a venue role can never hold a
// client role (principal_role_systems disjoint exclusion).
type Role string

const (
	RoleReadOnly       Role = "CLIENT_READ_ONLY"
	RoleFinanceManager Role = "CLIENT_FINANCE_MANAGER"
	RoleTrader         Role = "CLIENT_TRADER"
	RoleApprover       Role = "CLIENT_APPROVER"
)

// ValidRole reports whether r is a grantable client role.
func ValidRole(r Role) bool {
	switch r {
	case RoleReadOnly, RoleFinanceManager, RoleTrader, RoleApprover:
		return true
	}
	return false
}

// Action is one capability axis the enforcement helper evaluates.
type Action string

const (
	ActionRead              Action = "READ"
	ActionTrade             Action = "TRADE"              // order entry/cancel
	ActionInternalTransfer  Action = "INTERNAL_TRANSFER"  // within the family
	ActionWithdrawal        Action = "WITHDRAWAL"         // external money-out
	ActionBeneficiaryChange Action = "BENEFICIARY_CHANGE" // registry mutation
	ActionAPIKeyManage      Action = "API_KEY_MANAGE"     // create/revoke keys
	ActionSecurityChange    Action = "SECURITY_CHANGE"    // 2FA/password/session policy
	ActionApprove           Action = "APPROVE"            // M-of-N decision
)

// roleActions is the capability matrix (Task 12.3.11 item 2):
//   - finance managers transfer only within the entitled hierarchy
//     (scope-checked against the master family — never withdraw);
//   - traders cannot withdraw or change security;
//   - approvers approve but never initiate configured operations.
var roleActions = map[Role]map[Action]bool{
	RoleReadOnly: {ActionRead: true},
	RoleTrader:   {ActionRead: true, ActionTrade: true},
	RoleFinanceManager: {
		ActionRead:             true,
		ActionInternalTransfer: true,
	},
	RoleApprover: {ActionRead: true, ActionApprove: true},
}

// Operation names the governed operations a policy may cover.
type Operation string

const (
	OpWithdrawal        Operation = "WITHDRAWAL"
	OpBeneficiaryChange Operation = "BENEFICIARY_CHANGE"
	OpAPIKeyPrivilege   Operation = "API_KEY_PRIVILEGE_CHANGE"
	OpInternalTransfer  Operation = "INTERNAL_TRANSFER"
)

// ValidOperation reports whether op is a policy-eligible operation.
func ValidOperation(op Operation) bool {
	switch op {
	case OpWithdrawal, OpBeneficiaryChange, OpAPIKeyPrivilege, OpInternalTransfer:
		return true
	}
	return false
}

// opAction maps a governed operation to the initiating capability — a
// delegate may only request an operation its role can perform (an
// approver can never initiate). The master owner initiates anything.
var opAction = map[Operation]Action{
	OpWithdrawal:        ActionWithdrawal,
	OpBeneficiaryChange: ActionBeneficiaryChange,
	OpAPIKeyPrivilege:   ActionAPIKeyManage,
	OpInternalTransfer:  ActionInternalTransfer,
}

// Delegated user + binding lifecycle statuses.
const (
	StatusActive    = "ACTIVE"
	StatusSuspended = "SUSPENDED"
	StatusRevoked   = "REVOKED"
	StatusExpired   = "EXPIRED"
)

// Approval-request lifecycle.
const (
	ReqPending   = "PENDING"
	ReqApproved  = "APPROVED"
	ReqRejected  = "REJECTED"
	ReqExpired   = "EXPIRED"
	ReqCancelled = "CANCELLED"
	ReqConsumed  = "CONSUMED"
)

// Audit event names (client_delegation_events.event).
const (
	EvDelegationCreated = "DELEGATION_CREATED"
	EvBindingUpdated    = "SCOPE_CHANGED"
	EvDelegatedLogin    = "DELEGATED_LOGIN"
	EvDelegatedAction   = "DELEGATED_ACTION"
	EvDelegationRevoked = "DELEGATION_REVOKED"
	EvDelegationSuspend = "DELEGATION_SUSPENDED"
	EvRevokeAll         = "MASTER_REVOKE_ALL"
	EvPolicySet         = "POLICY_SET"
	EvPolicyDisabled    = "POLICY_DISABLED"
	EvApprovalRequested = "APPROVAL_REQUESTED"
	EvApprovalApproved  = "APPROVAL_APPROVED"
	EvApprovalRejected  = "APPROVAL_REJECTED"
	EvApprovalExpired   = "APPROVAL_EXPIRED"
	EvApprovalConsumed  = "APPROVAL_CONSUMED"
)

// Scope is the explicit allowlist attached to a client_role_bindings
// row. An absent/empty axis grants nothing (fail-closed).
type Scope struct {
	AccountIDs  []int64  `json:"account_ids,omitempty"` // master + sub-account ids
	Instruments []string `json:"instruments,omitempty"` // symbols, e.g. "EURUSD"
}

// CoversAccount reports whether the scope grants the target account.
func (s Scope) CoversAccount(id int64) bool {
	for _, a := range s.AccountIDs {
		if a == id {
			return true
		}
	}
	return false
}

// CoversInstrument reports whether the scope grants the symbol.
func (s Scope) CoversInstrument(symbol string) bool {
	for _, sym := range s.Instruments {
		if sym == symbol {
			return true
		}
	}
	return false
}

// DelegatedUser is one named human login under a master account.
type DelegatedUser struct {
	ID            int64      `json:"id"`
	MasterAccount int64      `json:"master_account_id"`
	UserID        int64      `json:"user_id"`
	DisplayName   string     `json:"display_name"`
	Status        string     `json:"status"`
	CreatedBy     int64      `json:"created_by"`
	CreatedAt     time.Time  `json:"created_at"`
	SuspendReason string     `json:"suspend_reason,omitempty"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
	RevokedBy     *int64     `json:"revoked_by,omitempty"`
	RevokeReason  string     `json:"revoke_reason,omitempty"`
	// Active binding, when present.
	Role           Role       `json:"role,omitempty"`
	Scope          Scope      `json:"scope,omitempty"`
	BindingID      int64      `json:"binding_id,omitempty"`
	BindingExpires *time.Time `json:"binding_expires_at,omitempty"`
}

// Policy is one M-of-N approval policy row.
type Policy struct {
	ID                int64            `json:"id"`
	MasterAccount     int64            `json:"master_account_id"`
	Operation         Operation        `json:"operation"`
	RequiredApprovals int              `json:"required_approvals"` // M
	ThresholdAmount   *decimal.Decimal `json:"threshold_amount,omitempty"`
	ThresholdCurrency string           `json:"threshold_currency,omitempty"`
	ExpiresInSeconds  int              `json:"expires_in_seconds"`
	Status            string           `json:"status"`
	CreatedBy         int64            `json:"created_by"`
	CreatedAt         time.Time        `json:"created_at"`
	UpdatedAt         time.Time        `json:"updated_at"`
}

// Applies reports whether the policy covers this (amount, currency):
// no threshold → always; threshold → same currency AND amount >= floor.
func (p *Policy) Applies(amount decimal.Decimal, currency string) bool {
	if p.ThresholdAmount == nil {
		return true
	}
	if p.ThresholdCurrency != "" && p.ThresholdCurrency != currency {
		return false
	}
	return !amount.LessThan(*p.ThresholdAmount)
}

// ApprovalRequest is one pending/decided M-of-N request.
type ApprovalRequest struct {
	ID                   int64          `json:"id"`
	MasterAccount        int64          `json:"master_account_id"`
	PolicyID             int64          `json:"policy_id"`
	Operation            Operation      `json:"operation"`
	Payload              map[string]any `json:"payload"`
	Fingerprint          string         `json:"fingerprint"`
	RequestedByUser      int64          `json:"requested_by_user"`
	RequestedByDelegated *int64         `json:"requested_by_delegated,omitempty"`
	Status               string         `json:"status"`
	ApprovalsCount       int            `json:"approvals_count"`
	RequiredApprovals    int            `json:"required_approvals"`
	ExpiresAt            time.Time      `json:"expires_at"`
	CreatedAt            time.Time      `json:"created_at"`
	DecidedAt            *time.Time     `json:"decided_at,omitempty"`
	ConsumedAt           *time.Time     `json:"consumed_at,omitempty"`
}

// Decision is one approver vote row.
type Decision struct {
	ID              int64     `json:"id"`
	RequestID       int64     `json:"request_id"`
	ApproverUserID  int64     `json:"approver_user_id"`
	DelegatedUserID *int64    `json:"delegated_user_id,omitempty"`
	Decision        string    `json:"decision"` // APPROVE | REJECT
	Note            string    `json:"note,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

// Event is one client_delegation_events audit row.
type Event struct {
	ID            int64          `json:"id"`
	MasterAccount int64          `json:"master_account_id"`
	ActorUserID   *int64         `json:"actor_user_id,omitempty"`
	DelegatedID   *int64         `json:"delegated_user_id,omitempty"`
	Event         string         `json:"event"`
	TargetType    string         `json:"target_type,omitempty"`
	TargetID      *int64         `json:"target_id,omitempty"`
	Detail        map[string]any `json:"detail"`
	CreatedAt     time.Time      `json:"created_at"`
}

// Identity is the resolved delegated-session context: the delegated
// user row plus its ACTIVE binding.
type Identity struct {
	DelegatedUserID int64 `json:"delegated_user_id"`
	MasterAccount   int64 `json:"master_account_id"`
	UserID          int64 `json:"user_id"`
	Role            Role  `json:"role"`
	Scope           Scope `json:"scope"`
}

// Target is what a delegated action acts upon. Zero fields are
// unconstrained axes of the call (the action decides which matter).
type Target struct {
	AccountID    int64  // account being acted on (must be in scope)
	Instrument   string // trading symbol (TRADE actions)
	Counterparty int64  // transfer destination (INTERNAL_TRANSFER)
}
