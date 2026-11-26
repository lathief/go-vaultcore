package transfer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
)

// Request is the validated input for a transfer.
type Request struct {
	SourceAccountID      int64 `json:"source_account_id"`
	DestinationAccountID int64 `json:"destination_account_id"`
	AmountCents          int64 `json:"amount_cents"`
}

// Result is the response for a transfer. Replays return the original transfer
// ID and the accounts' current balances.
type Result struct {
	TransferID         int64 `json:"transfer_id"`
	SourceBalanceCents int64 `json:"source_balance_cents"`
	TargetBalanceCents int64 `json:"target_balance_cents"`
}

type Service struct {
	DB *sql.DB
}

type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

type ConflictError struct{ Msg string }

func (e *ConflictError) Error() string { return e.Msg }

// KeyReusedError: the same Idempotency-Key was used with a different payload.
// Never replayable — the client must send a new key.
type KeyReusedError struct{}

func (e *KeyReusedError) Error() string { return "Idempotency-Key reused with a different payload" }

type AccountError struct{ Msg string }

func (e *AccountError) Error() string { return e.Msg }

type InsufficientFundsError struct{}

func (e *InsufficientFundsError) Error() string { return "insufficient funds" }

func requestHash(r Request) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%d:%d:%d", r.SourceAccountID, r.DestinationAccountID, r.AmountCents)))
	return hex.EncodeToString(h[:])
}

// lockOrder returns the two account ids in ascending order. Locking in a
// global, deterministic order makes the classic A<->B deadlock impossible:
// every concurrent transfer acquires the lower id first.
func lockOrder(a, b int64) (int64, int64) {
	if a <= b {
		return a, b
	}
	return b, a
}

type accountState struct {
	ID       int64
	Currency string
	Status   string
	Balance  int64
}

// Execute validates a transfer, claims its idempotency key, and runs it in a
// database transaction. idemKey comes from the Idempotency-Key header.
func (s *Service) Execute(ctx context.Context, idemKey string, req Request) (Result, bool, error) {
	if idemKey == "" {
		return Result{}, false, &ValidationError{Msg: "Idempotency-Key header is required"}
	}
	if req.SourceAccountID == req.DestinationAccountID {
		return Result{}, false, &ValidationError{Msg: "source and destination accounts must differ"}
	}
	if req.AmountCents <= 0 {
		return Result{}, false, &ValidationError{Msg: "amount_cents must be positive"}
	}

	if mismatch, err := s.payloadMismatch(ctx, idemKey, req); err != nil {
		return Result{}, false, err
	} else if mismatch {
		return Result{}, false, &KeyReusedError{}
	}

	// Replay: if this key already completed, return the stored outcome without
	// touching account rows, so a retried request never debits twice.
	if res, ok, err := s.ReplayResult(ctx, idemKey); err != nil {
		return Result{}, false, err
	} else if ok {
		return res, true, nil
	}

	claimToken, err := s.claimKey(ctx, idemKey, req)
	if err != nil {
		if errors.Is(err, errCompleted) {
			res, ok, replayErr := s.ReplayResult(ctx, idemKey)
			if replayErr != nil {
				return Result{}, false, replayErr
			}
			if ok {
				return res, true, nil
			}
			return Result{}, false, &ConflictError{Msg: "transfer with this Idempotency-Key already completed"}
		}
		return Result{}, false, err
	}

	res, err := s.runTransfer(ctx, idemKey, claimToken, req)
	if err != nil {
		if releaseErr := s.failKey(context.WithoutCancel(ctx), idemKey, claimToken); releaseErr != nil {
			return Result{}, false, fmt.Errorf("transfer failed (%v); release idempotency claim: %w", err, releaseErr)
		}
		return Result{}, false, err
	}
	return res, false, nil
}

// payloadMismatch reports whether the idempotency key was already used with a
// different request payload.
func (s *Service) payloadMismatch(ctx context.Context, key string, req Request) (bool, error) {
	var hash string
	err := s.DB.QueryRowContext(ctx,
		`SELECT request_hash FROM idempotency_keys WHERE scope='transfer' AND idem_key=?`,
		key).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return hash != requestHash(req), nil
}

