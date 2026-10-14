package httpapi

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gbrpscal/wallet/internal/app"
	"github.com/gbrpscal/wallet/internal/domain"
	"github.com/gbrpscal/wallet/internal/domain/event"
	"github.com/gbrpscal/wallet/internal/domain/money"
	"github.com/gbrpscal/wallet/internal/domain/wagering"
	"github.com/gbrpscal/wallet/internal/domain/wallet"
)

// API expõe os casos de uso por HTTP.
type API struct {
	svc  *app.Service
	auth Authenticator
	log  *slog.Logger
}

// NewAPI constrói a API.
func NewAPI(svc *app.Service, auth Authenticator, log *slog.Logger) *API {
	return &API{svc: svc, auth: auth, log: log}
}

// routes registra as rotas autenticadas com o scope exigido por cada uma.
func (a *API) routes(mux *http.ServeMux) {
	mux.Handle("POST /wallets", a.requireScope(ScopeWalletsWrite, a.openWallet))
	mux.Handle("GET /wallets/{walletId}", a.requireScope(ScopeWalletsRead, a.getWallet))
	mux.Handle("GET /wallets/{walletId}/ledger", a.requireScope(ScopeWalletsRead, a.listLedger))
	mux.Handle("POST /wallets/{walletId}/reconciliation", a.requireScope(ScopeWalletsReconcile, a.reconcile))
	mux.Handle("POST /wagering/transactions", a.requireScope(ScopeWageringWrite, a.submitOperation))
	mux.Handle("GET /wagering/transactions/{transactionId}", a.requireScope(ScopeWageringRead, a.getTransaction))
	mux.Handle("GET /providers/{providerId}/wagering/transactions/{externalTransactionId}",
		a.requireScope(ScopeWageringRead, a.getTransactionByExternalID))
}

// moneyInput aceita só {"amount":"<string>","currency":"<string>"}; um valor
// numérico (que passaria por ponto flutuante) é rejeitado com INVALID_AMOUNT.
type moneyInput struct {
	Amount   string
	Currency string
}

