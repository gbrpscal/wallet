// Package app contém os casos de uso e as portas de que eles dependem.
package app

import "go.uber.org/fx"

// Module fornece os casos de uso.
var Module = fx.Module("app")
