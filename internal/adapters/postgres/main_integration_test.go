//go:build integration

package postgres_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// migrationsDir é relativo a este pacote.
const migrationsDir = "../../../migrations"

// Pools do banco temporário deste pacote: owner aplica as migrations e
// executa operações privilegiadas; app usa o papel real da aplicação.
var (
	ownerPool *pgxpool.Pool
	appPool   *pgxpool.Pool
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	ctx := context.Background()
	ownerURL := os.Getenv("DATABASE_OWNER_URL")
	appURL := os.Getenv("DATABASE_URL")
	if ownerURL == "" || appURL == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_OWNER_URL and DATABASE_URL are required for integration tests")
		return 1
	}

	name, err := createDatabase(ctx, ownerURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer dropDatabase(ownerURL, name)

	if err := migrate(ctx, withDatabase(ownerURL, name), func(p *goose.Provider) error {
		_, err := p.Up(ctx)
		return err
	}); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		return 1
	}

	if ownerPool, err = pgxpool.New(ctx, withDatabase(ownerURL, name)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer ownerPool.Close()
	if appPool, err = pgxpool.New(ctx, withDatabase(appURL, name)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer appPool.Close()

	return m.Run()
}

// createDatabase cria um banco com nome aleatório, de propriedade do wallet_owner.
func createDatabase(ctx context.Context, ownerURL string) (string, error) {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	name := "wallet_test_" + hex.EncodeToString(buf)
	conn, err := pgx.Connect(ctx, ownerURL)
	if err != nil {
		return "", fmt.Errorf("connect as owner: %w", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		return "", fmt.Errorf("create database: %w", err)
	}
	return name, nil
}

func dropDatabase(ownerURL, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, ownerURL)
	if err != nil {
		return
	}
	defer conn.Close(ctx)
	_, _ = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
}

func withDatabase(raw, name string) string {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	u.Path = "/" + name
	return u.String()
}

func migrate(ctx context.Context, dsn string, fn func(*goose.Provider) error) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	p, err := goose.NewProvider(goose.DialectPostgres, db, os.DirFS(migrationsDir))
	if err != nil {
		return err
	}
	return fn(p)
}

// pgCode devolve o SQLSTATE de um erro do PostgreSQL, ou "" se não houver.
func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// SQLSTATEs usados nas asserções.
const (
	codeIntegrity    = "23000" // triggers de integridade
	codeCheck        = "23514"
	codeUnique       = "23505"
	codeInsufficient = "42501" // insufficient_privilege
)
