package telemetry

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/JoaoVictorVM/comporta/internal/storage"
)

// AlertMetrics counts alert occurrences and delivery attempts and exposes the
// outbox backlog. Every label is a closed category: trigger, channel type,
// outcome and error category. No app name, channel name, user hash or URL is
// ever a label.
type AlertMetrics struct {
	occurrences       *prometheus.CounterVec
	deliveries        *prometheus.CounterVec
	storeFailures     prometheus.Counter
	pendingInputs     prometheus.Gauge
	pendingDeliveries prometheus.Gauge
	oldestDelivery    prometheus.Gauge
}

func NewAlertMetrics() *AlertMetrics {
	return &AlertMetrics{
		occurrences: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Name: "alert_occurrences_total",
			Help: "Alert occurrences created by this process, by trigger."}, []string{"trigger"}),
		deliveries: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Name: "alert_deliveries_total",
			Help: "Alert delivery attempts finished by this process, by channel type, outcome and category."}, []string{"channel_type", "outcome", "category"}),
		storeFailures: prometheus.NewCounter(prometheus.CounterOpts{Namespace: namespace, Name: "alert_store_failures_total",
			Help: "Alert store operations that failed and will be retried."}),
		pendingInputs: prometheus.NewGauge(prometheus.GaugeOpts{Namespace: namespace, Name: "alert_inputs_pending",
			Help: "Events waiting for alert evaluation. Aggregate with max in a cluster."}),
		pendingDeliveries: prometheus.NewGauge(prometheus.GaugeOpts{Namespace: namespace, Name: "alert_deliveries_pending",
			Help: "Alert deliveries not yet confirmed, failed or canceled. Aggregate with max in a cluster."}),
		oldestDelivery: prometheus.NewGauge(prometheus.GaugeOpts{Namespace: namespace, Name: "alert_delivery_oldest_seconds",
			Help: "Age of the oldest open alert delivery. Aggregate with max in a cluster."}),
	}
}

func (m *AlertMetrics) collectors() []prometheus.Collector {
	return []prometheus.Collector{m.occurrences, m.deliveries, m.storeFailures, m.pendingInputs, m.pendingDeliveries, m.oldestDelivery}
}

func (m *AlertMetrics) Describe(ch chan<- *prometheus.Desc) {
	for _, c := range m.collectors() {
		c.Describe(ch)
	}
}

func (m *AlertMetrics) Collect(ch chan<- prometheus.Metric) {
	for _, c := range m.collectors() {
		c.Collect(ch)
	}
}

func (m *AlertMetrics) AlertOccurrences(trigger string, n int) {
	m.occurrences.WithLabelValues(trigger).Add(float64(n))
}

func (m *AlertMetrics) AlertDelivery(channelType, outcome, category string) {
	m.deliveries.WithLabelValues(channelType, outcome, category).Inc()
}

func (m *AlertMetrics) AlertBacklog(b storage.AlertBacklog) {
	m.pendingInputs.Set(float64(b.PendingInputs))
	m.pendingDeliveries.Set(float64(b.PendingDeliveries))
	m.oldestDelivery.Set(b.OldestDelivery.Seconds())
}

func (m *AlertMetrics) AlertStoreFailure() { m.storeFailures.Inc() }
