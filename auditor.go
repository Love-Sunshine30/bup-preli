package main

import (
	"regexp"
	"strconv"
	"strings"
)

// The auditor is NOT the interpreter. The LLM is. This exists to (a) detect
// disagreement so we can ask the model again, and (b) degrade gracefully if
// the model provider is unavailable. Documented as such in the README.

var (
	reTime     = regexp.MustCompile(`(?i)\b(\d{1,2})(?::(\d{2}))?\s*(a\.?m\.?|p\.?m\.?|o'?clock)?\b`)
	rePct      = regexp.MustCompile(`(?i)(\d{1,3}(?:\.\d+)?)\s*(?:%|percent)`)
	reKwh      = regexp.MustCompile(`(?i)(\d{1,6}(?:\.\d+)?)\s*kwh`)
	reDuration = regexp.MustCompile(`(?i)\b(?:for\s+)?(\d{1,2}|one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve)\s+hours?\b`)
	wordHours  = map[string]int{"one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6,
		"seven": 7, "eight": 8, "nine": 9, "ten": 10, "eleven": 11, "twelve": 12}
	wordFrac = map[string]float64{"half": 50, "a third": 33.33, "one-third": 33.33, "a quarter": 25,
		"one-quarter": 25, "a fifth": 20, "one-fifth": 20, "three-quarters": 75}
)

type auditHit struct {
	Raw        RawInterp
	FoundType  bool
	FoundHours bool
}

func AuditNote(i int, note string) auditHit {
	low := strings.ToLower(note)
	r := RawInterp{NoteIndex: i, DirectiveType: DNoOp, StartHour24: -1, EndHour24: -1,
		ValueKind: "none", Explanation: "Rule-based fallback interpretation."}
	h := auditHit{}

	switch {
	case strings.Contains(low, "discharg"):
		r.DirectiveType = DNoDischarge
	case strings.Contains(low, "charg"):
		r.DirectiveType = DNoCharge
	case containsAny(low, "solar", "pv ", "photovoltaic", "panel", "inverter"):
		r.DirectiveType = DSolarReduction
	case containsAny(low, "at least", "reserve", "remain in the battery", "stored in the battery", "state of charge"):
		r.DirectiveType = DMinReserve
	case containsAny(low, "grid", "import", "intake", "feeder", "transformer", "substation"):
		r.DirectiveType = DMaxGrid
	}
	h.FoundType = r.DirectiveType != DNoOp
	if !h.FoundType {
		return h
	}

	start, end, ok := parseWindow(low)
	if ok {
		r.StartHour24, r.EndHour24 = start, end
		h.FoundHours = true
	}

	switch r.DirectiveType {
	case DSolarReduction:
		pct, found := parsePercent(low)
		if found {
			if containsAny(low, "reduction", "reduce", "reduced by", "drop by", "derate", "less") &&
				!containsAny(low, "drop to", "reduced to", "% of", "percent of") {
				r.ValueKind, r.Value = "solar_reduction_percent", pct
			} else {
				r.ValueKind, r.Value = "solar_remaining_percent", pct
			}
		}
	case DMinReserve:
		if v, ok := parseKwh(low); ok {
			r.ValueKind, r.Value = "reserve_kwh", v
		} else if pct, ok := parsePercent(low); ok {
			r.ValueKind, r.Value = "reserve_percent_of_capacity", pct
		}
	case DMaxGrid:
		if v, ok := parseKwh(low); ok {
			r.ValueKind, r.Value = "grid_cap_kwh", v
		}
	}
	h.Raw = r
	return h
}

func parsePercent(low string) (float64, bool) {
	if m := rePct.FindStringSubmatch(low); m != nil {
		v, _ := strconv.ParseFloat(m[1], 64)
		return v, true
	}
	for w, v := range wordFrac {
		if strings.Contains(low, w) {
			return v, true
		}
	}
	return 0, false
}

func parseKwh(low string) (float64, bool) {
	if m := reKwh.FindStringSubmatch(low); m != nil {
		v, _ := strconv.ParseFloat(m[1], 64)
		return v, true
	}
	return 0, false
}

// parseWindow returns (startHour24, endHour24) using the literal endpoints.
func parseWindow(low string) (int, int, bool) {
	clean := rePct.ReplaceAllString(low, " ")
	clean = reKwh.ReplaceAllString(clean, " ")
	// "for three hours starting at 2 AM" -> remember duration, drop the words
	dur := 0
	if m := reDuration.FindStringSubmatch(clean); m != nil {
		w := strings.ToLower(m[1])
		if v, ok := wordHours[w]; ok {
			dur = v
		} else if v, err := strconv.Atoi(w); err == nil {
			dur = v
		}
		if dur > 0 && dur <= 24 {
			clean = strings.Replace(clean, m[0], " ", 1)
		}
	}
	clean = strings.ReplaceAll(clean, "noon", "12:00 pm")
	clean = strings.ReplaceAll(clean, "midnight", "12:00 am")
	for w, v := range wordHours {
		clean = regexp.MustCompile(`\b`+w+`\b`).ReplaceAllString(clean, strconv.Itoa(v))
	}
	ms := reTime.FindAllStringSubmatch(clean, -1)
	type tm struct {
		h   int
		mer string
	}
	var ts []tm
	for _, m := range ms {
		v, err := strconv.Atoi(m[1])
		if err != nil || v > 24 {
			continue
		}
		mer := strings.ToLower(strings.TrimRight(strings.ReplaceAll(m[3], ".", ""), " "))
		if strings.HasPrefix(mer, "o") {
			mer = ""
		}
		ts = append(ts, tm{v, mer})
	}
	if len(ts) == 1 && dur > 0 {
		t := ts[0]
		h := t.h
		if t.mer == "pm" && h < 12 {
			h += 12
		}
		if t.mer == "am" && h == 12 {
			h = 0
		}
		return h % 24, (h + dur) % 24, true
	}
	if len(ts) < 2 {
		return 0, 0, false
	}
	a, b := ts[0], ts[1]
	if a.mer == "" {
		a.mer = b.mer
	}
	if b.mer == "" {
		b.mer = a.mer
	}
	conv := func(t tm) int {
		h := t.h
		if t.mer == "pm" && h < 12 {
			h += 12
		}
		if t.mer == "am" && h == 12 {
			h = 0
		}
		return h % 24
	}
	sh, eh := conv(a), conv(b)
	// "from 11 to 2" with no meridiem on an evening phrase: if the window
	// would be empty, leave it to the LLM rather than guessing.
	if sh == eh {
		return 0, 0, false
	}
	return sh, eh, true
}

func containsAny(s string, subs ...string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}

func AuditAll(notes []string) []RawInterp {
	out := make([]RawInterp, len(notes))
	for i, n := range notes {
		h := AuditNote(i, n)
		if h.FoundType && h.FoundHours {
			out[i] = h.Raw
		} else {
			out[i] = RawInterp{NoteIndex: i, DirectiveType: DNoOp, StartHour24: -1, EndHour24: -1,
				ValueKind: "none", Explanation: "This note does not affect today's 24-hour energy schedule."}
		}
	}
	return out
}
