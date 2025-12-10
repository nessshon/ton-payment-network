//go:build !(js && wasm)

package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

var (
	WalletBalance      *prometheus.GaugeVec
	ChannelBalance     *prometheus.GaugeVec
	ActiveConditionals *prometheus.GaugeVec
	ActiveActions      *prometheus.GaugeVec
	QueuedTasks        *prometheus.GaugeVec
)

var Registered = false

func RegisterMetrics(namespace string) {
	if Registered {
		return
	}
	Registered = true

	WalletBalance = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:      "wallet_balance",
			Namespace: namespace,
			Subsystem: "payments",
			Help:      "The current balance of wallet.",
		},
		[]string{"coin"},
	)

	ChannelBalance = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:      "channels_balance",
			Namespace: namespace,
			Subsystem: "payments",
			Help:      "The current balance of channels.",
		},
		[]string{"peer", "coin", "is_our", "balance_type"},
	)

	ActiveConditionals = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:      "conditionals_num",
			Namespace: namespace,
			Subsystem: "payments",
			Help:      "Active conditionals count.",
		},
		[]string{"peer", "coin", "is_out"},
	)

	ActiveActions = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:      "actions_num",
			Namespace: namespace,
			Subsystem: "payments",
			Help:      "Active actions count.",
		},
		[]string{"peer", "coin", "is_out"},
	)

	QueuedTasks = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:      "queued_tasks",
			Namespace: namespace,
			Subsystem: "payments",
			Help:      "Number of tasks in the queue.",
		},
		[]string{"job_type", "in_retry", "execute_later"},
	)

	prometheus.MustRegister(ChannelBalance)
	prometheus.MustRegister(ActiveConditionals)
	prometheus.MustRegister(ActiveActions)
	prometheus.MustRegister(QueuedTasks)
	prometheus.MustRegister(WalletBalance)
}
