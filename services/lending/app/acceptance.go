package app

import (
	"context"
	"crypto/subtle"
	"fmt"

	"github.com/google/uuid"

	"bankplatform.internal/services/ledger/contract"
	"bankplatform.internal/services/lending/domain"
	"bankplatform.internal/services/lending/postgres"
)

const endpointAccept = "POST /v1/loan-offers/{id}/accept"

// AcceptRequest is the customer's acceptance of an offer.
type AcceptRequest struct {
	// DisclosureHash must equal the hash of the disclosure the offer carries.
	// It proves the customer accepted these exact terms.
	DisclosureHash string `json:"disclosure_hash"`
	// PIN is the step-up credential. It is verified by the identity service
	// and is never stored, logged or included in the idempotency hash.
	PIN string `json:"pin"`
	// AutoDebitAuthorised authorises scheduled repayment debits of the
	// customer's own deposit account on each due date.
	AutoDebitAuthorised bool        `json:"auto_debit_authorised"`
	Destination         Destination `json:"destination"`
}

// Acceptor identifies who is accepting, for the evidence record.
type Acceptor struct {
	CustomerID uuid.UUID
	TokenID    string // the access token's id (jti)
}

// AcceptOffer accepts an offer (T2), books the loan, and starts its
// disbursement. It returns the loan; replay is true when the idempotency key
// had already produced it.
//
// What prevents each failure:
//   - Expired or superseded offer: the acceptance is one UPDATE guarded by
//     state = 'OFFERED' AND expires_at > now. It cannot match an expired,
//     voided or already accepted offer.
//   - Two concurrent acceptances: the same UPDATE matches for exactly one.
//   - Two loans for one offer: UNIQUE(offer_id) and UNIQUE(application_id).
//   - Two disbursements for one loan: the disbursement is a posting intent
//     whose id is the ledger posting reference.
//   - Paying someone else: the internal destination is derived from the
//     customer id; an external destination must pass a name enquiry that
//     matches the customer's verified name.
//
// No money moves inside this function's transaction. The transaction records
// the intent to disburse; the posting driver performs it and only then is
// the loan marked ACTIVE.
func (s *Service) AcceptOffer(ctx context.Context, who Acceptor, offerID uuid.UUID, idemKey string, req AcceptRequest) (loan domain.Loan, replay bool, err error) {
	if err := validateIdemKey(idemKey); err != nil {
		return loan, false, err
	}
	if req.Destination.Type == "" {
		req.Destination.Type = DestinationDeposit
	}
	if err := validateDestination(req.Destination); err != nil {
		return loan, false, err
	}
	// The PIN is excluded from the fingerprint: it must not be persisted in
	// any form, and a retry after a mistyped PIN is the same request.
	hash, err := requestHash(struct {
		OfferID uuid.UUID
		Hash    string
		Auto    bool
		Dest    Destination
	}{offerID, req.DisclosureHash, req.AutoDebitAuthorised, req.Destination})
	if err != nil {
		return loan, false, err
	}

	// replayed answers a retry: if this key already produced a loan, that
	// loan is the response, whatever state the offer is in now.
	replayed := func() (domain.Loan, bool, error) {
		id, found, err := s.store.LookupIdempotencyKey(ctx, who.CustomerID, endpointAccept, idemKey, hash)
		if err != nil || !found {
			return domain.Loan{}, false, err
		}
		s.metrics.IdempotentReplay(endpointAccept)
		l, err := s.store.GetLoanForCustomer(ctx, id, who.CustomerID)
		return l, true, err
	}
	if l, found, err := replayed(); err != nil || found {
		return l, found, err
	}

	offer, err := s.store.GetOfferForCustomer(ctx, offerID, who.CustomerID)
	if err != nil {
		return loan, false, err
	}
	now := s.now()
	// Early, side-effect-free checks give precise errors. The authoritative
	// check is the compare-and-set inside the transaction below.
	var early error
	switch {
	case offer.State == domain.OfferExpired || (offer.State == domain.OfferOpen && !now.Before(offer.ExpiresAt)):
		early = domain.ErrOfferExpired
	case offer.State != domain.OfferOpen:
		early = domain.ErrOfferNotOpen
	case subtle.ConstantTimeCompare([]byte(req.DisclosureHash), []byte(offer.DisclosureHash)) != 1:
		early = domain.ErrDisclosureMismatch
	}
	if early != nil {
		// The offer may have stopped being open because a concurrent request
		// carrying this same key accepted it between the lookup above and
		// the read of the offer. In that case this request is a replay, not
		// a conflict. Look again before refusing.
		if l, found, err := replayed(); err != nil || found {
			return l, found, err
		}
		return loan, false, early
	}

	// Step-up authentication. A dependency failure is not a failed PIN.
	stepCtx, cancel := context.WithTimeout(ctx, s.cfg.IdentityTimeout)
	verified, locked, err := s.identity.VerifyPIN(stepCtx, who.CustomerID, req.PIN)
	cancel()
	switch {
	case err != nil:
		return loan, false, fmt.Errorf("%w: identity: %v", domain.ErrDependencyUnavailable, err)
	case locked:
		return loan, false, domain.ErrStepUpLocked
	case !verified:
		return loan, false, domain.ErrStepUpFailed
	}

	custCtx, cancel := context.WithTimeout(ctx, s.cfg.IdentityTimeout)
	customer, err := s.identity.GetCustomer(custCtx, who.CustomerID)
	cancel()
	if err != nil {
		return loan, false, fmt.Errorf("%w: identity: %v", domain.ErrDependencyUnavailable, err)
	}
	if !customer.Active || !customer.KYCVerified || customer.DepositAccountCode == "" {
		return loan, false, fmt.Errorf("%w: the customer is no longer eligible to accept this offer", domain.ErrConflict)
	}
	// The deposit account is derived from the authenticated customer id. It
	// must agree with identity's record; a mismatch is a data fault, not
	// something to proceed past.
	if customer.DepositAccountCode != contract.CustomerDepositCode(who.CustomerID.String()) {
		return loan, false, fmt.Errorf("deposit account code mismatch for customer %s", who.CustomerID)
	}

	var payout *domain.Payout
	var enquiryRef string
	if req.Destination.Type == DestinationExternal {
		if s.payouts == nil {
			return loan, false, fmt.Errorf("%w: external payout is not available", domain.ErrValidation)
		}
		resolved, err := s.resolveOwnAccount(ctx, customer, req.Destination)
		if err != nil {
			return loan, false, err
		}
		id := uuid.New()
		enquiryRef = resolved.EnquiryRef
		payout = &domain.Payout{
			ID: id, CustomerID: who.CustomerID, AmountMinor: offer.NetDisbursementMinor,
			BankCode: req.Destination.BankCode, AccountNumber: req.Destination.AccountNumber, AccountName: resolved.AccountName,
			// The reference is opaque and carries no personal data; it is
			// what the provider, callbacks and reports are matched on.
			Reference: "lp-" + id.String(), State: domain.PayoutPending, CreatedAt: now,
		}
	}

	today := s.today()
	schedule, err := domain.BuildSchedule(offer.PrincipalMinor, offer.MonthlyRateBps, offer.TenorMonths, today)
	if err != nil {
		return loan, false, err
	}
	loan = domain.Loan{
		ID: uuid.New(), ApplicationID: offer.ApplicationID, OfferID: offer.ID, CustomerID: who.CustomerID,
		ProductID: s.product.ID, ProductVersion: s.product.Version,
		PrincipalMinor: offer.PrincipalMinor, MonthlyRateBps: offer.MonthlyRateBps, TenorMonths: offer.TenorMonths,
		OriginationFeeMinor: offer.OriginationFeeMinor, State: domain.LoanPendingDisbursement, ScheduleVersion: 1,
		AutoDebitAuthorised: req.AutoDebitAuthorised, DepositAccountCode: customer.DepositAccountCode, AcceptedOn: today,
	}
	intent := domain.PostingIntent{
		ID: uuid.New(), Kind: domain.IntentDisbursement, LoanID: &loan.ID,
		JournalType: contract.JournalLoanDisbursement,
		Lines:       disbursementLines(offer, customer.DepositAccountCode),
	}

	var existing uuid.UUID
	err = s.store.InTx(ctx, func(q *postgres.Queries) error {
		id, claimed, err := q.ClaimIdempotencyKey(ctx, who.CustomerID, endpointAccept, idemKey, hash, loan.ID, now.Add(s.cfg.IdempotencyRetention))
		if err != nil {
			return err
		}
		if !claimed {
			existing = id
			return nil
		}
		// Application first, then offer: the same order the expiry job uses,
		// so the two cannot deadlock.
		ok, err := q.TransitionApplication(ctx, offer.ApplicationID, []domain.ApplicationState{domain.AppOffered},
			postgres.ApplicationUpdate{To: domain.AppAccepted}, now)
		if err != nil {
			return err
		}
		if !ok {
			return domain.ErrOfferNotOpen
		}
		accepted, err := q.AcceptOffer(ctx, offer.ID, now, map[string]any{
			"token_id": who.TokenID, "step_up": "PIN_VERIFIED", "disclosure_hash": offer.DisclosureHash,
			"auto_debit_authorised": req.AutoDebitAuthorised, "destination_type": req.Destination.Type,
			"consent_policy_version": s.cfg.ConsentPolicyVersion,
		})
		if err != nil {
			return err
		}
		if !accepted {
			// The offer expired or was accepted between the early check and
			// now. Returning an error rolls the application move back too.
			return domain.ErrOfferExpired
		}
		if err := q.InsertLoan(ctx, loan); err != nil {
			return err
		}
		if err := q.InsertInstalments(ctx, loan.ID, 1, schedule); err != nil {
			return err
		}
		if err := q.InsertIntent(ctx, intent, now.Add(s.cfg.InlineGrace)); err != nil {
			return err
		}
		if payout != nil {
			payout.LoanID = loan.ID
			if err := q.InsertPayout(ctx, *payout, s.payouts.Name(), enquiryRef); err != nil {
				return err
			}
		}
		if err := q.Audit(ctx, postgres.Actor{Kind: "customer", ID: who.CustomerID.String()}, "OFFER_ACCEPTED", "loan_offer", offer.ID.String(),
			map[string]any{"loan_id": loan.ID.String(), "disclosure_hash": offer.DisclosureHash, "token_id": who.TokenID,
				"step_up": "PIN_VERIFIED", "auto_debit_authorised": req.AutoDebitAuthorised, "destination_type": req.Destination.Type}); err != nil {
			return err
		}
		return s.emitLoan(ctx, q, "loan.offer.accepted", loan, loanEvent{PrincipalMinor: loan.PrincipalMinor})
	})
	if err != nil {
		return domain.Loan{}, false, err
	}
	if existing != uuid.Nil {
		s.metrics.IdempotentReplay(endpointAccept)
		loan, err = s.store.GetLoanForCustomer(ctx, existing, who.CustomerID)
		return loan, true, err
	}

	// Disburse now, in the request, so the customer normally sees the money
	// immediately. If this fails or the process dies here, the intent is
	// already durable and the sweeper completes it.
	if err := s.ProcessIntent(ctx, intent.ID); err != nil {
		s.log.WarnContext(ctx, "inline disbursement did not complete; the sweeper will retry", "loan_id", loan.ID.String(), "error", err.Error())
	}
	loan, err = s.store.GetLoan(ctx, loan.ID)
	return loan, false, err
}

// disbursementLines is the accounting of a disbursement, from the bank's
// perspective (plan section 6.11, example 4):
//
//	Dr Loans receivable - principal   principal
//	Cr Customer deposit               principal - fee
//	Cr Fee income                     fee
func disbursementLines(o domain.Offer, depositAccount string) []domain.JournalLine {
	lines := []domain.JournalLine{
		{AccountCode: contract.AccountLoansPrincipal, Direction: "DEBIT", AmountMinor: o.PrincipalMinor},
		{AccountCode: depositAccount, Direction: "CREDIT", AmountMinor: o.NetDisbursementMinor},
	}
	if o.OriginationFeeMinor > 0 {
		lines = append(lines, domain.JournalLine{AccountCode: contract.AccountFeeIncome, Direction: "CREDIT", AmountMinor: o.OriginationFeeMinor})
	}
	return lines
}

// GetOffer returns the customer's own offer.
func (s *Service) GetOffer(ctx context.Context, customerID, offerID uuid.UUID) (domain.Offer, error) {
	return s.store.GetOfferForCustomer(ctx, offerID, customerID)
}
