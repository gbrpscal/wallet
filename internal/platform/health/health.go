// Package health defines readiness checks contributed by infrastructure
// adapters and collected through an Fx value group.
package health

import (
	"context"

	"go.uber.org/fx"
)

// Checker reports whether one dependency is ready to serve traffic.
type Checker interface {
	Name() string
	Check(ctx context.Context) error
}

// AsChecker annotates a constructor so its result joins the readiness group.
func AsChecker(constructor any) any {
	return fx.Annotate(constructor,
		fx.As(new(Checker)),
		fx.ResultTags(`group:"readiness"`),
	)
}

// CheckerFunc adapts a function to the Checker interface.
type CheckerFunc struct {
	CheckName string
	Fn        func(ctx context.Context) error
}

// Name returns the dependency name.
func (c CheckerFunc) Name() string { return c.CheckName }

// Check runs the underlying function.
func (c CheckerFunc) Check(ctx context.Context) error { return c.Fn(ctx) }
