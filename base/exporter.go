package base

import (
	"expvar"

	"github.com/prometheus/client_golang/prometheus"
)

type Collector struct {
	Info   map[string]StatComponents
	VarMap *expvar.Map
}

type StatComponents struct {
	ValueType prometheus.ValueType
}

func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	return
}

func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.VarMap.Do(func(value expvar.KeyValue) {
		name := value.Key
		vType := c.Info[name].ValueType
		desc := prometheus.NewDesc(name, name, nil, nil)

		if _, ok := c.Info[name]; ok {
			switch v := value.Value.(type) {
			case *expvar.Int:
				ch <- prometheus.MustNewConstMetric(desc, vType, float64(v.Value()))
				break
			case *expvar.Float:
				ch <- prometheus.MustNewConstMetric(desc, vType, v.Value())
				break
			}
		}

	})
}