package api

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/ethpandaops/coredevs/internal/index"
)

// collector exposes coredevs state as Prometheus metrics, read at scrape time
// so the sync path stays free of metric bookkeeping.
type collector struct {
	store    *index.Store
	statuses StatusFunc
	keys     KeyProvider

	teamMembers    *prometheus.Desc
	indexGenerated *prometheus.Desc
	sourceMembers  *prometheus.Desc
	sourceTeams    *prometheus.Desc
	sourceWarnings *prometheus.Desc
	sourceSuccess  *prometheus.Desc
	keysHandles    *prometheus.Desc
	keysCached     *prometheus.Desc
	keysErrors     *prometheus.Desc
	keysLastCycle  *prometheus.Desc
}

var _ prometheus.Collector = (*collector)(nil)

func newCollector(store *index.Store, statuses StatusFunc, keyProvider KeyProvider) *collector {
	return &collector{
		store:    store,
		statuses: statuses,
		keys:     keyProvider,
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
		sourceTeams: prometheus.NewDesc(
			"coredevs_source_teams",
			"Number of distinct teams in a source's last successful fetch.",
			[]string{"source"}, nil,
		),
		sourceWarnings: prometheus.NewDesc(
			"coredevs_source_warnings",
			"Number of non-fatal problems a source reported about its last fetch, such as a configured upstream section that no longer exists.",
			[]string{"source"}, nil,
		),
		sourceSuccess: prometheus.NewDesc(
			"coredevs_source_last_success_timestamp_seconds",
			"Unix timestamp of a source's last successful fetch.",
			[]string{"source"}, nil,
		),
		keysHandles: prometheus.NewDesc(
			"coredevs_keys_handles",
			"Number of handles tracked by the key cache.",
			nil, nil,
		),
		keysCached: prometheus.NewDesc(
			"coredevs_keys_cached",
			"Number of handles with at least one successfully fetched key.",
			nil, nil,
		),
		keysErrors: prometheus.NewDesc(
			"coredevs_keys_errors",
			"Number of handles whose most recent key fetch failed.",
			nil, nil,
		),
		keysLastCycle: prometheus.NewDesc(
			"coredevs_keys_last_cycle_timestamp_seconds",
			"Unix timestamp when the key cache last completed a full refresh pass.",
			nil, nil,
		),
	}
}

// Describe implements prometheus.Collector.
func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.teamMembers
	ch <- c.indexGenerated
	ch <- c.sourceMembers
	ch <- c.sourceTeams
	ch <- c.sourceWarnings
	ch <- c.sourceSuccess
	ch <- c.keysHandles
	ch <- c.keysCached
	ch <- c.keysErrors
	ch <- c.keysLastCycle
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

	for _, st := range c.statuses() {
		ch <- prometheus.MustNewConstMetric(
			c.sourceMembers, prometheus.GaugeValue, float64(st.Members), st.Name,
		)

		ch <- prometheus.MustNewConstMetric(
			c.sourceTeams, prometheus.GaugeValue, float64(st.Teams), st.Name,
		)

		ch <- prometheus.MustNewConstMetric(
			c.sourceWarnings, prometheus.GaugeValue, float64(len(st.Warnings)), st.Name,
		)

		if !st.LastSuccess.IsZero() {
			ch <- prometheus.MustNewConstMetric(
				c.sourceSuccess, prometheus.GaugeValue, float64(st.LastSuccess.Unix()), st.Name,
			)
		}
	}

	if c.keys != nil {
		st := c.keys.Status()

		ch <- prometheus.MustNewConstMetric(c.keysHandles, prometheus.GaugeValue, float64(st.Handles))
		ch <- prometheus.MustNewConstMetric(c.keysCached, prometheus.GaugeValue, float64(st.Cached))
		ch <- prometheus.MustNewConstMetric(c.keysErrors, prometheus.GaugeValue, float64(st.Errors))

		if !st.LastCycle.IsZero() {
			ch <- prometheus.MustNewConstMetric(
				c.keysLastCycle, prometheus.GaugeValue, float64(st.LastCycle.Unix()),
			)
		}
	}
}
