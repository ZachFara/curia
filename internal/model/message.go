package model

import "time"

// MessageKind is what a message asks for.
type MessageKind string

// Message kinds.
const (
	KindInstruction MessageKind = "instruction"
	KindRule        MessageKind = "rule"
	KindTask        MessageKind = "task"
)

// MessageStatus is the delivery status of a message.
type MessageStatus string

// Message statuses.
const (
	MsgHeld      MessageStatus = "held"
	MsgQueued    MessageStatus = "queued"
	MsgDelivered MessageStatus = "delivered"
	MsgUndone    MessageStatus = "undone"
	MsgCanceled  MessageStatus = "canceled"
)

// RoutedBy says how a message's target was chosen.
type RoutedBy string

// Routing origins.
const (
	RoutedAddress    RoutedBy = "address"
	RoutedConfirmed  RoutedBy = "confirmed"
	RoutedOverridden RoutedBy = "overridden"
	RoutedAuto       RoutedBy = "auto"
	RoutedManual     RoutedBy = "manual"
	RoutedSend       RoutedBy = "send"
)

// Sender says who wrote a message.
type Sender string

// Senders.
const (
	SenderUser  Sender = "user"
	SenderAgent Sender = "agent"
)

// ViaApplied is the delivery path of a rule or task message once stored;
// messages also use ViaSendKeys, ViaStopHook and ViaCheck.
const ViaApplied DeliveryVia = "applied"

// Message is one row of the messages table.
type Message struct {
	N                                              int64
	ID, Text                                       string
	Kind                                           MessageKind
	Sender                                         Sender
	SenderSession                                  string
	TargetSession, TargetTask, TargetRepo          string
	SuggestedTarget, RunnerUp                      string
	SuggestedKind                                  MessageKind
	Confidence                                     float64
	HasConfidence                                  bool // false: no model was asked (confidence is NULL)
	RoutedBy                                       RoutedBy
	AutoEligible                                   bool
	Status                                         MessageStatus
	DeliveredVia                                   DeliveryVia
	ResultRef                                      string
	CreatedAt, HoldUntil, DeliveredAt, ConfirmedAt time.Time
}

// MessageOutcome is what happened to a message's delivery; Note is "" when
// delivered or applied.
type MessageOutcome struct {
	Message Message
	Note    string
}

// RouteLabel is one confirmed or overridden routing, for the accuracy gate.
type RouteLabel struct {
	Confidence float64
	Accepted   bool
}
