package main

import (
	"context"
	"encoding/json"
	"log"
	"math"
	"net/http"
	"os"
	"time"

	"github.com/joho/godotenv"
)

var interp *Interpreter

func main() {
	// Load .env file into os environment
	godotenv.Load()

	cfg := LoadConfig()
	if cfg.APIKey == "" {
		log.Println("WARNING: GEMINI_API_KEY is not set; the service will run on the rule-based fallback only")
	}
	interp = NewInterpreter(cfg)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/optimize-energy", optimizeHandler)

	port := envOr("PORT", "5050")
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           recoverMW(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      35 * time.Second,
	}
	log.Printf("gridwise listening on :%s (model=%s)", port, cfg.Model)
	if err := srv.ListenAndServe(); err != nil {
		log.Println(err)
		os.Exit(1)
	}
}

func recoverMW(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic recovered: %v", rec)
				writeErr(w, 500, "internal error")
			}
		}()
		h.ServeHTTP(w, r)
	})
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func optimizeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	var req ScenarioRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&req); err != nil {
		writeErr(w, 400, "malformed JSON request body")
		return
	}
	s, code, msg := ValidateRequest(req)
	if code != 0 {
		writeErr(w, code, msg)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	dirs, source := interp.Interpret(ctx, s)
	model, _, su, ch, dis, applied, err := SolveWithFallback(s, dirs)

	var plan []PlanHour
	if err != nil {
		log.Printf("scenario %s: optimizer failed (%v); using safe plan", s.ID, err)
		model = BuildModel(s, nil)
		plan = SafePlan(s)
	} else {
		plan = Materialize(s, model, su, ch, dis)
	}
	tot, cost, peak := Totals(s, plan)

	// Final replay: never return a plan our own judge rejects.
	if errs := Validate(s, model, plan, tot, cost, peak); len(errs) > 0 {
		log.Printf("scenario %s: self-validation failed %v; using safe plan", s.ID, errs)
		model = BuildModel(s, nil)
		plan = SafePlan(s)
		tot, cost, peak = Totals(s, plan)
	}

	entries := make([]DirectiveEntry, len(dirs))
	for i, d := range dirs {
		entries[i] = d.Entry()
	}
	log.Printf("scenario=%s notes=%d source=%s applied=%d cost=%.2f", s.ID, len(s.Notes), source, len(applied), cost)

	writeJSON(w, 200, OptimizeResponse{
		ScenarioID:              s.ID,
		DirectiveInterpretation: entries,
		HourlyPlan:              plan,
		TotalGridKwh:            tot,
		TotalCostBdt:            cost,
		PeakGridKwh:             peak,
		PlanSummary:             Summarize(dirs, plan, cost),
	})
}

// ValidateRequest returns (scenario, httpCode, message). code 0 means valid.
// 400 = structurally invalid, 422 = well-formed but semantically impossible.
func ValidateRequest(r ScenarioRequest) (Scenario, int, string) {
	var s Scenario
	if r.ScenarioID == "" {
		return s, 400, "scenario_id is required"
	}
	if len(r.OperatorNotes) < 1 || len(r.OperatorNotes) > 3 {
		return s, 400, "operator_notes must contain 1-3 entries"
	}
	for _, n := range r.OperatorNotes {
		if len(n) == 0 {
			return s, 400, "operator_notes entries must be non-empty"
		}
	}
	if len(r.Hours) != 24 {
		return s, 400, "hours must contain exactly 24 entries"
	}
	b := r.Battery
	for _, p := range []*float64{b.CapacityKwh, b.InitialEnergyKwh, b.MinimumEnergyKwh, b.MaxChargeKwhPerHour, b.MaxDischargeKwhPerHour} {
		if p == nil {
			return s, 400, "battery is missing required fields"
		}
		if math.IsNaN(*p) || math.IsInf(*p, 0) || *p < 0 {
			return s, 422, "battery values must be finite and non-negative"
		}
	}
	seen := map[int]bool{}
	for _, h := range r.Hours {
		if h.Hour == nil || h.DemandKwh == nil || h.SolarKwh == nil || h.TariffBdtPerKwh == nil {
			return s, 400, "each hour requires hour, demand_kwh, solar_kwh and tariff_bdt_per_kwh"
		}
		i := *h.Hour
		if i < 0 || i > 23 {
			return s, 422, "hour values must be 0-23"
		}
		if seen[i] {
			return s, 422, "duplicate hour entry"
		}
		seen[i] = true
		for _, v := range []float64{*h.DemandKwh, *h.SolarKwh, *h.TariffBdtPerKwh} {
			if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
				return s, 422, "hour values must be finite and non-negative"
			}
		}
		s.Demand[i] = *h.DemandKwh
		s.Solar[i] = *h.SolarKwh
		s.Tariff[i] = *h.TariffBdtPerKwh
	}
	if len(seen) != 24 {
		return s, 422, "hours must cover 0-23 exactly once"
	}
	s.ID = r.ScenarioID
	s.Notes = r.OperatorNotes
	s.Capacity = *b.CapacityKwh
	s.E0 = *b.InitialEnergyKwh
	s.MinE = *b.MinimumEnergyKwh
	s.MaxCh = *b.MaxChargeKwhPerHour
	s.MaxDis = *b.MaxDischargeKwhPerHour
	if s.MinE > s.Capacity || s.E0 > s.Capacity || s.E0 < s.MinE {
		return s, 422, "inconsistent battery capacity/initial/minimum values"
	}
	return s, 0, ""
}
