package main

import (
	"fmt"
	"math"
	"sort"
)

// RawInterp is the loose shape the LLM is asked to emit. The model reports
// *semantics*; all arithmetic and convention handling happens here.
type RawInterp struct {
	NoteIndex     int     `json:"note_index"`
	DirectiveType string  `json:"directive_type"`
	StartHour24   int     `json:"start_hour_24"` // -1 when not applicable
	EndHour24     int     `json:"end_hour_24"`   // -1 when not applicable
	EndInclusive  bool    `json:"end_inclusive"`
	Value         float64 `json:"value"`
	ValueKind     string  `json:"value_kind"`
	Explanation   string  `json:"explanation"`
}

// ExpandHours applies the start-inclusive / end-exclusive convention.
func ExpandHours(start, end int, inclusive bool) []int {
	if start < 0 || end < 0 {
		return nil
	}
	start = ((start % 24) + 24) % 24
	if end == 24 {
		end = 0
	}
	end = ((end % 24) + 24) % 24
	if inclusive {
		end = (end + 1) % 24
	}
	var out []int
	if end == start {
		out = []int{start} // degenerate single-hour window
	} else {
		for h := start; h != end; h = (h + 1) % 24 {
			out = append(out, h)
			if len(out) > 24 {
				break
			}
		}
	}
	seen := map[int]bool{}
	var uniq []int
	for _, h := range out {
		if h >= 0 && h <= 23 && !seen[h] {
			seen[h] = true
			uniq = append(uniq, h)
		}
	}
	sort.Ints(uniq)
	return uniq
}

// Normalize converts one raw entry into a canonical Directive, or returns an
// error which the caller treats as a guardrail failure.
func Normalize(r RawInterp, s Scenario) (Directive, error) {
	d := Directive{NoteIndex: r.NoteIndex, Type: r.DirectiveType, Explanation: r.Explanation}
	if !allowedTypes[d.Type] {
		return d, fmt.Errorf("unsupported directive_type %q", d.Type)
	}
	if d.Type == DNoOp {
		d.Explanation = pick(d.Explanation, "This note does not affect today's 24-hour energy schedule.")
		return d, nil
	}
	d.Hours = ExpandHours(r.StartHour24, r.EndHour24, r.EndInclusive)
	if len(d.Hours) == 0 {
		return d, fmt.Errorf("%s produced no valid hours", d.Type)
	}

	v := r.Value
	switch d.Type {
	case DSolarReduction:
		switch r.ValueKind {
		case "solar_reduction_percent":
			d.Factor = 1 - v/100
		case "solar_remaining_percent":
			d.Factor = v / 100
		case "solar_remaining_fraction":
			d.Factor = v
		default:
			// Heuristic rescue: a bare number >1 is almost certainly a percent
			// of remaining output; <=1 is already a fraction.
			if v > 1 {
				d.Factor = v / 100
			} else {
				d.Factor = v
			}
		}
		if math.IsNaN(d.Factor) || math.IsInf(d.Factor, 0) {
			return d, fmt.Errorf("solar factor not finite")
		}
		d.Factor = clamp(round4(d.Factor), 0, 1)
	case DMinReserve:
		if r.ValueKind == "reserve_percent_of_capacity" {
			v = v / 100 * s.Capacity
		}
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return d, fmt.Errorf("reserve not finite/non-negative")
		}
		d.MinEnergy = clamp(round4(v), 0, s.Capacity)
	case DMaxGrid:
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return d, fmt.Errorf("max_grid_kwh not finite/non-negative")
		}
		d.MaxGrid = round4(v)
	}
	d.Explanation = pick(d.Explanation, "Interpreted operator note as "+d.Type+".")
	return d, nil
}

// GuardDirectives enforces the Section 08 guardrails over the whole set:
// exactly one entry per note, in index order, with valid shapes.
func GuardDirectives(ds []Directive, n int, s Scenario) error {
	if len(ds) != n {
		return fmt.Errorf("expected %d interpretations, got %d", n, len(ds))
	}
	for i, d := range ds {
		if d.NoteIndex != i {
			return fmt.Errorf("note_index %d out of order at position %d", d.NoteIndex, i)
		}
		if !allowedTypes[d.Type] {
			return fmt.Errorf("unsupported type %q", d.Type)
		}
		if d.Type == DNoOp {
			continue
		}
		if len(d.Hours) == 0 {
			return fmt.Errorf("note %d: empty hours", i)
		}
		for k, h := range d.Hours {
			if h < 0 || h > 23 {
				return fmt.Errorf("note %d: hour %d out of range", i, h)
			}
			if k > 0 && h <= d.Hours[k-1] {
				return fmt.Errorf("note %d: hours not strictly ascending", i)
			}
		}
		if d.Type == DSolarReduction && (d.Factor < 0 || d.Factor > 1) {
			return fmt.Errorf("note %d: factor out of [0,1]", i)
		}
		if d.Type == DMinReserve && (d.MinEnergy < 0 || d.MinEnergy > s.Capacity) {
			return fmt.Errorf("note %d: reserve out of range", i)
		}
		if d.Type == DMaxGrid && d.MaxGrid < 0 {
			return fmt.Errorf("note %d: negative grid cap", i)
		}
	}
	return nil
}

func clamp(v, lo, hi float64) float64 {
	return math.Max(lo, math.Min(hi, v))
}
func round4(v float64) float64 { return math.Round(v*1e4) / 1e4 }
func pick(a, b string) string {
	if a == "" {
		return b
	}
	return a
}
