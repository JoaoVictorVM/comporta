package telemetry

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// ClusterHealth is the state of a distributed deployment as one replica sees
// it. It carries no identity: no request, subject, pod, DSN or message.
type ClusterHealth struct {
	PostgresUp     bool
	RedisUp        bool
	Mode           string
	OutboxAge      time.Duration
	OutboxPending  int64
	LocalBacklog   int
	Ready          bool
	RecoveryActive bool
}

// ClusterProbe reads the health on every scrape, within the scrape deadline.
type ClusterProbe func(ctx context.Context) ClusterHealth

// NewClusterCollector exposes the health of the shared stores. Every gauge is
// the same on every replica except the local backlog: aggregate the cluster
// gauges with max, never sum.
func NewClusterCollector(probe ClusterProbe) prometheus.Collector {
	return clusterCollector{probe: probe}
}

type clusterCollector struct{ probe ClusterProbe }

var (
	backendUp      = prometheus.NewDesc("comporta_backend_up", "Whether this replica reaches the shared store (1) or not (0).", []string{"backend"}, nil)
	clusterMode    = prometheus.NewDesc("comporta_cluster_mode", "Cluster mode as last read: 1 for the mode in force. Aggregate with max.", []string{"mode"}, nil)
	outboxAge      = prometheus.NewDesc("comporta_outbox_oldest_seconds", "Age of the oldest settlement not yet projected in Redis. Aggregate with max.", nil, nil)
	outboxPending  = prometheus.NewDesc("comporta_outbox_pending", "Settlements committed but not yet projected in Redis. Aggregate with max.", nil, nil)
	localBacklog   = prometheus.NewDesc("comporta_settlement_retries", "Settlements and slot releases of this replica waiting for a retry.", nil, nil)
	replicaReady   = prometheus.NewDesc("comporta_replica_ready", "Whether this replica reports itself ready.", nil, nil)
	clusterModeSet = []string{"normal", "degraded", "recovering"}
)

func (clusterCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{backendUp, clusterMode, outboxAge, outboxPending, localBacklog, replicaReady} {
		ch <- d
	}
}

func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func (c clusterCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h := c.probe(ctx)
	ch <- prometheus.MustNewConstMetric(backendUp, prometheus.GaugeValue, boolGauge(h.PostgresUp), "postgres")
	ch <- prometheus.MustNewConstMetric(backendUp, prometheus.GaugeValue, boolGauge(h.RedisUp), "redis")
	for _, mode := range clusterModeSet {
		ch <- prometheus.MustNewConstMetric(clusterMode, prometheus.GaugeValue, boolGauge(h.Mode == mode), mode)
	}
	ch <- prometheus.MustNewConstMetric(outboxAge, prometheus.GaugeValue, h.OutboxAge.Seconds())
	ch <- prometheus.MustNewConstMetric(outboxPending, prometheus.GaugeValue, float64(h.OutboxPending))
	ch <- prometheus.MustNewConstMetric(localBacklog, prometheus.GaugeValue, float64(h.LocalBacklog))
	ch <- prometheus.MustNewConstMetric(replicaReady, prometheus.GaugeValue, boolGauge(h.Ready))
}
