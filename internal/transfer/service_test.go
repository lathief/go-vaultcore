package transfer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"

	_ "github.com/go-sql-driver/mysql"
)

const testDSN = "root:root@tcp(127.0.0.1:3306)/app?parseTime=true&interpolateParams=true"

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("mysql", testDSN)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("mariadb not reachable: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, tbl := range []string{"idempotency_keys", "ledger_entries", "transfers", "accounts"} {
		if _, err := db.Exec("TRUNCATE TABLE " + tbl); err != nil {
			t.Fatalf("truncate %s: %v", tbl, err)
		}
	}
	return db
}

func mustAccount(t *testing.T, s *Service, owner int64, cents int64) int64 {
	t.Helper()
	id, err := s.CreateAccount(context.Background(), owner, "IDR", cents)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	return id
}

func balance(t *testing.T, s *Service, id int64) int64 {
	t.Helper()
	var b int64
	if err := s.DB.QueryRow("SELECT balance_cents FROM accounts WHERE id=?", id).Scan(&b); err != nil {
		t.Fatalf("balance: %v", err)
	}
	return b
}

func TestBasicTransfer(t *testing.T) {
	db := openDB(t)
	s := &Service{DB: db}
	ctx := context.Background()
	a := mustAccount(t, s, 1, 100_000)
	b := mustAccount(t, s, 2, 500)

	res, replayed, err := s.Execute(ctx, "key-basic-1", Request{SourceAccountID: a, DestinationAccountID: b, AmountCents: 25_000})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if replayed {
		t.Fatal("first execution reported as replay")
	}
	if res.SourceBalanceCents != 75_000 || res.TargetBalanceCents != 25_500 {
		t.Fatalf("unexpected balances: %+v", res)
	}
	if got := balance(t, s, a); got != 75_000 {
		t.Fatalf("source balance = %d", got)
	}
	if got := balance(t, s, b); got != 25_500 {
		t.Fatalf("target balance = %d", got)
	}

	// Replay with the same key must not debit twice.
	res2, replayed2, err := s.Execute(ctx, "key-basic-1", Request{SourceAccountID: a, DestinationAccountID: b, AmountCents: 25_000})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !replayed2 {
		t.Fatal("replay not flagged")
	}
	if res2.TransferID != res.TransferID {
		t.Fatalf("replay returned different transfer id: %d vs %d", res2.TransferID, res.TransferID)
	}
	if got := balance(t, s, a); got != 75_000 {
		t.Fatalf("source balance changed on replay: %d", got)
	}

	// Same key, different payload -> key reuse conflict (never replays).
	_, _, err = s.Execute(ctx, "key-basic-1", Request{SourceAccountID: a, DestinationAccountID: b, AmountCents: 1})
	var rerr *KeyReusedError
	if !errors.As(err, &rerr) {
		t.Fatalf("expected key reuse error, got %v", err)
	}
}

func TestExpiredProcessingClaimCanBeReclaimedAndFenced(t *testing.T) {
	db := openDB(t)
	s := &Service{DB: db}
	ctx := context.Background()
	src := mustAccount(t, s, 1, 10_000)
	dst := mustAccount(t, s, 2, 0)
	req := Request{SourceAccountID: src, DestinationAccountID: dst, AmountCents: 1_000}

	oldToken, err := s.claimKey(ctx, "expired-key", req)
	if err != nil {
		t.Fatalf("initial claim: %v", err)
	}
	if _, err := db.Exec(`UPDATE idempotency_keys
		SET expires_at=DATE_SUB(NOW(6), INTERVAL 1 SECOND)
		WHERE scope='transfer' AND idem_key=?`, "expired-key"); err != nil {
		t.Fatalf("expire initial claim: %v", err)
	}
	newToken, err := s.claimKey(ctx, "expired-key", req)
	if err != nil {
		t.Fatalf("reclaim expired key: %v", err)
	}
	if oldToken == newToken {
		t.Fatal("reclaimed claim reused the old fencing token")
	}

	_, err = s.runTransfer(ctx, "expired-key", oldToken, req)
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("old claim should be fenced out, got %v", err)
	}
	if got := balance(t, s, src); got != 10_000 {
		t.Fatalf("stale claim changed source balance: %d", got)
	}

	if _, err := s.runTransfer(ctx, "expired-key", newToken, req); err != nil {
		t.Fatalf("transfer with reclaimed claim: %v", err)
	}
	if got := balance(t, s, src); got != 9_000 {
		t.Fatalf("source balance = %d, want 9000", got)
	}
}

