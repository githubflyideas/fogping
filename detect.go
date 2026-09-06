package main

import (
	"sort"
	"time"
)

// Detection in fogping 2.0 serves exactly one master: the ◆ marks on the chart.
// No alerting, no state machine, no thresholds to tune. robust z-score
// (median + MAD) flags loss bursts; the chart shows them; humans decide.
type Detector struct {
	store *Store
}

func NewDetector(store *Store) *Detector { return &Detector{store: store} }

const burstZ = 3.5 // Iglewicz-Hoaglin conventional cutoff

// CheckBurst returns the verdict and the z value (hover-tooltip evidence).
func (d *Detector) CheckBurst(name string, r Round) (bool, float64) {
	loss := r.S - r.R
	if loss < 2 { // a single lost packet on the internet is noise
		return false, 0
	}
	hist := d.store.Recent(name, time.Now().Add(-4*time.Hour).Unix())
	if len(hist) < 30 {
		return float64(loss)/float64(r.S) >= 0.25, 0 // cold start: absolute floor
	}
	series := make([]float64, len(hist))
	for i, h := range hist {
		series[i] = float64(h.S - h.R)
	}
	z := robustZ(float64(loss), series)
	if z < 0 { // MAD=0: the healthy all-zero-loss baseline
		return float64(loss)/float64(r.S) >= 0.10, 0
	}
	return z >= burstZ, float64(int(z*100)) / 100
}

// robustZ = 0.6745*(x-median)/MAD; returns -1 when MAD==0 so callers fall back.
func robustZ(x float64, series []float64) float64 {
	med := median(series)
	dev := make([]float64, len(series))
	for i, v := range series {
		if v > med {
			dev[i] = v - med
		} else {
			dev[i] = med - v
		}
	}
	mad := median(dev)
	if mad == 0 {
		return -1
	}
	return 0.6745 * (x - med) / mad
}

func median(s []float64) float64 {
	if len(s) == 0 {
		return 0
	}
	c := append([]float64(nil), s...)
	sort.Float64s(c)
	n := len(c)
	if n%2 == 1 {
		return c[n/2]
	}
	return (c[n/2-1] + c[n/2]) / 2
}
