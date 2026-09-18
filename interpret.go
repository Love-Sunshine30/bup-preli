package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------- configuration ----------------

type Config struct {
	APIKey     string
	Model      string
	Endpoint   string
	Timeout    time.Duration
	Adjudicate bool
}

func LoadConfig() Config {
	c := Config{
		APIKey:   os.Getenv("GEMINI_API_KEY"),
		Model:    envOr("GEMINI_MODEL", "gemini-3.6-flash"),
		Endpoint: envOr("GEMINI_ENDPOINT", "https://generativelanguage.googleapis.com/v1beta"),
		Timeout:  time.Duration(envInt("LLM_TIMEOUT_MS", 12000)) * time.Millisecond,
	}
	c.Adjudicate = envOr("ENABLE_ADJUDICATION", "true") == "true"
	return c
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func envInt(k string, d int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return d
}

// ---------------- Gemini wire types ----------------

type gPart struct {
	Text string `json:"text"`
}
type gContent struct {
	Role  string  `json:"role,omitempty"`
	Parts []gPart `json:"parts"`
}
type gGenCfg struct {
	Temperature      float64     `json:"temperature"`
	ResponseMimeType string      `json:"responseMimeType"`
	ResponseSchema   interface{} `json:"responseSchema"`
	ThinkingConfig   interface{} `json:"thinkingConfig,omitempty"`
	MaxOutputTokens  int         `json:"maxOutputTokens"`
}
type gReq struct {
	SystemInstruction *gContent  `json:"system_instruction,omitempty"`
	Contents          []gContent `json:"contents"`
	GenerationConfig  gGenCfg    `json:"generationConfig"`
}
type gResp struct {
	Candidates []struct {
		Content gContent `json:"content"`
	} `json:"candidates"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// responseSchema forces the model into our exact shape; it cannot emit a
// seventh directive type or a missing field.
func responseSchema() interface{} {
	item := map[string]interface{}{
		"type": "OBJECT",
		"properties": map[string]interface{}{
			"note_index":     map[string]interface{}{"type": "INTEGER"},
			"directive_type": map[string]interface{}{"type": "STRING", "enum": []string{DSolarReduction, DMinReserve, DNoCharge, DNoDischarge, DMaxGrid, DNoOp}},
			"start_hour_24":  map[string]interface{}{"type": "INTEGER"},
			"end_hour_24":    map[string]interface{}{"type": "INTEGER"},
			"end_inclusive":  map[string]interface{}{"type": "BOOLEAN"},
			"value":          map[string]interface{}{"type": "NUMBER"},
			"value_kind": map[string]interface{}{"type": "STRING", "enum": []string{
				"none", "solar_reduction_percent", "solar_remaining_percent", "solar_remaining_fraction",
				"reserve_kwh", "reserve_percent_of_capacity", "grid_cap_kwh"}},
			"explanation": map[string]interface{}{"type": "STRING"},
		},
		"required":         []string{"note_index", "directive_type", "start_hour_24", "end_hour_24", "end_inclusive", "value", "value_kind", "explanation"},
		"propertyOrdering": []string{"note_index", "directive_type", "start_hour_24", "end_hour_24", "end_inclusive", "value", "value_kind", "explanation"},
	}
	return map[string]interface{}{
		"type":       "OBJECT",
		"properties": map[string]interface{}{"interpretations": map[string]interface{}{"type": "ARRAY", "items": item}},
		"required":   []string{"interpretations"},
	}
}

const systemPrompt = `You convert short campus-operator notes into structured energy directives for a 24-hour scheduling system. Return one entry per note, in note_index order starting at 0.

directive_type must be exactly one of:
- solar_reduction: usable rooftop solar / PV output is reduced during a time window.
- minimum_battery_reserve: a minimum amount of energy must stay in the battery during a window.
- no_charge_window: the battery cannot be charged during a window.
- no_discharge_window: the battery cannot be discharged during a window.
- max_grid_window: grid import / intake / feeder / transformer is capped during a window.
- no_op: the note has nothing to do with today's electricity schedule (events, bookings, menus, deadlines, notices, staffing, or anything not about power).

TIME: report the LITERAL clock endpoints of the window in 24-hour form in start_hour_24 and end_hour_24. Do NOT expand them into a list and do NOT shift them. "noon" is 12, "midnight" is 0 (use 24 for an end at midnight). Set end_inclusive=false for normal phrasings such as "from X to Y", "from X until Y", "between X and Y". Set end_inclusive=true only if the note explicitly means the final hour is included, e.g. "through 8 PM inclusive". If the note gives a start and a duration ("offline for three hours starting at 2 AM"), compute the end as start+duration and set end_inclusive=false.

VALUES:
- solar_reduction: if the note says output DROPS BY / IS REDUCED BY p percent, use value_kind="solar_reduction_percent" and value=p. If it says output will be / drop TO p percent of forecast, or "half"/"one-fifth" of normal, use value_kind="solar_remaining_percent" and value=p.
- minimum_battery_reserve: an absolute figure in kWh uses value_kind="reserve_kwh". A share of battery capacity uses value_kind="reserve_percent_of_capacity" with value = the percentage.
- max_grid_window: value_kind="grid_cap_kwh", value = the cap in kWh.
- no_op: start_hour_24=-1, end_hour_24=-1, value=0, value_kind="none".

Never invent demand, tariff, solar or battery figures. explanation is one short sentence.

Examples:
Note: "Rooftop output will be derated by 60% while technicians work on the array from 09:00 to 11:00."
-> solar_reduction, start 9, end 11, end_inclusive false, value 60, value_kind solar_reduction_percent
Note: "Hold the pack above a quarter of its rated capacity between 7 PM and 11 PM."
-> minimum_battery_reserve, start 19, end 23, end_inclusive false, value 25, value_kind reserve_percent_of_capacity
Note: "The charger stays locked out for three hours starting at 1 AM."
-> no_charge_window, start 1, end 4, end_inclusive false, value 0, value_kind none
Note: "Intake from the utility is limited to 140 kWh each hour from 8 PM until 11 PM."
-> max_grid_window, start 20, end 23, end_inclusive false, value 140, value_kind grid_cap_kwh
Note: "Relay testing means the pack must not supply load from 4 PM to 6 PM."
-> no_discharge_window, start 16, end 18, end_inclusive false, value 0, value_kind none
Note: "The canteen will trial a new breakfast menu from Sunday."
-> no_op`

// ---------------- interpreter ----------------

type Interpreter struct {
	cfg   Config
	http  *http.Client
	mu    sync.Mutex
	cache map[string][]Directive
}

func NewInterpreter(cfg Config) *Interpreter {
	return &Interpreter{
		cfg:   cfg,
		http:  &http.Client{Timeout: cfg.Timeout},
		cache: map[string][]Directive{},
	}
}

func cacheKey(s Scenario) string {
	h := sha256.New()
	fmt.Fprintf(h, "%v|%f|%f|%f|%f|%f", s.Notes, s.Capacity, s.E0, s.MinE, s.MaxCh, s.MaxDis)
	return hex.EncodeToString(h.Sum(nil))
}

// Interpret is the operator-note interpretation path. The LLM is the
// interpreter; everything after it is deterministic validation.
func (in *Interpreter) Interpret(ctx context.Context, s Scenario) ([]Directive, string) {
	key := cacheKey(s)
	in.mu.Lock()
	if d, ok := in.cache[key]; ok {
		in.mu.Unlock()
		return d, "cache"
	}
	in.mu.Unlock()

	source := "llm"
	dirs, err := in.llmPass(ctx, s, "")
	if err != nil {
		log.Printf("llm pass 1 failed: %v", err)
		dirs, err = in.llmPass(ctx, s, "Your previous output was rejected by validation: "+err.Error()+". Re-read every note and return corrected structured output.")
	}
	if err != nil {
		log.Printf("llm pass 2 failed: %v", err)
		raw := AuditAll(s.Notes)
		dirs, err = normalizeAll(raw, s)
		source = "rule-fallback"
		if err != nil {
			dirs = noOpAll(s.Notes)
			source = "noop-fallback"
		}
	} else if in.cfg.Adjudicate {
		if idx := disagreement(dirs, s); idx >= 0 {
			if fixed, e := in.adjudicate(ctx, s, idx, dirs[idx]); e == nil {
				dirs[idx] = fixed
				source = "llm+adjudicated"
			}
		}
	}

	in.mu.Lock()
	in.cache[key] = dirs
	in.mu.Unlock()
	return dirs, source
}

func (in *Interpreter) llmPass(ctx context.Context, s Scenario, correction string) ([]Directive, error) {
	if in.cfg.APIKey == "" {
		return nil, fmt.Errorf("GEMINI_API_KEY not set")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Battery capacity_kwh = %g (use it only if a reserve is expressed as a share of capacity).\n\nOperator notes:\n", s.Capacity)
	for i, n := range s.Notes {
		fmt.Fprintf(&b, "note_index %d: %s\n", i, n)
	}
	fmt.Fprintf(&b, "\nReturn exactly %d entries, note_index 0..%d, in order.", len(s.Notes), len(s.Notes)-1)
	if correction != "" {
		b.WriteString("\n\n" + correction)
	}

	req := gReq{
		SystemInstruction: &gContent{Parts: []gPart{{Text: systemPrompt}}},
		Contents:          []gContent{{Role: "user", Parts: []gPart{{Text: b.String()}}}},
		GenerationConfig: gGenCfg{
			Temperature:      0,
			ResponseMimeType: "application/json",
			ResponseSchema:   responseSchema(),
			ThinkingConfig:   map[string]interface{}{"thinkingBudget": 0},
			MaxOutputTokens:  2048,
		},
	}
	text, err := in.call(ctx, req)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Interpretations []RawInterp `json:"interpretations"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		return nil, fmt.Errorf("model returned unparseable JSON: %w", err)
	}
	return normalizeAll(payload.Interpretations, s)
}

