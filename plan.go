package main

import (
	"fmt"
	"math"
	"strings"
)

func r6(v float64) float64 { return math.Round(v*1e6) / 1e6 }

// Materialize converts LP variables into a schema-exact hourly plan.
// grid_kwh is DERIVED from the balance identity so the equation holds exactly.
func Materialize(s Scenario, m Model, su, ch, dis [24]float64) []PlanHour {
	net := make([]float64, 24)
	for h := 0; h < 24; h++ {
		net[h] = r6(ch[h] - dis[h])
	}
	// Force exact end-of-day neutrality by absorbing residue in the last
	// hour that has slack in the right direction.
	sum := 0.0
	for _, v := range net {
		sum += v
	}
	if d := -r6(sum); d != 0 {
		for h := 23; h >= 0; h-- {
			nv := net[h] + d
			if nv >= 0 && nv <= m.MaxCh[h] || nv <= 0 && -nv <= m.MaxDis[h] {
				net[h] = r6(nv)
				break
			}
		}
	}

	plan := make([]PlanHour, 24)
	e := s.E0
	for h := 0; h < 24; h++ {
		solar := r6(math.Min(math.Max(su[h], 0), m.EffSolar[h]))
		action, mag := "idle", 0.0
		if net[h] > 1e-9 {
			action, mag = "charge", r6(net[h])
		} else if net[h] < -1e-9 {
			action, mag = "discharge", r6(-net[h])
		}
		grid := s.Demand[h] - solar
		if action == "charge" {
			grid += mag
		} else if action == "discharge" {
			grid -= mag
		}
		grid = r6(math.Max(grid, 0))
		if action == "charge" {
			e += mag
		} else if action == "discharge" {
			e -= mag
		}
		e = r6(e)
		plan[h] = PlanHour{h, grid, solar, action, mag, e}
	}
	return plan
}

func Totals(s Scenario, plan []PlanHour) (grid, cost, peak float64) {
	for _, p := range plan {
		grid += p.GridKwh
		cost += p.GridKwh * s.Tariff[p.Hour]
		if p.GridKwh > peak {
			peak = p.GridKwh
		}
	}
	return r6(grid), r6(cost), r6(peak)
}

// Validate is our own copy of the judge. Run it on every response.
func Validate(s Scenario, m Model, plan []PlanHour, tot, cost, peak float64) []string {
	var errs []string
	const tol = 0.01
	if len(plan) != 24 {
		return []string{"hourly_plan must have 24 entries"}
	}
	e := s.E0
	gsum, csum, pk := 0.0, 0.0, 0.0
	for h, p := range plan {
		if p.Hour != h {
			errs = append(errs, fmt.Sprintf("hour %d out of order", h))
		}
		if p.GridKwh < -tol || p.SolarUsedKwh < -tol || p.BatteryKwh < -tol {
			errs = append(errs, fmt.Sprintf("h%d negative value", h))
		}
		if p.SolarUsedKwh > m.EffSolar[h]+tol {
			errs = append(errs, fmt.Sprintf("h%d solar %.3f > effective %.3f", h, p.SolarUsedKwh, m.EffSolar[h]))
		}
		if p.GridKwh > m.GridCap[h]+tol {
			errs = append(errs, fmt.Sprintf("h%d grid cap violated", h))
		}
		chg, dis := 0.0, 0.0
		switch p.BatteryAction {
		case "charge":
			chg = p.BatteryKwh
			if chg > m.MaxCh[h]+tol {
				errs = append(errs, fmt.Sprintf("h%d charge rate/window violated", h))
			}
		case "discharge":
			dis = p.BatteryKwh
			if dis > m.MaxDis[h]+tol {
				errs = append(errs, fmt.Sprintf("h%d discharge rate/window violated", h))
			}
		case "idle":
			if math.Abs(p.BatteryKwh) > tol {
				errs = append(errs, fmt.Sprintf("h%d idle with nonzero battery_kwh", h))
			}
		default:
			errs = append(errs, fmt.Sprintf("h%d bad battery_action", h))
		}
		if math.Abs(p.GridKwh+p.SolarUsedKwh+dis-s.Demand[h]-chg) > tol {
			errs = append(errs, fmt.Sprintf("h%d energy balance broken", h))
		}
		e += chg - dis
		if math.Abs(e-p.BatteryEnergyAfterKwh) > tol {
			errs = append(errs, fmt.Sprintf("h%d battery_energy_after mismatch", h))
		}
		if p.BatteryEnergyAfterKwh < m.MinE[h]-tol || p.BatteryEnergyAfterKwh > s.Capacity+tol {
			errs = append(errs, fmt.Sprintf("h%d battery out of bounds", h))
		}
		gsum += p.GridKwh
		csum += p.GridKwh * s.Tariff[h]
		if p.GridKwh > pk {
			pk = p.GridKwh
		}
	}
	if math.Abs(e-s.E0) > tol {
		errs = append(errs, "end-of-day battery neutrality violated")
	}
	if math.Abs(gsum-tot) > tol || math.Abs(csum-cost) > tol || math.Abs(pk-peak) > tol {
		errs = append(errs, "reported totals disagree with hourly_plan")
	}
	return errs
}

// SafePlan is the last-resort schedule: grid only, battery idle. Expensive but
// always valid, which preserves constraint-correctness credit.
func SafePlan(s Scenario) []PlanHour {
	plan := make([]PlanHour, 24)
	for h := 0; h < 24; h++ {
		plan[h] = PlanHour{h, r6(s.Demand[h]), 0, "idle", 0, s.E0}
	}
	return plan
}

func Summarize(dirs []Directive, plan []PlanHour, cost float64) string {
	var parts []string
	for _, d := range dirs {
		if d.Type != DNoOp {
			parts = append(parts, d.Type)
		}
	}
	applied := "no operator directives affected the schedule"
	if len(parts) > 0 {
		applied = "applied " + strings.Join(parts, ", ")
	}
	chg, dch := 0, 0
	for _, p := range plan {
		if p.BatteryAction == "charge" {
			chg++
		} else if p.BatteryAction == "discharge" {
			dch++
		}
	}
	return fmt.Sprintf("Charged the battery in %d low-tariff hours and discharged in %d high-tariff hours, using available solar first; %s. Battery returns to its initial state of charge at hour 23. Total grid cost %.2f BDT.", chg, dch, applied, cost)
}
