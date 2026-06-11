package server

import (
	"net/http"

	"github.com/inferplane/inferplane/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// metricsHandler serves Prometheus exposition for the given registry.
func metricsHandler(m *metrics.Metrics) http.Handler {
	return promhttp.HandlerFor(m.Registry(), promhttp.HandlerOpts{})
}
