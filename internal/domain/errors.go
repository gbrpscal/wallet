package domain

import (
	"errors"
	"fmt"
)

// ErrorKind classifica um erro de domínio para que os adaptadores possam
// mapeá-lo sem conhecer cada código individualmente.
type ErrorKind int

const (
	// KindValidation é um erro de entrada corrigível. Nada é persistido.
	KindValidation ErrorKind = iota + 1
	// KindConflict indica colisão com estado persistido (chave de
	// idempotência, transação externa, unicidade de carteira).
	KindConflict
	// KindRejection é uma rejeição de negócio definitiva, persistida como REJECTED.
	KindRejection
	// KindInvariant indica que o estado persistido ou em memória viola uma
	// invariante. Nunca é causado por entrada do cliente.
	KindInvariant
	// KindNotFound indica que o recurso referenciado pela requisição não existe.
	// Nada é persistido.
	KindNotFound
)

func (k ErrorKind) String() string {
	switch k {
	case KindValidation:
		return "validation"
	case KindConflict:
		return "conflict"
	case KindRejection:
		return "rejection"
	case KindInvariant:
		return "invariant"
	case KindNotFound:
		return "not_found"
	default:
		return "unknown"
	}
}

// Code é um código de falha estável e documentado.
type Code string

// Códigos estáveis. A categoria de cada um é fixada pelo sentinel abaixo.
const (
	CodeInvalidRequest         Code = "INVALID_REQUEST"
	CodeInvalidAmount          Code = "INVALID_AMOUNT"
	CodeUnsupportedCurrency    Code = "UNSUPPORTED_CURRENCY"
	CodeAmountMustBePositive   Code = "AMOUNT_MUST_BE_POSITIVE"
	CodeLossAmountMustBeZero   Code = "LOSS_AMOUNT_MUST_BE_ZERO"
	CodeKindNotAllowed         Code = "KIND_NOT_ALLOWED"
	CodeReferenceRequired      Code = "REFERENCE_REQUIRED"
	CodeIdempotencyConflict    Code = "IDEMPOTENCY_KEY_CONFLICT"
	CodeExternalTxConflict     Code = "EXTERNAL_TRANSACTION_CONFLICT"
	CodeWalletAlreadyExists    Code = "WALLET_ALREADY_EXISTS"
	CodeInsufficientFunds      Code = "INSUFFICIENT_FUNDS"
	CodeReversalInsufficient   Code = "REVERSAL_INSUFFICIENT_FUNDS"
	CodeCurrencyMismatch       Code = "CURRENCY_MISMATCH"
	CodeWalletPlayerMismatch   Code = "WALLET_PLAYER_MISMATCH"
	CodeReferenceNotFound      Code = "REFERENCE_NOT_FOUND"
	CodeReferenceNotProcessed  Code = "REFERENCE_NOT_PROCESSED"
	CodeReferenceMismatch      Code = "REFERENCE_MISMATCH"
	CodeReferenceAmount        Code = "REFERENCE_AMOUNT_MISMATCH"
	CodeReferenceNotReversible Code = "REFERENCE_KIND_NOT_REVERSIBLE"
	CodeAlreadyReversed        Code = "ALREADY_REVERSED"
	CodeInconsistentState      Code = "INCONSISTENT_STATE"
	CodeInvalidTransition      Code = "INVALID_STATE_TRANSITION"
	CodeWalletNotFound         Code = "WALLET_NOT_FOUND"
	CodeTransactionNotFound    Code = "TRANSACTION_NOT_FOUND"
	CodeMessageHashMismatch    Code = "MESSAGE_HASH_MISMATCH"
)

// Error é o único tipo de erro do domínio. Dois erros casam com errors.Is
// quando têm o mesmo Code, então os sentinels abaixo funcionam também com
// valores encapsulados que acrescentam uma mensagem específica.
type Error struct {
	Kind    ErrorKind
	Code    Code
	Message string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.Message
}

// Is informa se target é um erro de domínio com o mesmo código.
func (e *Error) Is(target error) bool {
	var t *Error
	if !errors.As(target, &t) {
		return false
	}
	return t.Code == e.Code
}

