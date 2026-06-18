package api

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/ethpandaops/coredevs/internal/index"
	"github.com/ethpandaops/coredevs/internal/syncer"
)

// collector exposes coredevs state as Prometheus metrics, read at scrape time
// so the sync path stays free of metric bookkeeping.
type collector struct {
	store  *index.Store
	syncer *syncer.Syncer

	teamMembers    *prometheus.Desc
	indexGenerated *prometheus.Desc
	sourceMembers  *prometheus.Desc
	sourceSuccess  *prometheus.Desc
}

var _ prometheus.Collector = (*collector)(nil)

func newCollector(store *index.Store, sync *syncer.Syncer) *collector {
	return &collector{
		store:  store,
		syncer: sync,
		teamMembers: prometheus.NewDesc(
			"coredevs_team_members",
			"Number of members in a team, by source.",
			[]string{"team", "source"}, nil,
		),
		indexGenerated: prometheus.NewDesc(
			"coredevs_index_generated_timestamp_seconds",
			"Unix timestamp when the current index was generated.",
			nil, nil,
		),
		sourceMembers: prometheus.NewDesc(
			"coredevs_source_members",
			"Membership count from a source's last successful fetch.",
			[]string{"source"}, nil,
		),
		sourceSuccess: prometheus.NewDesc(
			"coredevs_source_last_success_timestamp_seconds",
			"Unix timestamp of a source's last successful fetch.",
			[]string{"source"}, nil,
		),
	}
}

// Describe implements prometheus.Collector.
func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.teamMembers
	ch <- c.indexGenerated
	ch <- c.sourceMembers
	ch <- c.sourceSuccess
}

// Collect implements prometheus.Collector.
func (c *collector) Collect(ch chan<- prometheus.Metric) {
	if idx := c.store.Get(); idx != nil {
		ch <- prometheus.MustNewConstMetric(
			c.indexGenerated, prometheus.GaugeValue, float64(idx.GeneratedAt.Unix()),
		)

		for _, slug := range idx.TeamSlugs() {
			for src, count := range countBySource(idx.Members(slug, "")) {
				ch <- prometheus.MustNewConstMetric(
					c.teamMembers, prometheus.GaugeValue, float64(count), slug, src,
				)
			}
		}
	}

	for _, st := range c.syncer.Statuses() {
		ch <- prometheus.MustNewConstMetric(
			c.sourceMembers, prometheus.GaugeValue, float64(st.Members), st.Name,
		)

		if !st.LastSuccess.IsZero() {
			ch <- prometheus.MustNewConstMetric(
				c.sourceSuccess, prometheus.GaugeValue, float64(st.LastSuccess.Unix()), st.Name,
			)
		}
	}
}
