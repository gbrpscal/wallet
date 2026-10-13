package domain_test

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gbrpscal/wallet/internal/domain"
)

func TestErrorMatchingByCode(t *testing.T) {
	specific := domain.Errorf(domain.ErrInsufficientFunds, "balance 10.00 is lower than 20.00")
	wrapped := fmt.Errorf("process: %w", specific)

	if !errors.Is(wrapped, domain.ErrInsufficientFunds) {
		t.Error("errors.Is must match by code through wrapping")
	}
	if errors.Is(wrapped, domain.ErrReversalInsufficient) {
		t.Error("different codes must not match")
	}
	if k, ok := domain.KindOf(wrapped); !ok || k != domain.KindRejection {
		t.Errorf("KindOf = %v, %v", k, ok)
	}
	if c, ok := domain.CodeOf(wrapped); !ok || c != domain.CodeInsufficientFunds {
		t.Errorf("CodeOf = %v, %v", c, ok)
	}
	var de *domain.Error
	if !errors.As(wrapped, &de) || de.Message == "" {
		t.Error("errors.As must expose the specific message")
	}
	if _, ok := domain.KindOf(errors.New("plain")); ok {
		t.Error("non-domain errors have no kind")
	}
}

func TestBetAndReversalInsufficientFundsCodesDiffer(t *testing.T) {
	if domain.CodeInsufficientFunds == domain.CodeReversalInsufficient {
		t.Fatal("reversal without funds must use a distinct failure code")
	}
}

func TestParseID(t *testing.T) {
	const canonical = "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1"
	id, err := domain.ParseID[domain.PlayerID](canonical)
	if err != nil || id.String() != canonical {
		t.Fatalf("ParseID = %v, %v", id, err)
	}
	upper, err := domain.ParseID[domain.PlayerID](strings.ToUpper(canonical))
	if err != nil || upper != id {
		t.Errorf("upper-case form must normalize to the canonical id: %v, %v", upper, err)
	}
	for _, in := range []string{
		"", "not-a-uuid", "00000000-0000-0000-0000-000000000000",
		"urn:uuid:" + canonical, "{" + canonical + "}", strings.ReplaceAll(canonical, "-", ""),
	} {
		if _, err := domain.ParseID[domain.PlayerID](in); !errors.Is(err, domain.ErrInvalidRequest) {
			t.Errorf("ParseID(%q) error = %v", in, err)
		}
	}
	var zero domain.WalletID
	if !zero.IsZero() {
		t.Error("zero value id must report IsZero")
	}
	if _, err := zero.MarshalText(); err == nil {
		t.Error("uninitialized id must not serialize")
	}
	n, err := domain.NewID[domain.WalletID]()
	if err != nil || n.IsZero() || n.String()[14] != '7' {
		t.Errorf("NewID must produce a UUIDv7: %v, %v", n, err)
	}
}

func TestValidateTextID(t *testing.T) {
	if err := domain.ValidateTextID("providerId", "provider-a"); err != nil {
		t.Errorf("valid id rejected: %v", err)
	}
	if err := domain.ValidateTextID("providerId", strings.Repeat("a", domain.MaxTextIDLength)); err != nil {
		t.Errorf("id at the limit rejected: %v", err)
	}
	for _, in := range []string{
		"", " provider", "provider ", "pro\nvider", "pro\x00vider", "\xff\xfe",
		strings.Repeat("a", domain.MaxTextIDLength+1),
	} {
		if err := domain.ValidateTextID("providerId", in); !errors.Is(err, domain.ErrInvalidRequest) {
			t.Errorf("ValidateTextID(%q) = %v", in, err)
		}
	}
}

// TestNoFloatingPointInProductionCode falha se algum arquivo Go de produção em
// internal/ ou cmd/ mencionar float32 ou float64, para que dinheiro nunca passe
// por ponto flutuante.
func TestNoFloatingPointInProductionCode(t *testing.T) {
	roots := []string{"..", filepath.Join("..", "..", "cmd")}
	fset := token.NewFileSet()
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && (id.Name == "float32" || id.Name == "float64") {
					t.Errorf("%s: floating point type %s is forbidden", fset.Position(id.Pos()), id.Name)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// TestDomainHasNoInfrastructureImports mantém o domínio independente de Fx,
// HTTP, SQS e bibliotecas de persistência.
func TestDomainHasNoInfrastructureImports(t *testing.T) {
	forbidden := []string{"go.uber.org/fx", "net/http", "github.com/aws/", "github.com/jackc/", "database/sql"}
	fset := token.NewFileSet()
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			for _, bad := range forbidden {
				if strings.HasPrefix(p, bad) {
					t.Errorf("%s imports %s", path, p)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