func TestCheckedBalanceAdd(t *testing.T) {
	tests := []struct {
		name    string
		balance int64
		amount  int64
		want    int64
		wantErr bool
	}{
		{name: "normal addition", balance: 10, amount: 5, want: 15},
		{name: "maximum balance", balance: math.MaxInt64 - 1, amount: 1, want: math.MaxInt64},
		{name: "overflow", balance: math.MaxInt64, amount: 1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := checkedBalanceAdd(tt.balance, tt.amount)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %t", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Fatalf("balance = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestConcurrentTransfersBothDirections(t *testing.T) {
	db := openDB(t)
	s := &Service{DB: db}
	ctx := context.Background()
	a := mustAccount(t, s, 1, 1_000_000)
	b := mustAccount(t, s, 2, 1_000_000)

	const n = 60
	const amt = int64(1_000)
	deadlocks := 0

	var wg sync.WaitGroup
	errs := make(chan error, n*2)
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			_, _, err := s.Execute(ctx, fmt.Sprintf("ab-%d", i), Request{SourceAccountID: a, DestinationAccountID: b, AmountCents: amt})
			errs <- err
		}(i)
		go func(i int) {
			defer wg.Done()
			_, _, err := s.Execute(ctx, fmt.Sprintf("ba-%d", i), Request{SourceAccountID: b, DestinationAccountID: a, AmountCents: amt})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err == nil {
			continue
		}
		var iferr *InsufficientFundsError
		if errors.As(err, &iferr) {
			continue // expected once accounts drain
		}
		if isDeadlock(err) {
			deadlocks++
			continue
		}
		t.Fatalf("unexpected error: %v", err)
	}

	ba, bb := balance(t, s, a), balance(t, s, b)
	if ba+bb != 2_000_000 {
		t.Fatalf("money lost or created: %d + %d != 2000000", ba, bb)
	}

	var entries int64
	if err := db.QueryRow("SELECT COUNT(*) FROM ledger_entries").Scan(&entries); err != nil {
		t.Fatal(err)
	}
	var transfersCount int64
	if err := db.QueryRow("SELECT COUNT(*) FROM transfers").Scan(&transfersCount); err != nil {
		t.Fatal(err)
	}
	if entries != transfersCount*2 {
		t.Fatalf("ledger not double-entry: %d entries for %d transfers", entries, transfersCount)
	}
	t.Logf("concurrent test: %d transfers, %d deadlock retries at DB level", transfersCount, deadlocks)
}

func TestConcurrentSameKeySingleExecution(t *testing.T) {
	db := openDB(t)
	s := &Service{DB: db}
	ctx := context.Background()
	a := mustAccount(t, s, 1, 100_000)
	b := mustAccount(t, s, 2, 0)

	const n = 20
	var wg sync.WaitGroup
	results := make(chan Result, n)
	replayedCount := 0
	var mu sync.Mutex
	conflicts := 0

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, replayed, err := s.Execute(ctx, "same-key", Request{SourceAccountID: a, DestinationAccountID: b, AmountCents: 10_000})
			if err != nil {
				mu.Lock()
				conflicts++
				mu.Unlock()
				return
			}
			if replayed {
				mu.Lock()
				replayedCount++
				mu.Unlock()
			}
			results <- res
		}()
	}
	wg.Wait()
	close(results)

	executions := len(results)
	if executions == 0 {
		t.Fatal("no execution succeeded")
	}
	ids := map[int64]bool{}
	for r := range results {
		ids[r.TransferID] = true
	}
	if len(ids) != 1 {
		t.Fatalf("expected exactly one transfer id, got %v", ids)
	}
	if got := balance(t, s, a); got != 90_000 {
		t.Fatalf("debit executed %d times: balance=%d", 100_000-got, got)
	}
	t.Logf("same-key race: %d executed, %d replayed, %d conflicts", executions, replayedCount, conflicts)
}