func (in *Interpreter) call(ctx context.Context, req gReq) (string, error) {
	body, _ := json.Marshal(req)
	url := fmt.Sprintf("%s/models/%s:generateContent?key=%s", in.cfg.Endpoint, in.cfg.Model, in.cfg.APIKey)
	ctx, cancel := context.WithTimeout(ctx, in.cfg.Timeout)
	defer cancel()
	hr, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	hr.Header.Set("Content-Type", "application/json")
	res, err := in.http.Do(hr)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var gr gResp
	if err := json.Unmarshal(raw, &gr); err != nil {
		return "", fmt.Errorf("bad provider response")
	}
	if gr.Error != nil {
		return "", fmt.Errorf("provider error %d: %s", gr.Error.Code, gr.Error.Message)
	}
	if len(gr.Candidates) == 0 || len(gr.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("empty candidate")
	}
	return gr.Candidates[0].Content.Parts[0].Text, nil
}

// adjudicate re-asks the model about a single note when the rule-based
// auditor disagrees with the first pass.
func (in *Interpreter) adjudicate(ctx context.Context, s Scenario, idx int, cur Directive) (Directive, error) {
	alt := AuditNote(idx, s.Notes[idx])
	msg := fmt.Sprintf("Battery capacity_kwh = %g.\n\nNote (note_index %d): %s\n\nTwo candidate readings disagree:\nA) type=%s hours=%v\nB) type=%s start=%d end=%d\n\nRe-read the note carefully and return the single correct entry for note_index %d.",
		s.Capacity, idx, s.Notes[idx], cur.Type, cur.Hours, alt.Raw.DirectiveType, alt.Raw.StartHour24, alt.Raw.EndHour24, idx)
	req := gReq{
		SystemInstruction: &gContent{Parts: []gPart{{Text: systemPrompt}}},
		Contents:          []gContent{{Role: "user", Parts: []gPart{{Text: msg}}}},
		GenerationConfig: gGenCfg{Temperature: 0, ResponseMimeType: "application/json",
			ResponseSchema: responseSchema(), ThinkingConfig: map[string]interface{}{"thinkingBudget": 0}, MaxOutputTokens: 1024},
	}
	text, err := in.call(ctx, req)
	if err != nil {
		return cur, err
	}
	var payload struct {
		Interpretations []RawInterp `json:"interpretations"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil || len(payload.Interpretations) == 0 {
		return cur, fmt.Errorf("adjudication unparseable")
	}
	r := payload.Interpretations[0]
	r.NoteIndex = idx
	d, err := Normalize(r, s)
	if err != nil {
		return cur, err
	}
	return d, nil
}

// disagreement returns the first note index where the auditor is confident
// and contradicts the model on type or hours; -1 if none.
func disagreement(dirs []Directive, s Scenario) int {
	for i, n := range s.Notes {
		h := AuditNote(i, n)
		if !h.FoundType || !h.FoundHours {
			continue
		}
		d, err := Normalize(h.Raw, s)
		if err != nil {
			continue
		}
		// Only a TYPE disagreement is worth a second call. The auditor is
		// weakest on hour windows, so hour differences are not escalated;
		// escalating them would add latency for little expected gain.
		if d.Type != dirs[i].Type {
			return i
		}
	}
	return -1
}

func sameInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func normalizeAll(raws []RawInterp, s Scenario) ([]Directive, error) {
	byIdx := map[int]RawInterp{}
	for _, r := range raws {
		if _, dup := byIdx[r.NoteIndex]; dup {
			return nil, fmt.Errorf("duplicate note_index %d", r.NoteIndex)
		}
		byIdx[r.NoteIndex] = r
	}
	out := make([]Directive, len(s.Notes))
	for i := range s.Notes {
		r, ok := byIdx[i]
		if !ok {
			return nil, fmt.Errorf("missing note_index %d", i)
		}
		r.NoteIndex = i
		d, err := Normalize(r, s)
		if err != nil {
			return nil, err
		}
		out[i] = d
	}
	if err := GuardDirectives(out, len(s.Notes), s); err != nil {
		return nil, err
	}
	return out, nil
}

func noOpAll(notes []string) []Directive {
	out := make([]Directive, len(notes))
	for i := range notes {
		out[i] = Directive{NoteIndex: i, Type: DNoOp,
			Explanation: "This note does not affect today's 24-hour energy schedule."}
	}
	return out
}