// Sentinels, um por código, para uso com errors.Is.
var (
	ErrInvalidRequest         = &Error{Kind: KindValidation, Code: CodeInvalidRequest}
	ErrInvalidAmount          = &Error{Kind: KindValidation, Code: CodeInvalidAmount}
	ErrUnsupportedCurrency    = &Error{Kind: KindValidation, Code: CodeUnsupportedCurrency}
	ErrAmountMustBePositive   = &Error{Kind: KindValidation, Code: CodeAmountMustBePositive}
	ErrLossAmountMustBeZero   = &Error{Kind: KindValidation, Code: CodeLossAmountMustBeZero}
	ErrKindNotAllowed         = &Error{Kind: KindValidation, Code: CodeKindNotAllowed}
	ErrReferenceRequired      = &Error{Kind: KindValidation, Code: CodeReferenceRequired}
	ErrIdempotencyConflict    = &Error{Kind: KindConflict, Code: CodeIdempotencyConflict}
	ErrExternalTxConflict     = &Error{Kind: KindConflict, Code: CodeExternalTxConflict}
	ErrWalletAlreadyExists    = &Error{Kind: KindConflict, Code: CodeWalletAlreadyExists}
	ErrInsufficientFunds      = &Error{Kind: KindRejection, Code: CodeInsufficientFunds}
	ErrReversalInsufficient   = &Error{Kind: KindRejection, Code: CodeReversalInsufficient}
	ErrCurrencyMismatch       = &Error{Kind: KindRejection, Code: CodeCurrencyMismatch}
	ErrWalletPlayerMismatch   = &Error{Kind: KindRejection, Code: CodeWalletPlayerMismatch}
	ErrReferenceNotFound      = &Error{Kind: KindRejection, Code: CodeReferenceNotFound}
	ErrReferenceNotProcessed  = &Error{Kind: KindRejection, Code: CodeReferenceNotProcessed}
	ErrReferenceMismatch      = &Error{Kind: KindRejection, Code: CodeReferenceMismatch}
	ErrReferenceAmount        = &Error{Kind: KindRejection, Code: CodeReferenceAmount}
	ErrReferenceNotReversible = &Error{Kind: KindRejection, Code: CodeReferenceNotReversible}
	ErrAlreadyReversed        = &Error{Kind: KindRejection, Code: CodeAlreadyReversed}
	ErrInconsistentState      = &Error{Kind: KindInvariant, Code: CodeInconsistentState}
	ErrInvalidTransition      = &Error{Kind: KindInvariant, Code: CodeInvalidTransition}
	ErrWalletNotFound         = &Error{Kind: KindNotFound, Code: CodeWalletNotFound}
	ErrTransactionNotFound    = &Error{Kind: KindNotFound, Code: CodeTransactionNotFound}
	// ErrMessageHashMismatch: mensagem reentregue com o mesmo messageId e
	// conteúdo diferente do registrado na inbox.
	ErrMessageHashMismatch = &Error{Kind: KindConflict, Code: CodeMessageHashMismatch}
)

var byCode = func() map[Code]*Error {
	m := make(map[Code]*Error)
	for _, e := range []*Error{
		ErrInvalidRequest, ErrInvalidAmount, ErrUnsupportedCurrency, ErrAmountMustBePositive,
		ErrLossAmountMustBeZero, ErrKindNotAllowed, ErrReferenceRequired, ErrIdempotencyConflict,
		ErrExternalTxConflict, ErrWalletAlreadyExists, ErrInsufficientFunds, ErrReversalInsufficient,
		ErrCurrencyMismatch, ErrWalletPlayerMismatch, ErrReferenceNotFound, ErrReferenceNotProcessed,
		ErrReferenceMismatch, ErrReferenceAmount, ErrReferenceNotReversible, ErrAlreadyReversed,
		ErrInconsistentState, ErrInvalidTransition, ErrWalletNotFound, ErrTransactionNotFound, ErrMessageHashMismatch,
	} {
		m[e.Code] = e
	}
	return m
}()

// Errorf devolve uma cópia do sentinel com uma mensagem específica.
func Errorf(sentinel *Error, format string, args ...any) *Error {
	return &Error{Kind: sentinel.Kind, Code: sentinel.Code, Message: fmt.Sprintf(format, args...)}
}

// LookupCode devolve o sentinel registrado para o código.
func LookupCode(code Code) (*Error, bool) {
	e, ok := byCode[code]
	return e, ok
}

// KindOf devolve a categoria do primeiro erro de domínio na cadeia de err.
func KindOf(err error) (ErrorKind, bool) {
	var e *Error
	if !errors.As(err, &e) {
		return 0, false
	}
	return e.Kind, true
}

// CodeOf devolve o código do primeiro erro de domínio na cadeia de err.
func CodeOf(err error) (Code, bool) {
	var e *Error
	if !errors.As(err, &e) {
		return "", false
	}
	return e.Code, true
}
