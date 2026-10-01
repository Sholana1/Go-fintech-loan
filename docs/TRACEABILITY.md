# Traceability: personal loans

Plan requirement → implementation → tests → operational evidence → status.

**Status values.** *Done*: implemented and tested locally. *Simulated*: done against a simulator; the real integration is unverified. *Partial*: some of it is built; the gap is named. *Not built*: recorded as a launch dependency.

Test names refer to `services/<service>/*_test.go` unless a path is given.

## Financial foundation

| Plan requirement | Implementation | Tests | Operational evidence | Status |
|---|---|---|---|---|
| Journals balance per currency (6.4) | Deferred constraint trigger; `domain.ValidateLines` | `TestDatabaseRejectsUnbalancedJournalAtCommit`, `TestValidateLines` | `ledger_invariant_violations{kind="unbalanced_journal"}` | Done |
| Posted entries immutable; corrections are linked entries (6.10) | Triggers; no UPDATE/DELETE grant; `reverses_journal_id` | `TestPostedRowsAreImmutableAndBalancesAreNotWritable` | — | Done. No reversal use case exists yet; none is needed for loans |
| An operation cannot be posted twice (6.6) | `posting_refs` primary key, claimed first | `TestSameReferencePostsExactlyOnce`, `TestReferenceReusedWithDifferentContentIsRejected` | `ledger_journals_posted_total{duplicate="true"}` | Done |
| Concurrent spending cannot exceed funds (6.7) | Row lock + check + CHECK constraint | `TestConcurrentDebitsNeverOverspend`, `TestRandomOperationsPreserveInvariants` | `ledger_postings_rejected_total{reason="INSUFFICIENT_FUNDS"}` | Done |
| Deterministic lock order, no deadlocks (6.6) | `lockAccounts` | `TestOpposingPostingsDoNotDeadlock` | `ledger_tx_retries_total` | Done |
| Holds, capture, release (6.10) | `PlaceHold`, `CaptureHold`, `ReleaseHold` | `TestHoldLifecycle`, `TestCaptureAndReleaseRace` | `held_drift` | Done. Hold expiry job not built (no expiring holds yet) |
| Balances rebuildable from entries (6.13) | Trigger-maintained projection; `Verify` | `AssertInvariants` in every integration test | `ledgerd verify`; verifier gauge every minute | Done |
| Cached balances cannot authorise (6.9) | No such path exists | by construction | — | Done |
| Callers limited to their own postings (4) | mTLS identity, `posting_rights`, `journal_types` | `TestAuthorisationIsByWorkloadAndJournalShape`, `platform/grpcx` tests | gRPC access log with caller | Done |
| Transactional outbox (12) | `platform/outbox`; row in the posting transaction | `TestOutboxRowIsWrittenAtomicallyWithTheJournal`, `platform/outbox` tests | `*_outbox_oldest_unpublished_seconds` | Done |
| Consumer deduplication (12) | `lending.inbox` committed with the effect | `TestLedgerEventIsAppliedOncePerEventID`, `platform/kafkax` tests | `loan_events_consumed_total{outcome="duplicate"}` | Done |
| Monthly partitions (11) | `ensure_month_partitions`; default partition as safety net | exercised by every ledger test | `default_partition_rows` | Done. Archival not built |
| System-account striping (6.8) | Not implemented | — | measured: ADR 0010 | Deferred with evidence |
| Limits by KYC tier inside the ledger (2) | Not implemented | — | — | **Not built** |
| Identity, KYC status (7.1) | `services/identity` | `TestRegistration*`, `TestConcurrentDuplicateRegistrationCreatesOneCustomer` | identity audit log | Simulated (BVN). Liveness, device binding, OTP not built |
| Authentication and lockout (14) | Ed25519 tokens; lockout under row lock | `TestLoginLockoutCannotBeOutrunByParallelGuessing`, `platform/authn` tests | `CREDENTIAL_LOCKED` audit | Partial: no MFA, no device binding |
| Workload identity, mTLS (14) | `platform/grpcx` | `TestClientsWithoutATrustedCertificateAreRefused`, `TestCallerIdentityComesFromTheClientCertificate` | — | Done locally; production CA not chosen |

