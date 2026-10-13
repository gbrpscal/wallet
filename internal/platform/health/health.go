// Package health define as checagens de readiness fornecidas pelos
// adaptadores de infraestrutura e reunidas por um value group do Fx.
package health

import (
	"context"

	"go.uber.org/fx"
)

// Checker informa se uma dependência está pronta para atender tráfego.
type Checker interface {
	Name() string
	Check(ctx context.Context) error
}

// AsChecker anota um construtor para que o resultado entre no grupo de readiness.
func AsChecker(constructor any) any {
	return fx.Annotate(constructor,
		fx.As(new(Checker)),
		fx.ResultTags(`group:"readiness"`),
	)
}

// CheckerFunc adapta uma função à interface Checker.
type CheckerFunc struct {
	CheckName string
	Fn        func(ctx context.Context) error
}

// Name devolve o nome da dependência.
func (c CheckerFunc) Name() string { return c.CheckName }

// Check executa a função subjacente.
func (c CheckerFunc) Check(ctx context.Context) error { return c.Fn(ctx) }