// TestConcurrentBidirectionalStress fires 50 goroutines A->B (5,000 each,
// A holds 100,000) at the same time as 20 goroutines B->A. It asserts that no
// deadlock or lock-wait error escapes the service, no account ever goes
// negative, and the total money in the system stays constant.
func TestConcurrentBidirectionalStress(t *testing.T) {
	db := openDB(t)
	s := &Service{DB: db}
	ctx := context.Background()
	a := mustAccount(t, s, 1, 100_000)
	b := mustAccount(t, s, 2, 0)

	const (
		nAToB      = 50
		nBToA      = 20
		amt        = int64(5_000)
		totalMoney = int64(100_000)
	)

	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		errs    = make(chan error, nAToB+nBToA)
		mu      sync.Mutex
		okAToB  int
		okBToA  int
		results []Result
	)

	for i := 0; i < nAToB; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			res, _, err := s.Execute(ctx, fmt.Sprintf("stress-ab-%d", i),
				Request{SourceAccountID: a, DestinationAccountID: b, AmountCents: amt})
			if err != nil {
				errs <- err
				return
			}
			mu.Lock()
			okAToB++
			results = append(results, res)
			mu.Unlock()
		}(i)
	}
	for i := 0; i < nBToA; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			res, _, err := s.Execute(ctx, fmt.Sprintf("stress-ba-%d", i),
				Request{SourceAccountID: b, DestinationAccountID: a, AmountCents: amt})
			if err != nil {
				errs <- err
				return
			}
			mu.Lock()
			okBToA++
			results = append(results, res)
			mu.Unlock()
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)

	// Every error must be an ordinary business rejection. A deadlock (1213) or
	// lock wait timeout (1205) reaching the caller means locking is broken.
	insufficient := 0
	for err := range errs {
		var iferr *InsufficientFundsError
		if errors.As(err, &iferr) {
			insufficient++
			continue
		}
		if isDeadlock(err) {
			t.Fatalf("deadlock escaped the service: %v", err)
		}
		t.Fatalf("unexpected error: %v", err)
	}

	// Conservation: A started with everything, B with nothing, so the sum of
	// both balances must still be exactly 100,000.
	gotA, gotB := balance(t, s, a), balance(t, s, b)
	if gotA+gotB != totalMoney {
		t.Fatalf("money lost or created: %d + %d != %d", gotA, gotB, totalMoney)
	}
	// The race may drain A early or route money back via B->A, so only these
	// exact bounds hold: A succeeded at most 20 times net of B->A traffic, and
	// at least one transfer of each bookkeeping side effect happened.
	if okAToB+okBToA == 0 {
		t.Fatal("no transfer succeeded")
	}
	if insufficient != (nAToB+nBToA)-(okAToB+okBToA) {
		t.Fatalf("failed=%d but succeeded=%d/%d: every goroutine must either succeed or reject",
			insufficient, okAToB+okBToA, nAToB+nBToA)
	}
	if gotA != totalMoney-amt*int64(okAToB)+amt*int64(okBToA) {
		t.Fatalf("source balance %d does not match %d A->B and %d B->A successes",
			gotA, okAToB, okBToA)
	}
	if gotA < 0 || gotB < 0 {
		t.Fatalf("negative balance: A=%d B=%d", gotA, gotB)
	}

	// DB-level guard: the CHECK constraint plus this scan rule out any
	// negative balance ever having been committed on any account.
	var minBalance, sumBalance sql.NullInt64
	if err := db.QueryRow("SELECT MIN(balance_cents), SUM(balance_cents) FROM accounts").Scan(&minBalance, &sumBalance); err != nil {
		t.Fatal(err)
	}
	if !minBalance.Valid || minBalance.Int64 < 0 {
		t.Fatalf("negative balance present in accounts: %v", minBalance)
	}
	if !sumBalance.Valid || sumBalance.Int64 != totalMoney {
		t.Fatalf("system total = %d, want %d", sumBalance.Int64, totalMoney)
	}

	// Ledger conservation: per account, debit/credit sums must equal the
	// stored balance, and every transfer must have exactly two entries.
	var netA, netB sql.NullInt64
	if err := db.QueryRow(`SELECT SUM(CASE WHEN direction='debit' THEN -amount_cents ELSE amount_cents END) FROM ledger_entries WHERE account_id=?`, a).Scan(&netA); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT SUM(CASE WHEN direction='debit' THEN -amount_cents ELSE amount_cents END) FROM ledger_entries WHERE account_id=?`, b).Scan(&netB); err != nil {
		t.Fatal(err)
	}
	// Ledger entries record only movements (the opening balance is not an
	// entry), so the two accounts' nets must cancel and each balance must
	// equal its opening balance plus its net entries.
	if !netA.Valid || !netB.Valid || netA.Int64+netB.Int64 != 0 {
		t.Fatalf("ledger not conserved: net A=%v B=%v", netA, netB)
	}
	if totalMoney+netA.Int64 != gotA || netB.Int64 != gotB {
		t.Fatalf("ledger does not reconcile: net A=%v B=%v, balances A=%d B=%d", netA, netB, gotA, gotB)
	}
	var entries, transfersCount int64
	if err := db.QueryRow("SELECT (SELECT COUNT(*) FROM ledger_entries), (SELECT COUNT(*) FROM transfers)").Scan(&entries, &transfersCount); err != nil {
		t.Fatal(err)
	}
	if entries != transfersCount*2 {
		t.Fatalf("ledger not double-entry: %d entries for %d transfers", entries, transfersCount)
	}
	if int64(okAToB+okBToA) != transfersCount {
		t.Fatalf("%d successes but %d transfers rows", okAToB+okBToA, transfersCount)
	}
	t.Logf("stress: %d A->B and %d B->A succeeded, %d rejected, final A=%d B=%d, no deadlocks",
		okAToB, okBToA, insufficient, gotA, gotB)
}

func TestValidationErrors(t *testing.T) {
	db := openDB(t)
	s := &Service{DB: db}
	ctx := context.Background()
	a := mustAccount(t, s, 1, 100)

	cases := []struct {
		name string
		key  string
		req  Request
	}{
		{"missing key", "", Request{SourceAccountID: a, DestinationAccountID: a + 1, AmountCents: 1}},
		{"self transfer", "k1", Request{SourceAccountID: a, DestinationAccountID: a, AmountCents: 1}},
		{"non positive", "k2", Request{SourceAccountID: a, DestinationAccountID: a + 1, AmountCents: 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := s.Execute(ctx, tc.key, tc.req)
			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("want ValidationError, got %v", err)
			}
		})
	}

	b := mustAccount(t, s, 2, 0)
	_, _, err := s.Execute(ctx, "k3", Request{SourceAccountID: b, DestinationAccountID: a, AmountCents: 1})
	var iferr *InsufficientFundsError
	if !errors.As(err, &iferr) {
		t.Fatalf("want insufficient funds, got %v", err)
	}
	if got := balance(t, s, a); got != 100 {
		t.Fatalf("failed transfer changed balance: %d", got)
	}

	// A failed transfer releases its key: retrying the same key after a
	// top-up is allowed and executes once.
	if _, err := db.Exec("UPDATE accounts SET balance_cents=5 WHERE id=?", b); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Execute(ctx, "k3", Request{SourceAccountID: b, DestinationAccountID: a, AmountCents: 1}); err != nil {
		t.Fatalf("retry after failure: %v", err)
	}

	_, _, err = s.Execute(ctx, "k4", Request{SourceAccountID: 999999, DestinationAccountID: a, AmountCents: 1})
	var aerr *AccountError
	if !errors.As(err, &aerr) {
		t.Fatalf("want account error, got %v", err)
	}
}

func isDeadlock(err error) bool {
	type coder interface{ Number() int }
	var ec coder
	if errors.As(err, &ec) {
		n := ec.Number()
		return n == 1213 || n == 1205
	}
	s := err.Error()
	return containsAny(s, "Error 1213", "Error 1205", "Deadlock found", "Lock wait timeout")
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(s) >= len(sub) && indexOf(s, sub) >= 0 {
			return true
		}
	}
	return false
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