## Loan lifecycle

| Plan requirement | Implementation | Tests | Operational evidence | Status |
|---|---|---|---|---|
| Application with consent; minimal data (8.2) | `SubmitApplication` | `TestSubmissionValidation` | audit `APPLICATION_SUBMITTED` with consent ref | Done |
| Duplicate applications (8.6) | Partial unique index; idempotency key | `TestConcurrentDuplicateSubmissionsCreateOneApplication`, `TestSubmissionIdempotency` | `loan_idempotent_replays_total` | Done |
| KYC and eligibility from the system of record (8.3) | `Assess` → identity gRPC; `domain.Ineligible` | `TestAssessmentOutcomes`, `TestIneligibleApplicationsDoNotTriggerABureauEnquiry` | decision snapshot | Done |
| Fraud screening (8.3) | `fraudrules` velocity rules | `TestApplicationVelocityIsScreened` | reason codes | Partial: velocity only; device and link analysis belong to the risk service |
| Credit bureau; raw response kept (8.2) | `CreditBureau` port; `bureau_reports` | `TestStoredDecisionIsReproducible`, `TestAssessmentRetriesReuseTheBureauReference` | `loan_assessment_stage_seconds{stage="bureau"}` | Simulated |
| Degraded behaviour when the bureau is down (8.12) | Application waits, then expires | `TestBureauOutageDefersTheDecisionAndRecovers`, `TestApplicationExpiresIfBureauNeverReturns` | alert `LoanBureauFailing` | Done. Secondary bureau not built |
| Affordability, exposure, versioned policy (8.3, 8.4) | `domain.Evaluate`, `SizeLoan`; JSON policy | `TestPolicy*`, `TestAffordabilityReducesToTheLargestAmountThatFits`, `TestExposureLimitReducesOrDeclines` | snapshot `policy_version` | Done |
| Reproducible decision record (8.4) | `decision_snapshots`, append-only | `TestStoredDecisionIsReproducible`, `TestDecisionIsReproducibleFromItsStoredFeatures` | — | Done |
| Manual review bound by policy (8.4) | `DecideManualReview` | `TestManualReview` | audit with staff id | Done |
| Customer right to request human review (8.4, NDPA) | — | — | — | **Not built** |
| Model serving boundary (8.4) | — | — | — | Not built; rules only, as the plan permits |
| Offer: terms, schedule, rounding, expiry (8.5) | `domain.PriceOffer`, `BuildSchedule`, `Disclosure` | `TestScheduleMatchesThePlansWorkedExample`, `TestInterestRoundsHalfToEven`, `TestSchedulePropertiesHoldForRandomTerms`, `TestCostOfCredit` | disclosure stored with its hash | Done |
| Acceptance evidence; expired or superseded offers (8.5) | Compare-and-set acceptance; hash and PIN | `TestAcceptanceRequiresMatchingDisclosureAndStepUp`, `TestExpiredOrSupersededOffersCannotBeAccepted`, `TestConcurrentAcceptanceCreatesOneLoanAndOneDisbursement` | `offers.acceptance` | Done |
| Disbursement: durable, idempotent (7.2, 8.6) | Posting intent | `TestDisbursementSurvivesLedgerOutageAtAcceptance`, `TestDisbursementIsNotRepeatedAfterALostLedgerResponse`, `TestDuplicateDisbursementCommandsPostOnce`, `TestBacklogOfPendingDisbursementsDrainsAfterAnOutage` | `loan_disbursement_oldest_pending_seconds`, `loan_duplicate_postings_prevented_total` | Done |
| "Disbursed" only when posted (9) | `applyDisbursement`; CHECK on `loans` | same tests | — | Done |
| Destination verification (8) | Derived account; name enquiry | `TestExternalDestinationMustBeTheCustomersOwnAccount`, `TestNamesMatch` | — | Simulated |
| External timeout is unknown, not failure (7.5) | Payout state machine | `TestPayoutTimeoutIsUnknownNotFailed`, `TestPayoutSenderCrashIsResolvedByQueryNotResend`, `TestPayoutNotFoundStaysUnknownUntilTheSettlementReport`, `TestAbsenceFromReportDoesNotReleaseByDefault` | `loan_payout_unresolved`, `…_oldest_unresolved_seconds` | Simulated |
| Callbacks: signed, replay-protected, deduplicated (13) | `simsig.Authenticate`, `HandlePayoutCallback` | `TestPayoutCallbacks`, `TestPayoutCallbackWithoutAnOutcomeIsResolvedByQuery` | `loan_payout_callbacks_total` | Simulated |
| Production payout adapter from a verified contract (9) | `providers/paystack` | `paystack/*_test.go` (local server returning the specified shapes) | — | Written from the spec; never run against Paystack |
| Repayment from outside the bank, verified with the provider (7.5 pattern) | `InitiateExternalRepayment`, `DriveExternalPayment`, `HandlePaymentNotice` | `TestExternalRepaymentIsCreditedOnlyOnProviderConfirmation`, `TestDuplicateAndConcurrentPaymentNotificationsCreditOnce`, `TestForgedPaymentWebhooksAreRejected`, `TestProviderFailuresDuringVerificationAreUnknownNotFailed`, `TestFailedAndExpiredPaymentsAreStillCreditedIfTheMoneyArrivesLater`, `TestWhatIsCreditedIsWhatTheProviderCollected`, `TestExternalPaymentCreditSurvivesLedgerFailures`, `TestExternalPaymentForASettledLoanStaysInTheCustomersAccount` | `loan_external_payment_state_changes_total`, `…_oldest_confirmed_wait_seconds` | Simulated; provider settlement postings not built |
| Rate limiting; Redis never authoritative (12) | `platform/ratelimit` | `TestRedis*`, `TestGuard*`, `TestSignInIsRateLimitedPerPhone`, `TestSignInWorksAndLockoutHoldsWhenRedisIsDown`, `TestApplicationSubmissionIsRateLimitedPerCustomer`, `TestSubmissionRulesHoldWhenRedisIsDown` | `ratelimit_decisions_total` | Done for sign-in and submission; per-IP limits need the gateway |
| Identity verification over HTTP, fail closed (2) | `bvnsim.Client` | `TestBVNAdapter`, `TestRegistrationWaitsNoLongerThanTheProviderTimeout`, `TestRegistrationRejections` | — | Simulated |
| Late success after timeout (7.5) | `resolveLateSuccess` | `TestLateSuccessAfterRecordedFailureIsDebited`, `TestLateSuccessWithoutFundsStaysAnOpenException` | exception `PAYOUT_LATE_SUCCESS` | Simulated |
| Schedules, partial and full repayment, allocation (8.7, 8.9) | `domain.Allocate`, `Repay` | `TestAllocate*`, `TestPartialRepaymentPaysInterestThenPrincipal`, `TestRandomRepaymentSequencesPreserveInvariants` | audit `REPAYMENT_ALLOCATED` | Done |
| Overpayment, early repayment (8.7) | Payoff settlement | `TestOverpaymentIsNotTaken`, `TestEarlySettlementWaivesUnearnedInterestAndCloses`, `TestSettlementBeforeTheAccrualJobIsCaughtUp` | — | Done. Partial prepayment not offered |
| Authorised collection (8.8) | `CollectDue`, own-account debit | `TestScheduledCollection`, `TestCollectionTakesPartialAmountsAndIsBounded`, `TestFundsArrivingTriggerCollectionThroughKafka` | repayments with `source=AUTO_DEBIT` | Partial: no external mandates |
| Daily interest accrual; duplicate and missed runs (7.7 pattern) | `AccrueInterest` | `TestDailyAccrualSumsExactlyForEveryMonthLength`, `TestInterestAccrualIsIdempotentAndSafeToRunConcurrently`, `TestMissedAccrualDaysAreCaughtUpInOrder` | `accrual_runs` | Done |
| Arrears, late fee (8.7) | `ClassifyArrears` | `TestArrearsLateFeeAndClassification` | `loan.arrears.changed` | Done |
| Restructure, write-off, recovery; maker-checker (8.7, 14) | `admin_actions`; `domain.Restructure` | `TestWriteOffRequiresMakerCheckerAndRecoveriesAreTracked`, `TestRestructureCreatesANewScheduleVersion` | audit trail | Done. Provisioning model not built |
| Statements, operational reporting (8.9) | `GetStatement`, `PortfolioReport` | `TestLoanLifecycleFromApplicationToClosure` | `/v1/ops/portfolio` | Done. Regulatory returns and bureau submission not built |
| Reconciliation (6.12) | `ReconcileLedger`, `ReconcilePayouts` | `TestReconciliationDetectsDrift`, `TestSettlementReportDisagreementsRaiseExceptions`, `reconcileClean` in lifecycle tests | `loan_recon_exceptions_open` | Done for loans; bank-statement and GL layers not built |
| Resource authorisation (14) | Owner-scoped queries; role checks | `TestCustomersCannotReachEachOthersLoans` | access log | Done |
| Five-minute target, measured honestly (1.A, 8.11) | T0–T3 timestamps; `loan_system_time_seconds` | `cmd/loanbench` | dashboard panel 1; alert `LoanSystemTimeSLO` | Instrumented. Not validated: providers are simulated |

