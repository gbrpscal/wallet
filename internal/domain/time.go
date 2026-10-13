package domain

import "time"

// Timestamp normalizes instants to UTC with microsecond precision, the
// precision PostgreSQL stores, so persisted and in-memory values are equal.
func Timestamp(t time.Time) time.Time {
	return t.UTC().Truncate(time.Microsecond)
}
