package main

import (
	"math"
	"sort"
)

const bigCap = 1e7

// Model holds the per-hour bounds after directives have been applied.
type Model struct {
	EffSolar [24]float64
	MaxCh    [24]float64
	MaxDis   [24]float64
	GridCap  [24]float64 // bigCap when uncapped
	MinE     [24]float64 // required battery_energy_after_kwh floor
}

func BuildModel(s Scenario, dirs []Directive) Model {
	var m Model
	for h := 0; h < 24; h++ {
		m.EffSolar[h] = s.Solar[h]
		m.MaxCh[h] = s.MaxCh
		m.MaxDis[h] = s.MaxDis
		m.GridCap[h] = bigCap
		m.MinE[h] = s.MinE
	}
	for _, d := range dirs {
		for _, h := range d.Hours {
			if h < 0 || h > 23 {
				continue
			}
			switch d.Type {
			case DSolarReduction:
				m.EffSolar[h] = s.Solar[h] * d.Factor
			case DNoCharge:
				m.MaxCh[h] = 0
			case DNoDischarge:
				m.MaxDis[h] = 0
			case DMaxGrid:
				if d.MaxGrid < m.GridCap[h] {
					m.GridCap[h] = d.MaxGrid
				}
			case DMinReserve:
				if d.MinEnergy > m.MinE[h] {
					m.MinE[h] = d.MinEnergy
				}
			}
		}
	}
	return m
}

// Solve returns per-hour (grid, solarUsed, charge, discharge).
func Solve(s Scenario, m Model) (g, su, ch, dis [24]float64, err error) {
	// Variables: g[0..23]=0..23, s=24..47, c=48..71, d=72..95, then slacks.
	nBase := 96
	type row struct {
		coef map[int]float64
		rhs  float64
	}
	rows := []row{}
	slack := nBase

	addUB := func(varIdx int, ub float64) {
		if ub >= bigCap {
			return
		}
		r := row{coef: map[int]float64{varIdx: 1, slack: 1}, rhs: ub}
		slack++
		rows = append(rows, r)
	}

	for h := 0; h < 24; h++ {
		// balance: g + s + d - c = demand
		rows = append(rows, row{coef: map[int]float64{h: 1, 24 + h: 1, 72 + h: 1, 48 + h: -1}, rhs: s.Demand[h]})
	}
	// neutrality: sum(c - d) = 0
	neu := map[int]float64{}
	for h := 0; h < 24; h++ {
		neu[48+h] = 1
		neu[72+h] = -1
	}
	rows = append(rows, row{coef: neu, rhs: 0})

	for h := 0; h < 24; h++ {
		addUB(h, m.GridCap[h])
		addUB(24+h, m.EffSolar[h])
		addUB(48+h, m.MaxCh[h])
		addUB(72+h, m.MaxDis[h])
	}
	// SOC: E0 + cum(c-d) in [MinE[h], capacity]
	for h := 0; h < 24; h++ {
		up := map[int]float64{}
		lo := map[int]float64{}
		for k := 0; k <= h; k++ {
			up[48+k] = 1
			up[72+k] = -1
			lo[48+k] = 1
			lo[72+k] = -1
		}
		up[slack] = 1
		slack++
		rows = append(rows, row{coef: up, rhs: s.Capacity - s.E0})
		lo[slack] = -1
		slack++
		rows = append(rows, row{coef: lo, rhs: m.MinE[h] - s.E0})
	}

	n := slack
	A := make([][]float64, len(rows))
	b := make([]float64, len(rows))
	for i, r := range rows {
		a := make([]float64, n)
		for j, v := range r.coef {
			a[j] = v
		}
		A[i] = a
		b[i] = r.rhs
	}
	c := make([]float64, n)
	for h := 0; h < 24; h++ {
		c[h] = s.Tariff[h]
		c[24+h] = 0 // solar is free; grid cost alone drives the objective
	}

	x, _, e := SolveLP(c, A, b)
	if e != nil {
		return g, su, ch, dis, e
	}
	for h := 0; h < 24; h++ {
		g[h] = math.Max(0, x[h])
		su[h] = math.Max(0, x[24+h])
		ch[h] = math.Max(0, x[48+h])
		dis[h] = math.Max(0, x[72+h])
	}
	return g, su, ch, dis, nil
}

// SolveWithFallback tries the full directive set, then progressively drops
// directives if the interpretation produced an infeasible model. Physical
// constraints are never relaxed, so the returned plan is always valid.
func SolveWithFallback(s Scenario, dirs []Directive) (Model, [24]float64, [24]float64, [24]float64, [24]float64, []Directive, error) {
	applied := []Directive{}
	for _, d := range dirs {
		if d.Type != DNoOp && len(d.Hours) > 0 {
			applied = append(applied, d)
		}
	}
	subsets := subsetsByPreference(len(applied))
	for _, keep := range subsets {
		sel := []Directive{}
		for _, i := range keep {
			sel = append(sel, applied[i])
		}
		m := BuildModel(s, sel)
		g, su, ch, dis, err := Solve(s, m)
		if err == nil {
			return m, g, su, ch, dis, sel, nil
		}
	}
	m := BuildModel(s, nil)
	g, su, ch, dis, err := Solve(s, m)
	return m, g, su, ch, dis, nil, err
}

// subsetsByPreference lists index subsets of {0..n-1} largest-first.
func subsetsByPreference(n int) [][]int {
	var all [][]int
	for mask := 0; mask < (1 << n); mask++ {
		var idx []int
		for i := 0; i < n; i++ {
			if mask&(1<<i) != 0 {
				idx = append(idx, i)
			}
		}
		all = append(all, idx)
	}
	sort.SliceStable(all, func(i, j int) bool { return len(all[i]) > len(all[j]) })
	return all
}
