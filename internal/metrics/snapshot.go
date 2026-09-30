package metrics

import (
	"fmt"
	"sort"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// Sample is one time series read out of the registry at a point in time.
//
// Counters and gauges carry their value in Value. Histograms carry their
// observation count and total in Count and Sum, and leave Value at zero —
// there is no single "value" for a histogram, and the UI shows a mean derived
// from Sum/Count rather than pretending otherwise.
type Sample struct {
	Labels map[string]string
	Value  float64
	Count  uint64
	Sum    float64
}

// Label returns the value of one label, or "" if absent.
func (s Sample) Label(name string) string {
	return s.Labels[name]
}

// Mean returns the average observation of a histogram sample, or 0 when
// nothing has been observed yet.
func (s Sample) Mean() float64 {
	if s.Count == 0 {
		return 0
	}
	return s.Sum / float64(s.Count)
}

// Snapshot is the value of every registered proxy metric at one instant.
//
// These are process-lifetime figures held in the Prometheus registry, not
// database state: they start at zero when the proxy starts and are lost on
// restart. Anything that needs to survive a restart, or needs history, has to
// come from a Prometheus server scraping /metrics.
type Snapshot struct {
	families map[string][]Sample
}

// Gather reads the default Prometheus registry. It is the same data /metrics
// serves, shaped for rendering rather than for scraping.
func Gather() (*Snapshot, error) {
	return GatherFrom(prometheus.DefaultGatherer)
}

// GatherFrom reads an explicit gatherer, so tests can supply their own registry.
func GatherFrom(g prometheus.Gatherer) (*Snapshot, error) {
	families, err := g.Gather()
	if err != nil {
		return nil, fmt.Errorf("gathering metrics: %w", err)
	}

	snap := &Snapshot{families: make(map[string][]Sample, len(families))}
	for _, mf := range families {
		samples := make([]Sample, 0, len(mf.GetMetric()))
		for _, m := range mf.GetMetric() {
			samples = append(samples, sampleOf(m))
		}
		snap.families[mf.GetName()] = samples
	}
	return snap, nil
}

func sampleOf(m *dto.Metric) Sample {
	s := Sample{Labels: make(map[string]string, len(m.GetLabel()))}
	for _, lp := range m.GetLabel() {
		s.Labels[lp.GetName()] = lp.GetValue()
	}

	switch {
	case m.GetCounter() != nil:
		s.Value = m.GetCounter().GetValue()
	case m.GetGauge() != nil:
		s.Value = m.GetGauge().GetValue()
	case m.GetHistogram() != nil:
		h := m.GetHistogram()
		s.Count = h.GetSampleCount()
		s.Sum = h.GetSampleSum()
	case m.GetSummary() != nil:
		sm := m.GetSummary()
		s.Count = sm.GetSampleCount()
		s.Sum = sm.GetSampleSum()
	case m.GetUntyped() != nil:
		s.Value = m.GetUntyped().GetValue()
	}
	return s
}

// Names returns the name of every metric family in the snapshot, sorted.
func (s *Snapshot) Names() []string {
	if s == nil {
		return nil
	}

	names := make([]string, 0, len(s.families))
	for name := range s.families {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Samples returns every series of a metric, or nil when it has never been
// observed. Series are ordered by their label values so that rendering is
// stable between scrapes.
func (s *Snapshot) Samples(name string) []Sample {
	if s == nil {
		return nil
	}
	out := append([]Sample(nil), s.families[name]...)
	sort.Slice(out, func(i, j int) bool {
		return labelKey(out[i].Labels) < labelKey(out[j].Labels)
	})
	return out
}

func labelKey(labels map[string]string) string {
	names := make([]string, 0, len(labels))
	for n := range labels {
		names = append(names, n)
	}
	sort.Strings(names)

	key := ""
	for _, n := range names {
		key += n + "=" + labels[n] + ","
	}
	return key
}

// Sum totals every series of a counter or gauge.
func (s *Snapshot) Sum(name string) float64 {
	var total float64
	for _, sample := range s.Samples(name) {
		total += sample.Value
	}
	return total
}

// Count adds up the observation counts of a histogram across every series.
func (s *Snapshot) Count(name string) uint64 {
	var total uint64
	for _, sample := range s.Samples(name) {
		total += sample.Count
	}
	return total
}

// Mean returns the average observation of a histogram across every series, or
// 0 when nothing has been observed.
func (s *Snapshot) Mean(name string) float64 {
	var count uint64
	var sum float64
	for _, sample := range s.Samples(name) {
		count += sample.Count
		sum += sample.Sum
	}
	if count == 0 {
		return 0
	}
	return sum / float64(count)
}

// SumBy groups a metric by one label and totals each group.
func (s *Snapshot) SumBy(name, label string) map[string]float64 {
	out := make(map[string]float64)
	for _, sample := range s.Samples(name) {
		out[sample.Label(label)] += sample.Value
	}
	return out
}