func (s *Service) claimKey(ctx context.Context, key string, req Request) (string, error) {
	token, err := newClaimToken()
	if err != nil {
		return "", err
	}
	_, insertErr := s.DB.ExecContext(ctx, `
		INSERT INTO idempotency_keys (idem_key, scope, request_hash, state, expires_at, claim_token)
		VALUES (?, 'transfer', ?, 'processing', DATE_ADD(NOW(6), INTERVAL 24 HOUR), ?)`,
		key, requestHash(req), token)
	if insertErr == nil {
		return token, nil
	}
	var (
		state string
		hash  string
	)
	qerr := s.DB.QueryRowContext(ctx,
		`SELECT state, request_hash FROM idempotency_keys WHERE scope='transfer' AND idem_key=?`,
		key).Scan(&state, &hash)
	if qerr != nil {
		if errors.Is(qerr, sql.ErrNoRows) {
			return "", insertErr
		}
		return "", qerr
	}
	if hash != requestHash(req) {
		return "", &KeyReusedError{}
	}
	if state == "completed" {
		return "", errCompleted
	}
	res, err := s.DB.ExecContext(ctx, `
		UPDATE idempotency_keys
		SET state='processing', result_code=NULL, transfer_id=NULL, claim_token=?,
		    expires_at=DATE_ADD(NOW(6), INTERVAL 24 HOUR)
		WHERE scope='transfer' AND idem_key=? AND request_hash=?
		  AND (state='failed' OR (state='processing' AND expires_at <= NOW(6)))`,
		token, key, requestHash(req))
	if err != nil {
		return "", err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", err
	}
	if n == 1 {
		return token, nil
	}

	err = s.DB.QueryRowContext(ctx,
		`SELECT state, request_hash FROM idempotency_keys WHERE scope='transfer' AND idem_key=?`,
		key).Scan(&state, &hash)
	if err != nil {
		return "", err
	}
	if hash != requestHash(req) {
		return "", &KeyReusedError{}
	}
	if state == "completed" {
		return "", errCompleted
	}
	return "", &ConflictError{Msg: "request with this Idempotency-Key is already in progress"}
}

var errCompleted = errors.New("idempotency key already completed")

func newClaimToken() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", fmt.Errorf("generate idempotency claim token: %w", err)
	}
	return hex.EncodeToString(token[:]), nil
}

