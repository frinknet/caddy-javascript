// Package metric holds the Prometheus counters the handler exports.
package metric

import "github.com/prometheus/client_golang/prometheus"

var (
	Requests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "caddy",
		Subsystem: "javascript",
		Name:      "requests_total",
		Help:      "Total JavaScript handler responses by status code.",
	}, []string{"status"})

	Timeouts = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "caddy",
		Subsystem: "javascript",
		Name:      "timeouts_total",
		Help:      "Total requests interrupted by the execution timeout.",
	})

	QueueFull = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "caddy",
		Subsystem: "javascript",
		Name:      "queue_full_total",
		Help:      "Total requests and callbacks rejected or dropped because the event queue was full.",
	})

	Reloads = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "caddy",
		Subsystem: "javascript",
		Name:      "reloads_total",
		Help:      "Total successful script reloads.",
	})
)

func init() {
	prometheus.MustRegister(Requests, Timeouts, QueueFull, Reloads)
}
