package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

type Metrics struct {
	ActiveSubscriptions prometheus.Gauge
	Requests            *prometheus.HistogramVec
	DeliveredMessages   prometheus.Counter
	PushedMessages      prometheus.Counter
}

var Global Metrics

func init() {
	Global = Metrics{
		ActiveSubscriptions: promauto.NewGauge(prometheus.GaugeOpts{
			Namespace: "tonconnect",
			Subsystem: "bridge",
			Name:      "active_subscriptions",
			Help:      "Active (connected) subscriptions with clients",
		}),
		Requests: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "tonconnect",
			Subsystem: "bridge",
			Name:      "requests",
			Help:      "HTTP Requests",
		}, []string{"path", "status"}),
		DeliveredMessages: promauto.NewCounter(prometheus.CounterOpts{
			Namespace: "tonconnect",
			Subsystem: "bridge",
			Name:      "delivered_messages",
			Help:      "Delivered messages",
		}),
		PushedMessages: promauto.NewCounter(prometheus.CounterOpts{
			Namespace: "tonconnect",
			Subsystem: "bridge",
			Name:      "pushed_messages",
			Help:      "Pushed messages",
		}),
	}
}
