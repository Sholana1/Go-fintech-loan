package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"bankplatform.internal/platform/pgxutil"
	"bankplatform.internal/services/lending/domain"
)

const applicationColumns = `application_id, customer_id, product_id, product_version, requested_principal_minor,
	tenor_months, stated_monthly_income_minor, state, state_reason, waiting_on, consent_ref,
	submitted_at, decided_at, expires_at, attempts, version`

func scanApplication(row pgx.Row) (domain.Application, error) {
	var (
		a     domain.Application
		state string
	)
	err := row.Scan(&a.ID, &a.CustomerID, &a.ProductID, &a.ProductVersion, &a.RequestedPrincipalMinor,
		&a.TenorMonths, &a.StatedMonthlyIncomeMinor, &state, &a.StateReason, &a.WaitingOn, &a.ConsentRef,
		&a.SubmittedAt, &a.DecidedAt, &a.ExpiresAt, &a.Attempts, &a.Version)
	a.State = domain.ApplicationState(state)
	return a, err
}

// InsertApplication stores a new application. If the customer already has an
// open application for the product, the partial unique index rejects the
// insert and ErrApplicationOpen is returned: under concurrent submissions
// exactly one succeeds.
func (q *Queries) InsertApplication(ctx context.Context, a domain.Application, consentPolicyVersion string, nextAttemptAt time.Time) error {
	_, err := q.db.Exec(ctx, `
		INSERT INTO lending.applications (application_id, customer_id, product_id, product_version,
			requested_principal_minor, tenor_months, stated_monthly_income_minor, state,
			consent_ref, consent_policy_version, consented_at, submitted_at, expires_at, next_attempt_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$11,$12,$13)`,
		a.ID, a.CustomerID, a.ProductID, a.ProductVersion, a.RequestedPrincipalMinor, a.TenorMonths,
		a.StatedMonthlyIncomeMinor, string(a.State), a.ConsentRef, consentPolicyVersion, a.SubmittedAt, a.ExpiresAt, nextAttemptAt)
	if pgxutil.IsUniqueViolation(err, "one_open_application") {
		return domain.ErrApplicationOpen
	}
	return err
}

func (q *Queries) GetApplication(ctx context.Context, id uuid.UUID) (domain.Application, error) {
	a, err := scanApplication(q.db.QueryRow(ctx, `SELECT `+applicationColumns+` FROM lending.applications WHERE application_id = $1`, id))
	return a, notFound(err, "application")
}

// GetApplicationForCustomer returns the application only if it belongs to
// the customer. A foreign id is indistinguishable from a missing one.
func (q *Queries) GetApplicationForCustomer(ctx context.Context, id, customerID uuid.UUID) (domain.Application, error) {
	a, err := scanApplication(q.db.QueryRow(ctx, `
		SELECT `+applicationColumns+` FROM lending.applications WHERE application_id = $1 AND customer_id = $2`, id, customerID))
	return a, notFound(err, "application")
}

// OpenApplication returns the customer's open application for a product.
func (q *Queries) OpenApplication(ctx context.Context, customerID uuid.UUID, productID string) (domain.Application, error) {
	a, err := scanApplication(q.db.QueryRow(ctx, `
		SELECT `+applicationColumns+` FROM lending.applications
		 WHERE customer_id = $1 AND product_id = $2 AND state IN ('SUBMITTED','ASSESSING','REFERRED','OFFERED','ACCEPTED')`, customerID, productID))
	return a, notFound(err, "open application")
}

// ClaimApplication takes the assessment lease on one application: it moves
// it to ASSESSING and pushes next_attempt_at past the lease. It returns
// false if the application is not due (another worker holds the lease) or is
// no longer assessable.
func (q *Queries) ClaimApplication(ctx context.Context, id uuid.UUID, now time.Time, lease time.Duration) (domain.Application, bool, error) {
	a, err := scanApplication(q.db.QueryRow(ctx, `
		UPDATE lending.applications
		   SET state = 'ASSESSING', attempts = attempts + 1, next_attempt_at = $3, version = version + 1, updated_at = $2
		 WHERE application_id = $1 AND state IN ('SUBMITTED','ASSESSING') AND next_attempt_at <= $2
		RETURNING `+applicationColumns, id, now, now.Add(lease)))
	if err == pgx.ErrNoRows {
		return domain.Application{}, false, nil
	}
	return a, err == nil, err
}

// DueApplications lists applications whose assessment is due, oldest first.
// It only reads ids; each is then claimed individually.
func (q *Queries) DueApplications(ctx context.Context, now time.Time, limit int) ([]uuid.UUID, error) {
	rows, err := q.db.Query(ctx, `
		SELECT application_id FROM lending.applications
		 WHERE next_attempt_at <= $1 AND state IN ('SUBMITTED','ASSESSING')
		 ORDER BY next_attempt_at LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// ApplicationUpdate describes a state change.
type ApplicationUpdate struct {
	To            domain.ApplicationState
	Reasons       []string
	WaitingOn     string
	DecidedAt     *time.Time // set once, at T1
	NextAttemptAt *time.Time // nil clears the work-queue entry
}

// TransitionApplication moves an application from one of the `from` states
// to u.To, compare-and-set. It returns false if the application was not in
// any of those states.
func (q *Queries) TransitionApplication(ctx context.Context, id uuid.UUID, from []domain.ApplicationState, u ApplicationUpdate, now time.Time) (bool, error) {
	states := make([]string, len(from))
	for i, s := range from {
		states[i] = string(s)
	}
	if u.Reasons == nil {
		u.Reasons = []string{}
	}
	tag, err := q.db.Exec(ctx, `
		UPDATE lending.applications
		   SET state = $3, state_reason = $4, waiting_on = $5,
		       decided_at = coalesce(decided_at, $6), next_attempt_at = $7,
		       version = version + 1, updated_at = $8
		 WHERE application_id = $1 AND state = ANY($2)`,
		id, states, string(u.To), u.Reasons, u.WaitingOn, u.DecidedAt, u.NextAttemptAt, now)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// CountApplicationsSince counts a customer's applications submitted since t;
// used by the velocity rule in fraud screening.
func (q *Queries) CountApplicationsSince(ctx context.Context, customerID uuid.UUID, since time.Time) (int, error) {
	var n int
	err := q.db.QueryRow(ctx, `SELECT count(*) FROM lending.applications WHERE customer_id = $1 AND submitted_at >= $2`, customerID, since).Scan(&n)
	return n, err
}

// ExpirableApplications lists applications past their validity that are
// still waiting on assessment or review.
func (q *Queries) ExpirableApplications(ctx context.Context, now time.Time, limit int) ([]uuid.UUID, error) {
	rows, err := q.db.Query(ctx, `
		SELECT application_id FROM lending.applications
		 WHERE expires_at <= $1 AND state IN ('SUBMITTED','ASSESSING','REFERRED')
		 ORDER BY expires_at LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// ReferredApplications is the manual-review queue, oldest first.
func (q *Queries) ReferredApplications(ctx context.Context, limit int) ([]domain.Application, error) {
	rows, err := q.db.Query(ctx, `
		SELECT `+applicationColumns+` FROM lending.applications
		 WHERE state = 'REFERRED' ORDER BY submitted_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Application
	for rows.Next() {
		a, err := scanApplication(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
