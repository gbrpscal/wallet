package domain

import "time"

// Timestamp normaliza instantes para UTC com precisão de microssegundos, a
// mesma que o PostgreSQL armazena, para que valores persistidos e em memória
// sejam iguais.
func Timestamp(t time.Time) time.Time {
	return t.UTC().Truncate(time.Microsecond)
}
