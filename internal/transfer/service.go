package transfer

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
)

// Request is the validated input for a transfer.
type Request struct {
	SourceAccountID      int64 `json:"source_account_id"`
	DestinationAccountID int64 `json:"destination_account_id"`
	AmountCents          int64 `json:"amount_cents"`
}

// Result is the outcome of a transfer, persisted alongside the idempotency key
// so replays return a consistent response.
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

// Execute runs the whole transfer in one database transaction.
// idemKey comes from the Idempotency-Key header.
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

	// Claim the key. The unique index uq_idem makes this race-safe: only one
	// concurrent request can insert; duplicates are a replay-in-progress or a
	// key reuse with a different payload.
	claimed, err := s.claimKey(ctx, idemKey, req)
	if err != nil {
		return Result{}, false, err
	}
	if !claimed {
		if errors.Is(err, errCompleted) {
			if res, ok, rerr := s.ReplayResult(ctx, idemKey); rerr == nil && ok {
				return res, true, nil
			}
			return Result{}, false, &ConflictError{Msg: "transfer with this Idempotency-Key already completed"}
		}
		return Result{}, false, &ConflictError{Msg: "request with this Idempotency-Key is already in progress or has a different payload"}
	}

	res, err := s.runTransfer(ctx, idemKey, req)
	if err != nil {
		// Release the claim so a genuine retry can start over. Run even if the
		// caller's request is cancelled mid-flight.
		s.failKey(context.WithoutCancel(ctx), idemKey)
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

func (s *Service) claimKey(ctx context.Context, key string, req Request) (bool, error) {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO idempotency_keys (idem_key, scope, request_hash, state, expires_at)
		VALUES (?, 'transfer', ?, 'processing', DATE_ADD(NOW(6), INTERVAL 24 HOUR))`,
		key, requestHash(req))
	if err == nil {
		return true, nil
	}
	var (
		state string
		hash  string
	)
	qerr := s.DB.QueryRowContext(ctx,
		`SELECT state, request_hash FROM idempotency_keys WHERE scope='transfer' AND idem_key=?`,
		key).Scan(&state, &hash)
	if qerr != nil {
		return false, &ConflictError{Msg: "idempotency key collision"}
	}
	if hash != requestHash(req) {
		return false, &KeyReusedError{}
	}
	if state == "completed" {
		// A completed replay slipped past the fast path (e.g. right after
		// commit); serve the stored result instead of a conflict.
		return false, errCompleted
	}
	// state == 'failed': a previous attempt released its claim, so this key is
	// reclaimable. The conditional update keeps the race with other claimers safe.
	res2, uerr := s.DB.ExecContext(ctx, `
		UPDATE idempotency_keys
		SET state='processing', result_code=NULL, transfer_id=NULL,
		    request_hash=?, expires_at=DATE_ADD(NOW(6), INTERVAL 24 HOUR)
		WHERE scope='transfer' AND idem_key=? AND state='failed'`,
		requestHash(req), key)
	if uerr == nil {
		if n, _ := res2.RowsAffected(); n == 1 {
			return true, nil
		}
	}
	return false, &ConflictError{Msg: "request with this Idempotency-Key is already in progress"}
}

var errCompleted = errors.New("idempotency key already completed")

func (s *Service) failKey(ctx context.Context, key string) {
	_, _ = s.DB.ExecContext(ctx,
		`UPDATE idempotency_keys SET state='failed', result_code='error'
		 WHERE scope='transfer' AND idem_key=? AND state='processing'`, key)
}

// runTransfer does the money movement inside one transaction. Locking both
// rows in ascending id order prevents the A<->B deadlock; the single commit
// makes the debit, credit and idempotency claim atomic.
func (s *Service) runTransfer(ctx context.Context, idemKey string, req Request) (Result, error) {
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
	dstNew := dst.Balance + req.AmountCents

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
	// claim and the money movement commit (or roll back) atomically.
	if _, err := tx.ExecContext(ctx, `
		UPDATE idempotency_keys
		SET state='completed', result_code='ok', transfer_id=?
		WHERE scope='transfer' AND idem_key=?`, transferID, idemKey); err != nil {
		return zero, err
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
