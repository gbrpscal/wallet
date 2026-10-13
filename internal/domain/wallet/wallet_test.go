package wallet_test

import (
	"errors"
	"testing"
	"time"

	"github.com/gbrpscal/wallet/internal/domain"
	"github.com/gbrpscal/wallet/internal/domain/money"
	"github.com/gbrpscal/wallet/internal/domain/wallet"
)

var (
	t0       = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	playerID = domain.MustParseID[domain.PlayerID]("0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1")
	walletID = domain.MustParseID[domain.WalletID]("0192f291-27dd-7d3f-8071-5f8685deef37")
)

func brl(t *testing.T, amount string) money.Money {
	t.Helper()
	m, err := money.Parse(amount, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func txID(t *testing.T) domain.TransactionID {
	t.Helper()
	id, err := domain.NewID[domain.TransactionID]()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func open(t *testing.T, initial string) *wallet.Wallet {
	t.Helper()
	w, _, err := wallet.Open(walletID, playerID, brl(t, initial), txID(t), t0)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestOpenWithPositiveBalance(t *testing.T) {
	opening := txID(t)
	w, entry, err := wallet.Open(walletID, playerID, brl(t, "1000.00"), opening, t0)
	if err != nil {
		t.Fatal(err)
	}
	if w.Version() != wallet.InitialVersion || w.Version() != 1 {
		t.Errorf("version = %d, want 1", w.Version())
	}
	if w.Balance().Amount() != "1000.00" || w.Currency() != money.BRL {
		t.Errorf("balance = %v", w.Balance())
	}
	if entry == nil {
		t.Fatal("positive opening must produce a ledger entry")
	}
	if entry.Direction() != wallet.Credit || entry.TransactionID() != opening || entry.WalletVersion() != 1 ||
		entry.BalanceBefore().Amount() != "0.00" || entry.BalanceAfter().Amount() != "1000.00" {
		t.Errorf("unexpected opening entry %+v", entry)
	}
}

func TestOpenWithZeroBalanceHasNoEntry(t *testing.T) {
	w, entry, err := wallet.Open(walletID, playerID, brl(t, "0.00"), domain.TransactionID{}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if entry != nil {
		t.Error("zero opening must not produce a ledger entry")
	}
	if w.Version() != 1 || !w.Balance().IsZero() {
		t.Errorf("wallet = v%d %v", w.Version(), w.Balance())
	}
}

func TestOpenValidation(t *testing.T) {
	neg, _ := money.FromMinor(-1, money.BRL)
	cases := map[string]func() error{
		"zero wallet id": func() error {
			_, _, err := wallet.Open(domain.WalletID{}, playerID, brl(t, "1.00"), txID(t), t0)
			return err
		},
		"zero player id": func() error {
			_, _, err := wallet.Open(walletID, domain.PlayerID{}, brl(t, "1.00"), txID(t), t0)
			return err
		},
		"uninitialized": func() error { _, _, err := wallet.Open(walletID, playerID, money.Money{}, txID(t), t0); return err },
		"negative":      func() error { _, _, err := wallet.Open(walletID, playerID, neg, txID(t), t0); return err },
		"no opening tx": func() error {
			_, _, err := wallet.Open(walletID, playerID, brl(t, "1.00"), domain.TransactionID{}, t0)
			return err
		},
		"zero time": func() error {
			_, _, err := wallet.Open(walletID, playerID, brl(t, "1.00"), txID(t), time.Time{})
			return err
		},
	}
	for name, fn := range cases {
		if fn() == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestDebitAndCredit(t *testing.T) {
	w := open(t, "100.00")

	e1, err := w.Debit(txID(t), brl(t, "80.00"), t0.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if w.Balance().Amount() != "20.00" || w.Version() != 2 {
		t.Errorf("after debit: %v v%d", w.Balance(), w.Version())
	}
	if e1.BalanceBefore().Amount() != "100.00" || e1.BalanceAfter().Amount() != "20.00" || e1.Direction() != wallet.Debit || e1.WalletVersion() != 2 {
		t.Errorf("debit entry %+v", e1)
	}

	e2, err := w.Credit(txID(t), brl(t, "5.50"), t0.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if w.Balance().Amount() != "25.50" || w.Version() != 3 {
		t.Errorf("after credit: %v v%d", w.Balance(), w.Version())
	}
	if e2.BalanceBefore() != e1.BalanceAfter() {
		t.Error("entries must chain balances")
	}
	if !w.UpdatedAt().Equal(t0.Add(2 * time.Second)) {
		t.Errorf("updatedAt = %v", w.UpdatedAt())
	}
}

func TestDebitCanEmptyTheWallet(t *testing.T) {
	w := open(t, "80.00")
	if _, err := w.Debit(txID(t), brl(t, "80.00"), t0); err != nil {
		t.Fatal(err)
	}
	if !w.Balance().IsZero() {
		t.Errorf("balance = %v", w.Balance())
	}
}

func TestInsufficientFundsLeavesWalletUnchanged(t *testing.T) {
	w := open(t, "100.00")
	_, err := w.Debit(txID(t), brl(t, "100.01"), t0)
	if !errors.Is(err, domain.ErrInsufficientFunds) {
		t.Fatalf("error = %v", err)
	}
	if w.Balance().Amount() != "100.00" || w.Version() != 1 {
		t.Errorf("wallet changed after rejection: %v v%d", w.Balance(), w.Version())
	}
}

func TestMovementValidation(t *testing.T) {
	w := open(t, "100.00")
	usd, _ := money.Parse("1.00", "USD")
	cases := map[string]struct {
		run  func() error
		want *domain.Error
	}{
		"currency mismatch": {func() error { _, err := w.Credit(txID(t), usd, t0); return err }, domain.ErrCurrencyMismatch},
		"zero amount":       {func() error { _, err := w.Debit(txID(t), brl(t, "0.00"), t0); return err }, domain.ErrAmountMustBePositive},
		"uninitialized":     {func() error { _, err := w.Credit(txID(t), money.Money{}, t0); return err }, domain.ErrInvalidAmount},
		"no transaction":    {func() error { _, err := w.Credit(domain.TransactionID{}, brl(t, "1.00"), t0); return err }, domain.ErrInconsistentState},
		"zero time":         {func() error { _, err := w.Credit(txID(t), brl(t, "1.00"), time.Time{}); return err }, domain.ErrInconsistentState},
	}
	for name, c := range cases {
		if err := c.run(); !errors.Is(err, c.want) {
			t.Errorf("%s: error = %v, want %s", name, err, c.want.Code)
		}
	}
	if w.Version() != 1 || w.Balance().Amount() != "100.00" {
		t.Error("rejected movements must not change the wallet")
	}
}

func snapshot(t *testing.T) wallet.Snapshot {
	return wallet.Snapshot{
		ID: walletID, PlayerID: playerID, Currency: money.BRL, Balance: brl(t, "975.00"),
		Version: 2, CreatedAt: t0, UpdatedAt: t0.Add(time.Minute),
	}
}

func TestRehydrateDoesNotApplyMovements(t *testing.T) {
	w, err := wallet.Rehydrate(snapshot(t))
	if err != nil {
		t.Fatal(err)
	}
	if w.Version() != 2 || w.Balance().Amount() != "975.00" || !w.UpdatedAt().Equal(t0.Add(time.Minute)) {
		t.Errorf("rehydrated wallet differs from snapshot: v%d %v", w.Version(), w.Balance())
	}
}

func TestRehydrateRejectsInconsistentState(t *testing.T) {
	neg, _ := money.FromMinor(-1, money.BRL)
	usd, _ := money.Parse("1.00", "USD")
	mutations := map[string]func(*wallet.Snapshot){
		"zero id":          func(s *wallet.Snapshot) { s.ID = domain.WalletID{} },
		"zero player":      func(s *wallet.Snapshot) { s.PlayerID = domain.PlayerID{} },
		"negative":         func(s *wallet.Snapshot) { s.Balance = neg },
		"currency":         func(s *wallet.Snapshot) { s.Balance = usd },
		"no currency":      func(s *wallet.Snapshot) { s.Currency = money.Currency{} },
		"version zero":     func(s *wallet.Snapshot) { s.Version = 0 },
		"updated < create": func(s *wallet.Snapshot) { s.UpdatedAt = t0.Add(-time.Second) },
	}
	for name, mutate := range mutations {
		s := snapshot(t)
		mutate(&s)
		if _, err := wallet.Rehydrate(s); !errors.Is(err, domain.ErrInconsistentState) {
			t.Errorf("%s: error = %v", name, err)
		}
	}
}

func TestLedgerEntryInvariant(t *testing.T) {
	entryID, _ := domain.NewID[domain.LedgerEntryID]()
	base := wallet.LedgerEntrySnapshot{
		ID: entryID, WalletID: walletID, TransactionID: txID(t), Direction: wallet.Debit,
		Amount: brl(t, "25.00"), BalanceBefore: brl(t, "1000.00"), BalanceAfter: brl(t, "975.00"), WalletVersion: 2, CreatedAt: t0,
	}
	if _, err := wallet.NewLedgerEntry(base); err != nil {
		t.Fatalf("valid entry rejected: %v", err)
	}

	usd, _ := money.Parse("25.00", "USD")
	mutations := map[string]func(*wallet.LedgerEntrySnapshot){
		"wrong after":     func(s *wallet.LedgerEntrySnapshot) { s.BalanceAfter = brl(t, "976.00") },
		"wrong direction": func(s *wallet.LedgerEntrySnapshot) { s.Direction = wallet.Credit },
		"bad direction":   func(s *wallet.LedgerEntrySnapshot) { s.Direction = "SIDEWAYS" },
		"zero amount":     func(s *wallet.LedgerEntrySnapshot) { s.Amount = brl(t, "0.00"); s.BalanceAfter = s.BalanceBefore },
		"currency":        func(s *wallet.LedgerEntrySnapshot) { s.Amount = usd },
		"negative after": func(s *wallet.LedgerEntrySnapshot) {
			s.BalanceBefore = brl(t, "10.00")
			s.BalanceAfter, _ = money.FromMinor(-1500, money.BRL)
		},
		"no transaction": func(s *wallet.LedgerEntrySnapshot) { s.TransactionID = domain.TransactionID{} },
		"no time":        func(s *wallet.LedgerEntrySnapshot) { s.CreatedAt = time.Time{} },
		"no version":     func(s *wallet.LedgerEntrySnapshot) { s.WalletVersion = 0 },
	}
	for name, mutate := range mutations {
		s := base
		mutate(&s)
		if _, err := wallet.NewLedgerEntry(s); !errors.Is(err, domain.ErrInconsistentState) {
			t.Errorf("%s: error = %v", name, err)
		}
	}
}

func TestSignedAmount(t *testing.T) {
	w := open(t, "100.00")
	debit, _ := w.Debit(txID(t), brl(t, "30.00"), t0)
	credit, _ := w.Credit(txID(t), brl(t, "10.00"), t0)
	if s, _ := debit.SignedAmount(); s.Amount() != "-30.00" {
		t.Errorf("debit signed = %v", s)
	}
	if s, _ := credit.SignedAmount(); s.Amount() != "10.00" {
		t.Errorf("credit signed = %v", s)
	}
}

func TestTimestampsUseUTCMicroseconds(t *testing.T) {
	local := time.Date(2026, 9, 8, 9, 0, 0, 123456789, time.FixedZone("BRT", -3*3600))
	w, _, err := wallet.Open(walletID, playerID, brl(t, "1.00"), txID(t), local)
	if err != nil {
		t.Fatal(err)
	}
	if w.CreatedAt().Location() != time.UTC || w.CreatedAt().Nanosecond() != 123456000 {
		t.Errorf("createdAt = %v", w.CreatedAt())
	}
}
