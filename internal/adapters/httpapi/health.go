package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync/atomic"

	"go.uber.org/fx"

	"github.com/gbrpscal/wallet/internal/platform/config"
	"github.com/gbrpscal/wallet/internal/platform/health"
)

// HealthHandler serves the public liveness and readiness endpoints.
type HealthHandler struct {
	checkers     []health.Checker
	cfg          config.Config
	log          *slog.Logger
	shuttingDown atomic.Bool
}

// HealthParams collects the readiness checks contributed by adapters.
type HealthParams struct {
	fx.In
	Checkers []health.Checker `group:"readiness"`
	Config   config.Config
	Log      *slog.Logger
}

// NewHealthHandler builds the handler.
func NewHealthHandler(p HealthParams) *HealthHandler {
	return &HealthHandler{checkers: p.Checkers, cfg: p.Config, log: p.Log}
}

// MarkShuttingDown makes readiness fail so load balancers stop routing new
// requests while in-flight ones finish.
func (h *HealthHandler) MarkShuttingDown() { h.shuttingDown.Store(true) }

type healthResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
}

// Live reports that the process is running.
func (h *HealthHandler) Live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{Status: "ok"})
}

// Ready reports whether PostgreSQL and SQS are reachable. Error details are
// logged, not returned, because the endpoint is public.
func (h *HealthHandler) Ready(w http.ResponseWriter, r *http.Request) {
	resp := healthResponse{Status: "ok", Checks: make(map[string]string, len(h.checkers))}
	if h.shuttingDown.Load() {
		resp.Status = "shutting_down"
	}
	for _, c := range h.checkers {
		ctx, cancel := context.WithTimeout(r.Context(), h.cfg.ReadinessTimeout)
		err := c.Check(ctx)
		cancel()
		if err != nil {
			h.log.Warn("readiness check failed", "check", c.Name(), "error", err)
			resp.Checks[c.Name()] = "unavailable"
			if resp.Status == "ok" {
				resp.Status = "unavailable"
			}
			continue
		}
		resp.Checks[c.Name()] = "ok"
	}
	status := http.StatusOK
	if resp.Status != "ok" {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, resp)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
