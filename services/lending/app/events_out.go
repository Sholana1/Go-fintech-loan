package app

import (
	"context"

	"bankplatform.internal/platform/outbox"
	"bankplatform.internal/services/lending/domain"
	"bankplatform.internal/services/lending/postgres"
)

// TopicLoans carries every lending event. Events of one application and its
// loan share a partition key (the application id), so a consumer sees a
// loan's history in order.
const TopicLoans = "lending.loan.v1"

type applicationEvent struct {
	ApplicationID string   `json:"application_id"`
	CustomerID    string   `json:"customer_id"`
	ProductID     string   `json:"product_id"`
	State         string   `json:"state"`
	ReasonCodes   []string `json:"reason_codes,omitempty"`
	OfferID       string   `json:"offer_id,omitempty"`
}

type loanEvent struct {
	LoanID         string `json:"loan_id"`
	ApplicationID  string `json:"application_id"`
	CustomerID     string `json:"customer_id"`
	State          string `json:"state"`
	PrincipalMinor int64  `json:"principal_minor,omitempty"`
	AmountMinor    int64  `json:"amount_minor,omitempty"`
	JournalID      int64  `json:"journal_id,omitempty"`
	RepaymentID    string `json:"repayment_id,omitempty"`
	PayoutID       string `json:"payout_id,omitempty"`
	// ExternalPaymentID is set on loan.external_payment.* events.
	ExternalPaymentID string `json:"external_payment_id,omitempty"`
	Detail            string `json:"detail,omitempty"`
}

func (s *Service) emitApplication(ctx context.Context, q *postgres.Queries, eventType string, a domain.Application, state domain.ApplicationState, reasons []string, offerID string, version int64) error {
	return q.Emit(ctx, outbox.Event{
		Topic: TopicLoans, PartitionKey: a.ID.String(), Type: eventType, SchemaVersion: 1,
		AggregateType: "loan_application", AggregateID: a.ID.String(), AggregateVersion: version,
		Data: applicationEvent{ApplicationID: a.ID.String(), CustomerID: a.CustomerID.String(), ProductID: a.ProductID,
			State: string(state), ReasonCodes: reasons, OfferID: offerID},
	})
}

func (s *Service) emitLoan(ctx context.Context, q *postgres.Queries, eventType string, l domain.Loan, ev loanEvent) error {
	ev.LoanID, ev.ApplicationID, ev.CustomerID = l.ID.String(), l.ApplicationID.String(), l.CustomerID.String()
	if ev.State == "" {
		ev.State = string(l.State)
	}
	return q.Emit(ctx, outbox.Event{
		Topic: TopicLoans, PartitionKey: l.ApplicationID.String(), Type: eventType, SchemaVersion: 1,
		AggregateType: "loan", AggregateID: l.ID.String(), AggregateVersion: l.Version + 1,
		Data: ev,
	})
}
