package auth

import (
	"net/http"
	"os"
	"strings"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

const requireFillFirstEnv = "EPK_PROXY_REQUIRE_FILL_FIRST"

func envTruthy(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func (m *Manager) routingStrategyError() error {
	if m == nil || !m.requireFillFirst {
		return nil
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	strategy := ""
	if cfg != nil {
		strategy = strings.ToLower(strings.TrimSpace(cfg.Routing.Strategy))
	}
	switch strategy {
	case "fill-first", "fillfirst", "ff":
		return nil
	default:
		if strategy == "" {
			strategy = "unset"
		}
		return &Error{
			Code:       "routing_strategy_disabled",
			Message:    "routing strategy is disabled by control-plane law: " + strategy + "; require fill-first",
			Retryable:  false,
			HTTPStatus: http.StatusConflict,
		}
	}
}
