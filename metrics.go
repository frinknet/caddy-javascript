package caddyjavascript

import "github.com/prometheus/client_golang/prometheus"

var (
	metricRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "caddy",
		Subsystem: "javascript",
		Name:      "requests_total",
		Help:      "Total JavaScript handler responses by status code.",
	}, []string{"status"})

	metricTimeouts = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "caddy",
		Subsystem: "javascript",
		Name:      "timeouts_total",
		Help:      "Total requests interrupted by the execution timeout.",
	})

	metricQueueFull = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "caddy",
		Subsystem: "javascript",
		Name:      "queue_full_total",
		Help:      "Total requests rejected because the event queue was full.",
	})

	metricReloads = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "caddy",
		Subsystem: "javascript",
		Name:      "reloads_total",
		Help:      "Total successful script reloads.",
	})
)

func init() {
	prometheus.MustRegister(metricRequests, metricTimeouts, metricQueueFull, metricReloads)
}