## Tests the brief names explicitly

| Named scenario | Test |
|---|---|
| Concurrent duplicate loan submissions | `TestConcurrentDuplicateSubmissionsCreateOneApplication` |
| Concurrent offer acceptance | `TestConcurrentAcceptanceCreatesOneLoanAndOneDisbursement` |
| Duplicate disbursement commands | `TestDuplicateDisbursementCommandsPostOnce` |
| Same idempotency key, different content | `TestSubmissionIdempotency`, `TestAcceptanceReplayReturnsTheSameLoan`, `TestReferenceReusedWithDifferentContentIsRejected` |
| Database commit followed by process crash | `TestDisbursementSurvivesLedgerOutageAtAcceptance`, `TestDisbursementIsNotRepeatedAfterALostLedgerResponse`, `TestRepaymentIsAppliedOnceAfterALostLedgerResponse` |
| Consumer commit followed by crash before acknowledgement | `TestLedgerEventIsAppliedOncePerEventID`, `TestRecordIsRedeliveredWhenOffsetWasNotCommitted` |
| Provider success with a lost response | `TestPayoutTimeoutIsUnknownNotFailed`, `TestPayoutSenderCrashIsResolvedByQueryNotResend` |
| Late success after a timeout | `TestLateSuccessAfterRecordedFailureIsDebited`, `TestLateSuccessWithoutFundsStaysAnOpenException` |
| Duplicate and out-of-order callbacks | `TestPayoutCallbacks`, `TestLateSuccessAfterRecordedFailureIsDebited` |
| Partial repayment | `TestPartialRepaymentPaysInterestThenPrincipal` |
| Overpayment | `TestOverpaymentIsNotTaken` |
| Rounding boundaries | `TestInterestRoundsHalfToEven`, `TestRoundHalfEven`, `TestDailyAccrualSumsExactlyForEveryMonthLength` |
| Database failover or interrupted transactions | `TestInterruptedTransactionsLeaveNoPartialPostings`, `TestCrashAfterPublishRepublishesWithTheSameEventID` (backends terminated mid-transaction; a real replica failover has not been exercised) |
| Unauthorised access to another customer's loan | `TestCustomersCannotReachEachOthersLoans` |
