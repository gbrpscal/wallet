//go:build integration

package postgres_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gbrpscal/wallet/internal/testutil/pgtest"
)

// Pools do banco temporário deste pacote: owner aplica as migrations e
// executa operações privilegiadas; app usa o papel real da aplicação.
var (
	ownerPool *pgxpool.Pool
	appPool   *pgxpool.Pool
)

func TestMain(m *testing.M) {
	db, err := pgtest.New(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ownerPool, appPool = db.Owner, db.App
	code := m.Run()
	db.Close()
	os.Exit(code)
}

// pgCode devolve o SQLSTATE de um erro do PostgreSQL, ou "" se não houver.
func pgCode(err error) string { return pgtest.Code(err) }

// SQLSTATEs usados nas asserções.
const (
	codeIntegrity    = "23000" // triggers de integridade
	codeCheck        = "23514"
	codeUnique       = "23505"
	codeInsufficient = "42501" // insufficient_privilege
)
