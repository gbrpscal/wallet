//go:build integration

// Package pgtest cria, para testes de integração, um banco PostgreSQL
// temporário com as migrations aplicadas e os papéis reais (wallet_owner e
// wallet_app).
package pgtest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// DB é um banco temporário de testes.
type DB struct {
	Name     string
	OwnerURL string
	AppURL   string
	// Owner executa operações privilegiadas; App usa o papel da aplicação.
	Owner *pgxpool.Pool
	App   *pgxpool.Pool
	admin string
}

// MigrationsDir devolve o caminho absoluto de migrations/ no repositório.
func MigrationsDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "migrations")
}

// New cria o banco, aplica todas as migrations e abre os pools. Exige
// DATABASE_OWNER_URL e DATABASE_URL no ambiente.
func New(ctx context.Context) (*DB, error) {
	ownerURL, appURL := os.Getenv("DATABASE_OWNER_URL"), os.Getenv("DATABASE_URL")
	if ownerURL == "" || appURL == "" {
		return nil, errors.New("DATABASE_OWNER_URL and DATABASE_URL are required for integration tests")
	}
	name, err := CreateDatabase(ctx, ownerURL)
	if err != nil {
		return nil, err
	}
	db := &DB{Name: name, OwnerURL: WithDatabase(ownerURL, name), AppURL: WithDatabase(appURL, name), admin: ownerURL}
	if err := Migrate(ctx, db.OwnerURL, func(p *goose.Provider) error { _, err := p.Up(ctx); return err }); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if db.Owner, err = pgxpool.New(ctx, db.OwnerURL); err != nil {
		db.Close()
		return nil, err
	}
	if db.App, err = pgxpool.New(ctx, db.AppURL); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// Close fecha os pools e apaga o banco.
func (db *DB) Close() {
	if db.App != nil {
		db.App.Close()
	}
	if db.Owner != nil {
		db.Owner.Close()
	}
	DropDatabase(db.admin, db.Name)
}

// CreateDatabase cria um banco com nome aleatório, de propriedade do wallet_owner.
func CreateDatabase(ctx context.Context, ownerURL string) (string, error) {
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

// DropDatabase apaga o banco, derrubando conexões remanescentes.
func DropDatabase(ownerURL, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, ownerURL)
	if err != nil {
		return
	}
	defer conn.Close(ctx)
	_, _ = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
}

// WithDatabase troca o banco de uma URL de conexão.
func WithDatabase(raw, name string) string {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	u.Path = "/" + name
	return u.String()
}

// Migrate executa fn com um provider do goose sobre o banco.
func Migrate(ctx context.Context, dsn string, fn func(*goose.Provider) error) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	p, err := goose.NewProvider(goose.DialectPostgres, db, os.DirFS(MigrationsDir()))
	if err != nil {
		return err
	}
	return fn(p)
}

// Code devolve o SQLSTATE de um erro do PostgreSQL, ou "" se não houver.
func Code(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}
