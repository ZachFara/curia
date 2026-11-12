package model

import "time"

// DecisionStatus is where a decision ticket is in its life.
type DecisionStatus string

// Decision statuses.
const (
	StatusOpen       DecisionStatus = "open"
	StatusAnswered   DecisionStatus = "answered"
	StatusConfirmed  DecisionStatus = "confirmed"
	StatusOverridden DecisionStatus = "overridden"
	StatusWithdrawn  DecisionStatus = "withdrawn"
	StatusExpired    DecisionStatus = "expired"
)

// Mode says whether the agent waits for the answer or proceeds on an assumption.
type Mode string

// Decision modes.
const (
	ModeBlocking Mode = "blocking"
	ModeAssume   Mode = "assume"
)

// Reversibility is how costly it is to undo the decision.
type Reversibility string

// Reversibility levels.
const (
	ReversCheap        Reversibility = "cheap"
	ReversCostly       Reversibility = "costly"
	ReversIrreversible Reversibility = "irreversible"
)

// Scope is how far a decision's answer applies.
type Scope string

// Decision scopes.
const (
	ScopeTask   Scope = "task"
	ScopeRepo   Scope = "repo"
	ScopeGlobal Scope = "global"
)

// Delivery is the delivery state of an answer.
type Delivery string

// Delivery states.
const (
	DeliveryNone      Delivery = "none"
	DeliveryQueued    Delivery = "queued"
	DeliveryDelivered Delivery = "delivered"
)

// DeliveryVia is the path that delivered an answer.
type DeliveryVia string

// Delivery paths.
const (
	ViaSendKeys DeliveryVia = "send_keys"
	ViaStopHook DeliveryVia = "stop_hook"
	ViaCheck    DeliveryVia = "check"
)

// AnsweredVia is where an answer was given.
type AnsweredVia string

// Answer origins.
const (
	AnsweredPopup AnsweredVia = "popup"
	AnsweredSSH   AnsweredVia = "ssh"
	AnsweredCLI   AnsweredVia = "cli"
)

// Option is one choice offered by a decision.
type Option struct {
	Key       string `json:"key"`
	Label     string `json:"label"`
	Tradeoffs string `json:"tradeoffs"`
}

// Decision is one row of the decisions table. ConfirmedAt is, for an
// answer delivered by send_keys, when the agent was seen to get it (its
// prompt naming the decision was submitted, or it called check_decision);
// zero while the typed answer is unconfirmed (Ruling 86).
type Decision struct {
	N                                          int64
	ID, SessionID, TaskID, Repo                string
	Question, Context                          string
	Options                                    []Option
	Recommendation, Rationale                  string
	Reversibility                              Reversibility
	Mode                                       Mode
	Assumption                                 string
	Scope                                      Scope
	Tags                                       []string
	Status                                     DecisionStatus
	AnswerKey, Answer, AnswerReasoning         string
	AnsweredVia                                AnsweredVia
	PrecedentOf, DiffersFrom, DiffersWhy       string
	WithdrawReason, SupersededBy, ArgsHash     string
	Delivery                                   Delivery
	DeliveredVia                               DeliveryVia
	CreatedAt, ExpiresAt, AnsweredAt, ClosedAt time.Time
	SupersededAt, DeliveredAt, ConfirmedAt     time.Time
}

// Ticket is an open decision with what the queue shows about its session.
type Ticket struct {
	Decision
	SessionState                          State // "" when the session is unknown
	Pane, TmuxSession, TmuxWindow, Branch string
	ContextUsed                           int
	CacheExpiresAt                        time.Time // zero: unknown
	CacheRebuildUSD                       float64
}

// Answer is what curia decide writes. Exactly one of Key and Text is set.
type Answer struct {
	Key, Text, Reasoning string
	Scope                Scope // "" keeps the ticket's scope
	Via                  AnsweredVia
}

// Outcome is what happened to an answer's delivery.
type Outcome struct {
	Decision Decision
	Note     string // "" when delivered; otherwise why it is still queued or needs none
}

// OpenCount counts open tickets by mode.
type OpenCount struct{ Blocking, Assume int }

// DecisionEdit rescopes and/or supersedes a decided ticket.
type DecisionEdit struct {
	Scope        Scope // "" keeps it
	Supersede    bool
	SupersededBy string // optional id of the replacing decision
}
