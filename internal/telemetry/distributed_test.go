package telemetry

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestDistributedMetricsNoIdentityLabels(t *testing.T) {
	registry := prometheus.NewRegistry()
	registry.MustRegister(NewClusterCollector(func(context.Context) ClusterHealth {
		return ClusterHealth{PostgresUp: true, Mode: "degraded", OutboxAge: 3 * time.Second, OutboxPending: 2, Ready: true}
	}))
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"backend": true, "mode": true}
	for _, f := range families {
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if !allowed[l.GetName()] {
					t.Fatalf("%s carries label %s", f.GetName(), l.GetName())
				}
			}
		}
	}
	modes := map[string]float64{}
	for _, f := range families {
		if f.GetName() != "comporta_cluster_mode" {
			continue
		}
		for _, m := range f.GetMetric() {
			modes[m.GetLabel()[0].GetValue()] = m.GetGauge().GetValue()
		}
	}
	if modes["degraded"] != 1 || modes["normal"] != 0 || modes["recovering"] != 0 {
		t.Fatalf("cluster mode gauges: %v", modes)
	}
}