func (s *Service) failKey(ctx context.Context, key, claimToken string) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE idempotency_keys SET state='failed', result_code='error', claim_token=NULL
		 WHERE scope='transfer' AND idem_key=? AND state='processing' AND claim_token=?`,
		key, claimToken)
	return err
}

// runTransfer does the money movement inside one transaction. Locking both
// rows in ascending id order prevents the A<->B deadlock; the single commit
// makes the debit, credit and idempotency completion atomic.
func (s *Service) runTransfer(ctx context.Context, idemKey, claimToken string, req Request) (Result, error) {
	var zero Result

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback() //nolint:errcheck

	lo, hi := lockOrder(req.SourceAccountID, req.DestinationAccountID)
	rows, err := tx.QueryContext(ctx, `
		SELECT id, currency, status, balance_cents
		FROM accounts
		WHERE id IN (?, ?)
		ORDER BY id
		FOR UPDATE`, lo, hi)
	if err != nil {
		return zero, err
	}
	balances := make(map[int64]accountState, 2)
	for rows.Next() {
		var a accountState
		if err := rows.Scan(&a.ID, &a.Currency, &a.Status, &a.Balance); err != nil {
			rows.Close()
			return zero, err
		}
		balances[a.ID] = a
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return zero, err
	}
	src, okSrc := balances[req.SourceAccountID]
	dst, okDst := balances[req.DestinationAccountID]
	if !okSrc || !okDst {
		return zero, &AccountError{Msg: "account not found"}
	}
	if src.Status != "active" || dst.Status != "active" {
		return zero, &AccountError{Msg: "account is not active"}
	}
	if src.Currency != dst.Currency {
		return zero, &AccountError{Msg: "currency mismatch between accounts"}
	}
	if src.Balance < req.AmountCents {
		return zero, &InsufficientFundsError{}
	}
	dstNew, err := checkedBalanceAdd(dst.Balance, req.AmountCents)
	if err != nil {
		return zero, err
	}

	lr, err := tx.ExecContext(ctx,
		`INSERT INTO transfers (amount_cents, currency) VALUES (?, ?)`,
		req.AmountCents, src.Currency)
	if err != nil {
		return zero, err
	}
	transferID, err := lr.LastInsertId()
	if err != nil {
		return zero, err
	}

	srcNew := src.Balance - req.AmountCents

	if _, err := tx.ExecContext(ctx,
		`UPDATE accounts SET balance_cents=?, version=version+1 WHERE id=?`, srcNew, src.ID); err != nil {
		return zero, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE accounts SET balance_cents=?, version=version+1 WHERE id=?`, dstNew, dst.ID); err != nil {
		return zero, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO ledger_entries (transfer_id, account_id, direction, amount_cents, balance_after)
		VALUES (?, ?, 'debit', ?, ?), (?, ?, 'credit', ?, ?)`,
		transferID, src.ID, req.AmountCents, srcNew,
		transferID, dst.ID, req.AmountCents, dstNew); err != nil {
		return zero, err
	}

	// Finalize the idempotency record inside the same transaction, so the
	// completion and the money movement commit (or roll back) atomically.
	finalizeResult, err := tx.ExecContext(ctx, `
		UPDATE idempotency_keys
		SET state='completed', result_code='ok', transfer_id=?, claim_token=NULL
		WHERE scope='transfer' AND idem_key=? AND state='processing' AND claim_token=?`,
		transferID, idemKey, claimToken)
	if err != nil {
		return zero, err
	}
	updated, err := finalizeResult.RowsAffected()
	if err != nil {
		return zero, err
	}
	if updated != 1 {
		return zero, &ConflictError{Msg: "idempotency claim expired before transfer completed"}
	}

	if err := tx.Commit(); err != nil {
		return zero, err
	}
	return Result{TransferID: transferID, SourceBalanceCents: srcNew, TargetBalanceCents: dstNew}, nil
}

// ReplayResult returns the stored outcome of a completed transfer for a given
// idempotency key. Balances are re-read live so a replay after further
// transfers reflects current values while never re-executing the debit.
func (s *Service) ReplayResult(ctx context.Context, key string) (Result, bool, error) {
	var (
		transferID    int64
		sourceBalance int64
		targetBalance int64
	)
	err := s.DB.QueryRowContext(ctx, `
		SELECT ik.transfer_id, src.balance_cents, dst.balance_cents
		FROM idempotency_keys ik
		JOIN transfers t      ON t.id = ik.transfer_id
		JOIN ledger_entries d ON d.transfer_id = t.id AND d.direction='debit'
		JOIN ledger_entries c ON c.transfer_id = t.id AND c.direction='credit'
		JOIN accounts src     ON src.id = d.account_id
		JOIN accounts dst     ON dst.id = c.account_id
		WHERE ik.scope='transfer' AND ik.idem_key=? AND ik.state='completed'`,
		key).Scan(&transferID, &sourceBalance, &targetBalance)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, err
	}
	return Result{TransferID: transferID, SourceBalanceCents: sourceBalance, TargetBalanceCents: targetBalance}, true, nil
}

func checkedBalanceAdd(balance, amount int64) (int64, error) {
	if amount > 0 && balance > math.MaxInt64-amount {
		return 0, &ValidationError{Msg: "destination balance would overflow"}
	}
	if amount < 0 && balance < math.MinInt64-amount {
		return 0, &ValidationError{Msg: "destination balance would overflow"}
	}
	return balance + amount, nil
}

// CreateAccount seeds an account (tests and fixtures).
func (s *Service) CreateAccount(ctx context.Context, ownerID int64, currency string, startCents int64) (int64, error) {
	res, err := s.DB.ExecContext(ctx,
		`INSERT INTO accounts (owner_id, currency, balance_cents) VALUES (?, ?, ?)`,
		ownerID, currency, startCents)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}
