package domain

import (
	"errors"
	"fmt"
)

// ErrorKind classifies a domain error so adapters can map it without knowing
// every individual code.
type ErrorKind int

const (
	// KindValidation is a correctable input error. Nothing is persisted.
	KindValidation ErrorKind = iota + 1
	// KindConflict means the request collides with persisted state
	// (idempotency key, external transaction, wallet uniqueness).
	KindConflict
	// KindRejection is a definitive business rejection, persisted as REJECTED.
	KindRejection
	// KindInvariant means persisted or in-memory state violates an invariant.
	// It is never caused by client input.
	KindInvariant
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
	default:
		return "unknown"
	}
}

// Code is a stable, documented failure code.
type Code string

// Stable codes. The kind of each one is fixed by its sentinel below.
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
)

// Error is the single domain error type. Two errors match with errors.Is when
// they carry the same Code, so the sentinels below work with wrapped values
// that add a specific message.
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

// Is reports whether target is a domain error with the same code.
func (e *Error) Is(target error) bool {
	var t *Error
	if !errors.As(target, &t) {
		return false
	}
	return t.Code == e.Code
}

// Sentinels, one per code, for errors.Is.
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
)

var byCode = func() map[Code]*Error {
	m := make(map[Code]*Error)
	for _, e := range []*Error{
		ErrInvalidRequest, ErrInvalidAmount, ErrUnsupportedCurrency, ErrAmountMustBePositive,
		ErrLossAmountMustBeZero, ErrKindNotAllowed, ErrReferenceRequired, ErrIdempotencyConflict,
		ErrExternalTxConflict, ErrWalletAlreadyExists, ErrInsufficientFunds, ErrReversalInsufficient,
		ErrCurrencyMismatch, ErrWalletPlayerMismatch, ErrReferenceNotFound, ErrReferenceNotProcessed,
		ErrReferenceMismatch, ErrReferenceAmount, ErrReferenceNotReversible, ErrAlreadyReversed,
		ErrInconsistentState, ErrInvalidTransition,
	} {
		m[e.Code] = e
	}
	return m
}()

// Errorf returns a copy of the sentinel carrying a specific message.
func Errorf(sentinel *Error, format string, args ...any) *Error {
	return &Error{Kind: sentinel.Kind, Code: sentinel.Code, Message: fmt.Sprintf(format, args...)}
}

// LookupCode returns the sentinel registered for code.
func LookupCode(code Code) (*Error, bool) {
	e, ok := byCode[code]
	return e, ok
}

// KindOf returns the kind of the first domain error in err's chain.
func KindOf(err error) (ErrorKind, bool) {
	var e *Error
	if !errors.As(err, &e) {
		return 0, false
	}
	return e.Kind, true
}

// CodeOf returns the code of the first domain error in err's chain.
func CodeOf(err error) (Code, bool) {
	var e *Error
	if !errors.As(err, &e) {
		return "", false
	}
	return e.Code, true
}