func (m *moneyInput) UnmarshalJSON(b []byte) error {
	var raw struct {
		Amount   json.RawMessage `json:"amount"`
		Currency string          `json:"currency"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return domain.Errorf(domain.ErrInvalidAmount, "money must be an object with string amount and currency")
	}
	if len(raw.Amount) > 0 && raw.Amount[0] != '"' {
		return domain.Errorf(domain.ErrInvalidAmount, "amount must be a decimal string, e.g. \"25.00\"")
	}
	if len(raw.Amount) > 0 {
		if err := json.Unmarshal(raw.Amount, &m.Amount); err != nil {
			return domain.Errorf(domain.ErrInvalidAmount, "amount must be a decimal string")
		}
	}
	m.Currency = raw.Currency
	return nil
}

type moneyView struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

func viewMoney(m money.Money) moneyView {
	return moneyView{Amount: m.Amount(), Currency: m.Currency().String()}
}

func viewMoneyPtr(m money.Money, ok bool) *moneyView {
	if !ok {
		return nil
	}
	v := viewMoney(m)
	return &v
}

func timestamp(t time.Time) string { return t.UTC().Format(event.TimeLayout) }

func trace(r *http.Request) event.Trace {
	return event.Trace{CorrelationID: correlationFrom(r.Context())}
}

// --- Carteiras ---------------------------------------------------------------

type openWalletRequest struct {
	PlayerID       string     `json:"playerId"`
	InitialBalance moneyInput `json:"initialBalance"`
}

type walletView struct {
	ID       string    `json:"id"`
	PlayerID string    `json:"playerId"`
	Balance  moneyView `json:"balance"`
	Version  int64     `json:"version"`
}

func viewWallet(w *wallet.Wallet) walletView {
	return walletView{ID: w.ID().String(), PlayerID: w.PlayerID().String(), Balance: viewMoney(w.Balance()), Version: w.Version()}
}

func (a *API) openWallet(w http.ResponseWriter, r *http.Request) {
	var req openWalletRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.failDecode(w, r, err)
		return
	}
	opened, err := a.svc.OpenWallet(r.Context(), app.OpenWalletInput{
		PlayerID: req.PlayerID, Amount: req.InitialBalance.Amount, Currency: req.InitialBalance.Currency,
	}, trace(r))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/wallets/"+opened.ID().String())
	writeJSON(w, http.StatusCreated, viewWallet(opened))
}

func (a *API) walletID(w http.ResponseWriter, r *http.Request) (domain.WalletID, bool) {
	id, err := domain.ParseID[domain.WalletID](r.PathValue("walletId"))
	if err != nil {
		a.fail(w, r, domain.Errorf(domain.ErrInvalidRequest, "walletId must be a uuid"))
		return domain.WalletID{}, false
	}
	return id, true
}

func (a *API) getWallet(w http.ResponseWriter, r *http.Request) {
	id, ok := a.walletID(w, r)
	if !ok {
		return
	}
	got, err := a.svc.GetWallet(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, viewWallet(got))
}

type ledgerEntryView struct {
	ID            string    `json:"id"`
	TransactionID string    `json:"transactionId"`
	Direction     string    `json:"direction"`
	Money         moneyView `json:"money"`
	BalanceBefore moneyView `json:"balanceBefore"`
	BalanceAfter  moneyView `json:"balanceAfter"`
	WalletVersion int64     `json:"walletVersion"`
	CreatedAt     string    `json:"createdAt"`
}

type ledgerPageView struct {
	WalletID   string            `json:"walletId"`
	Entries    []ledgerEntryView `json:"entries"`
	NextCursor *string           `json:"nextCursor"`
}

func (a *API) listLedger(w http.ResponseWriter, r *http.Request) {
	id, ok := a.walletID(w, r)
	if !ok {
		return
	}
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n == 0 {
			a.fail(w, r, domain.Errorf(domain.ErrInvalidRequest, "limit must be an integer between 1 and %d", app.MaxPageSize))
			return
		}
		limit = n
	}
	page, err := a.svc.ListLedger(r.Context(), id, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	view := ledgerPageView{WalletID: id.String(), Entries: make([]ledgerEntryView, 0, len(page.Entries))}
	for _, e := range page.Entries {
		view.Entries = append(view.Entries, ledgerEntryView{
			ID: e.ID().String(), TransactionID: e.TransactionID().String(), Direction: string(e.Direction()),
			Money: viewMoney(e.Amount()), BalanceBefore: viewMoney(e.BalanceBefore()), BalanceAfter: viewMoney(e.BalanceAfter()),
			WalletVersion: e.WalletVersion(), CreatedAt: timestamp(e.CreatedAt()),
		})
	}
	if page.NextCursor != "" {
		view.NextCursor = &page.NextCursor
	}
	writeJSON(w, http.StatusOK, view)
}

type reconciliationView struct {
	WalletID          string    `json:"walletId"`
	StoredBalance     moneyView `json:"storedBalance"`
	CalculatedBalance moneyView `json:"calculatedBalance"`
	Difference        moneyView `json:"difference"`
	Consistent        bool      `json:"consistent"`
	CheckedEntries    int       `json:"checkedEntries"`
}

func (a *API) reconcile(w http.ResponseWriter, r *http.Request) {
	id, ok := a.walletID(w, r)
	if !ok {
		return
	}
	rec, err := a.svc.Reconcile(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, reconciliationView{
		WalletID: rec.WalletID.String(), StoredBalance: viewMoney(rec.Stored),
		CalculatedBalance: viewMoney(rec.Calculated), Difference: viewMoney(rec.Difference),
		Consistent: rec.Consistent, CheckedEntries: rec.CheckedEntries,
	})
}

// --- Operações de wagering ------------------------------------------------------

// IdempotencyKeyHeader é obrigatório no envio de operações.
const IdempotencyKeyHeader = "Idempotency-Key"

type operationRequest struct {
	ProviderID                     string     `json:"providerId"`
	ExternalTransactionID          string     `json:"externalTransactionId"`
	PlayerID                       string     `json:"playerId"`
	WalletID                       string     `json:"walletId"`
	RoundID                        string     `json:"roundId"`
	GameID                         string     `json:"gameId"`
	Kind                           string     `json:"kind"`
	Money                          moneyInput `json:"money"`
	ReferenceExternalTransactionID string     `json:"referenceExternalTransactionId"`
}

type operationResultView struct {
	TransactionID    string     `json:"transactionId"`
	Status           string     `json:"status"`
	Balance          *moneyView `json:"balance,omitempty"`
	FailureCode      string     `json:"failureCode,omitempty"`
	IdempotentReplay bool       `json:"idempotentReplay"`
}

// statusFor: PROCESSED 200; REJECTED 422; em espera 202 (com Location para
// acompanhar); FAILED 500. Replays usam o mesmo status do original.
func statusFor(s wagering.Status) int {
	switch s {
	case wagering.Processed:
		return http.StatusOK
	case wagering.Rejected:
		return http.StatusUnprocessableEntity
	case wagering.Pending, wagering.PendingReference:
		return http.StatusAccepted
	default:
		return http.StatusInternalServerError
	}
}

func (a *API) submitOperation(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get(IdempotencyKeyHeader)
	if key == "" {
		a.fail(w, r, domain.Errorf(domain.ErrInvalidRequest, "%s header is required", IdempotencyKeyHeader))
		return
	}
	var req operationRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.failDecode(w, r, err)
		return
	}
	// Isolamento: o provedor só opera em seu próprio nome.
	p := principalFrom(r.Context())
	if p.ProviderID == "" || (req.ProviderID != "" && req.ProviderID != p.ProviderID) {
		a.problem(w, r, http.StatusForbidden, CodeForbidden, "providerId does not match the authenticated provider")
		return
	}
	res, err := a.svc.SubmitOperation(r.Context(), wagering.RequestInput{
		ProviderID: req.ProviderID, ExternalTransactionID: req.ExternalTransactionID, IdempotencyKey: key,
		PlayerID: req.PlayerID, WalletID: req.WalletID, RoundID: req.RoundID, GameID: req.GameID,
		Kind: req.Kind, Amount: req.Money.Amount, Currency: req.Money.Currency,
		ReferenceExternalTransactionID: req.ReferenceExternalTransactionID,
	}, trace(r))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	t := res.Transaction
	status := statusFor(t.Status())
	if status == http.StatusAccepted {
		w.Header().Set("Location", "/wagering/transactions/"+t.ID().String())
	}
	writeJSON(w, status, operationResultView{
		TransactionID: t.ID().String(), Status: string(t.Status()), Balance: viewMoneyPtr(t.ResultBalance()),
		FailureCode: string(t.FailureCode()), IdempotentReplay: res.Replay,
	})
}

type transactionView struct {
	TransactionID                  string     `json:"transactionId"`
	ProviderID                     string     `json:"providerId"`
	ExternalTransactionID          string     `json:"externalTransactionId"`
	WalletID                       string     `json:"walletId"`
	PlayerID                       string     `json:"playerId"`
	RoundID                        string     `json:"roundId"`
	GameID                         string     `json:"gameId"`
	Kind                           string     `json:"kind"`
	Money                          moneyView  `json:"money"`
	ReferenceExternalTransactionID string     `json:"referenceExternalTransactionId,omitempty"`
	ReferenceTransactionID         string     `json:"referenceTransactionId,omitempty"`
	Status                         string     `json:"status"`
	FailureCode                    string     `json:"failureCode,omitempty"`
	Balance                        *moneyView `json:"balance,omitempty"`
	CreatedAt                      string     `json:"createdAt"`
	UpdatedAt                      string     `json:"updatedAt"`
}

func viewTransaction(t *wagering.WagerTransaction) transactionView {
	v := transactionView{
		TransactionID: t.ID().String(), ProviderID: t.ProviderID(), ExternalTransactionID: t.ExternalTransactionID(),
		WalletID: t.WalletID().String(), PlayerID: t.PlayerID().String(), RoundID: t.RoundID(), GameID: t.GameID(),
		Kind: string(t.Kind()), Money: viewMoney(t.Money()),
		ReferenceExternalTransactionID: t.ReferenceExternalTransactionID(),
		Status:                         string(t.Status()), FailureCode: string(t.FailureCode()), Balance: viewMoneyPtr(t.ResultBalance()),
		CreatedAt: timestamp(t.CreatedAt()), UpdatedAt: timestamp(t.UpdatedAt()),
	}
	if ref, ok := t.ReferenceTransactionID(); ok {
		v.ReferenceTransactionID = ref.String()
	}
	return v
}

// getTransaction só devolve transações do próprio provedor; as de outro
// provedor, e o OPENING interno, respondem 404 sem revelar que existem.
func (a *API) getTransaction(w http.ResponseWriter, r *http.Request) {
	id, err := domain.ParseID[domain.TransactionID](r.PathValue("transactionId"))
	if err != nil {
		a.fail(w, r, domain.Errorf(domain.ErrInvalidRequest, "transactionId must be a uuid"))
		return
	}
	t, err := a.svc.GetTransaction(r.Context(), id)
	if err == nil && (t.IsInternal() || t.ProviderID() != principalFrom(r.Context()).ProviderID) {
		err = domain.Errorf(domain.ErrTransactionNotFound, "transaction %s not found", id)
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, viewTransaction(t))
}

func (a *API) getTransactionByExternalID(w http.ResponseWriter, r *http.Request) {
	providerID := r.PathValue("providerId")
	if providerID != principalFrom(r.Context()).ProviderID {
		a.problem(w, r, http.StatusForbidden, CodeForbidden, "providerId does not match the authenticated provider")
		return
	}
	t, err := a.svc.GetTransactionByExternalID(r.Context(), providerID, r.PathValue("externalTransactionId"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, viewTransaction(t))
}
