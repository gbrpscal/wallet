package app

import "errors"

// Erros de infraestrutura vistos pelos casos de uso. Rejeições de negócio
// continuam sendo *domain.Error.
var (
	// ErrNotFound indica que o registro procurado não existe.
	ErrNotFound = errors.New("not found")
	// ErrTransient indica falha temporária (conexão, deadlock, serialização,
	// timeout de lock). Nada foi confirmado; a operação pode ser repetida.
	ErrTransient = errors.New("transient failure")
	// ErrConcurrentUpdate indica que a linha mudou entre a leitura e a
	// escrita (versão ou estado diferente do esperado). Nada foi confirmado;
	// a operação pode ser repetida.
	ErrConcurrentUpdate = errors.New("concurrent update")
	// ErrDuplicate indica violação de unicidade sem significado de negócio
	// próprio; DuplicateError informa a constraint.
	ErrDuplicate = errors.New("duplicate")
)

// DuplicateError identifica a constraint de unicidade violada, para que o
// caso de uso decida (por exemplo, reler e devolver o replay).
type DuplicateError struct {
	Constraint string
}

func (e *DuplicateError) Error() string { return "duplicate: " + e.Constraint }

// Is faz errors.Is(err, ErrDuplicate) funcionar.
func (e *DuplicateError) Is(target error) bool { return target == ErrDuplicate }

// TransientError encapsula uma falha temporária preservando a causa.
type TransientError struct {
	Err error
}

func (e *TransientError) Error() string { return "transient: " + e.Err.Error() }

// Unwrap expõe a causa original.
func (e *TransientError) Unwrap() error { return e.Err }

// Is faz errors.Is(err, ErrTransient) funcionar.
func (e *TransientError) Is(target error) bool { return target == ErrTransient }

// IsRetryable informa se a operação pode ser repetida com segurança, porque
// nada foi confirmado.
func IsRetryable(err error) bool {
	return errors.Is(err, ErrTransient) || errors.Is(err, ErrConcurrentUpdate)
}
