# GridWise — LLM-Assisted Campus Energy Optimizer

BUP CSE Fest 2026 Hackathon · Online Preliminary · Smart Campus Energy Optimization Challenge

A single Go HTTP service that reads natural-language campus operator notes with a language
model, validates the model's output deterministically, applies the resulting directives to a
linear program, and returns a cost-minimal, fully valid 24-hour energy schedule.

---

## 1. Quickstart (clean machine, ~2 minutes)

Requires **Go 1.22+** and a **Google AI Studio (Gemini) API key**. No other dependencies —
the service uses only the Go standard library.

```bash
git clone <REPO_URL> && cd gridwise
cp .env.example .env            # then paste your key into .env
export GEMINI_API_KEY="<your key>"
go run .                        # listens on :8080
```

Health check:

```bash
curl -s http://localhost:8080/health
# {"status":"ok"}
```

One public sample against the main endpoint:

```bash
curl -s -X POST http://localhost:8080/optimize-energy \
  -H 'Content-Type: application/json' \
  -d @- <<'JSON' | head -40
{
  "scenario_id": "GRID-101",
  "operator_notes": [
    "Solar output will drop to about 20% from 1 PM to 3 PM.",
    "The cafeteria menu changes tomorrow."
  ],
  "hours": [
    {"hour":0,"demand_kwh":90,"solar_kwh":0,"tariff_bdt_per_kwh":6},
    {"hour":1,"demand_kwh":85,"solar_kwh":0,"tariff_bdt_per_kwh":6},
    {"hour":2,"demand_kwh":80,"solar_kwh":0,"tariff_bdt_per_kwh":5},
    {"hour":3,"demand_kwh":80,"solar_kwh":0,"tariff_bdt_per_kwh":5},
    {"hour":4,"demand_kwh":85,"solar_kwh":0,"tariff_bdt_per_kwh":5},
    {"hour":5,"demand_kwh":95,"solar_kwh":0,"tariff_bdt_per_kwh":6},
    {"hour":6,"demand_kwh":110,"solar_kwh":5,"tariff_bdt_per_kwh":8},
    {"hour":7,"demand_kwh":130,"solar_kwh":20,"tariff_bdt_per_kwh":10},
    {"hour":8,"demand_kwh":150,"solar_kwh":50,"tariff_bdt_per_kwh":12},
    {"hour":9,"demand_kwh":165,"solar_kwh":90,"tariff_bdt_per_kwh":14},
    {"hour":10,"demand_kwh":175,"solar_kwh":130,"tariff_bdt_per_kwh":16},
    {"hour":11,"demand_kwh":180,"solar_kwh":160,"tariff_bdt_per_kwh":16},
    {"hour":12,"demand_kwh":185,"solar_kwh":180,"tariff_bdt_per_kwh":15},
    {"hour":13,"demand_kwh":180,"solar_kwh":170,"tariff_bdt_per_kwh":14},
    {"hour":14,"demand_kwh":170,"solar_kwh":140,"tariff_bdt_per_kwh":13},
    {"hour":15,"demand_kwh":165,"solar_kwh":90,"tariff_bdt_per_kwh":14},
    {"hour":16,"demand_kwh":170,"solar_kwh":45,"tariff_bdt_per_kwh":18},
    {"hour":17,"demand_kwh":185,"solar_kwh":10,"tariff_bdt_per_kwh":22},
    {"hour":18,"demand_kwh":205,"solar_kwh":0,"tariff_bdt_per_kwh":28},
    {"hour":19,"demand_kwh":215,"solar_kwh":0,"tariff_bdt_per_kwh":30},
    {"hour":20,"demand_kwh":205,"solar_kwh":0,"tariff_bdt_per_kwh":26},
    {"hour":21,"demand_kwh":175,"solar_kwh":0,"tariff_bdt_per_kwh":18},
    {"hour":22,"demand_kwh":135,"solar_kwh":0,"tariff_bdt_per_kwh":10},
    {"hour":23,"demand_kwh":105,"solar_kwh":0,"tariff_bdt_per_kwh":7}
  ],
  "battery": {
    "capacity_kwh": 220, "initial_energy_kwh": 110, "minimum_energy_kwh": 40,
    "max_charge_kwh_per_hour": 50, "max_discharge_kwh_per_hour": 50
  }
}
JSON
```

Full public sample pack through the built-in judge replica:

```bash
go run ./cmd/harness http://localhost:8080 testdata/public_cases.json
# expected: interpretation 18/18  valid 10/10  avg cost-ratio 1.0000
```

Offline unit tests (no API key, no network needed):

```bash
go test ./... -v
```

---

## 2. Architecture

```
POST /optimize-energy
   │
   ├─ [1] Request validation (main.go)         400 malformed · 422 semantically invalid
   │
   ├─ [2] LLM interpreter (interpret.go)       Gemini 2.5 Flash, temperature 0,
   │        forced responseSchema, thinkingBudget 0, one call for all notes
   │        └─ emits LOOSE semantics: literal clock endpoints + a value_kind tag
   │
   ├─ [3] Deterministic normalizer (normalize.go)
   │        clock window  → hour list (start-inclusive, end-EXCLUSIVE, wrap-aware)
   │        "80% reduction"/"25% of forecast" → factor (usable fraction remaining)
   │        "50% of capacity" → kWh reserve
   │
   ├─ [4] Guardrails (normalize.go: GuardDirectives)
   │        allowed types · one entry per note in index order · hours unique,
   │        0–23, strictly ascending · factor ∈ [0,1] · reserve ∈ [0, capacity] ·
   │        grid cap finite & non-negative · applies semantics
   │
   ├─ [5] LP optimizer (optimize.go + simplex.go)
   │        two-phase dense simplex, stdlib only, ~20 ms for all 10 public cases
   │        infeasible interpretation → directives progressively dropped,
   │        physical constraints never relaxed
   │
   ├─ [6] Plan materializer (plan.go)
   │        nets charge/discharge into one action, DERIVES grid_kwh from the
   │        energy-balance identity, forces exact end-of-day neutrality
   │
   ├─ [7] Self replay validator (plan.go: Validate)
   │        our own copy of the judge; on failure we fall back to a grid-only
   │        plan that is expensive but guaranteed valid
   │
   └─ 200 JSON
```

### Where the LLM sits

The language model is the **only** component that decides what a note means. It receives the
raw note text and produces the structured directive that the optimizer consumes. Nothing
downstream can invent a directive: the guardrails can only reject.

`auditor.go` contains a rule-based extractor. It is **not** the interpreter. It has two jobs:

1. **Disagreement detection.** If it confidently reads a different *directive type* than the
   model did, the service makes one short adjudication call asking the model to re-read that
   single note. Hour-window differences are deliberately not escalated (the rules are weakest
   there, and a second call would cost latency for little expected gain).
2. **Outage survival.** If the provider is unreachable or returns malformed output twice, the
   service degrades to the rule-based reading rather than returning 5xx.

Measured on the public pack the auditor reads 18/18 notes correctly, but on our own
paraphrase stress set (`testdata/paraphrase_cases.json`) it manages only 5/12 — which is
exactly why phrase matching cannot be the interpreter and the LLM is mandatory.

### Interpretation contract sent to the model

The model reports **semantics, not arithmetic**. It returns `start_hour_24`, `end_hour_24`,
`end_inclusive`, a numeric `value`, and a `value_kind` tag from a fixed enum. Every convention
in the problem statement — end-exclusive windows, factor-as-remaining-fraction,
percent-of-capacity reserves, wrap-around midnight — is applied in Go, not by the model. This
removes the two most common failure modes (off-by-one windows and inverted solar factors)
from the probabilistic part of the system.

---

## 3. Configuration

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `GEMINI_API_KEY` | **yes** | – | Google AI Studio key. Never committed; pass at run time. |
| `GEMINI_MODEL` | no | `gemini-2.5-flash` | Model identifier. |
| `GEMINI_ENDPOINT` | no | `https://generativelanguage.googleapis.com/v1beta` | Override for testing against a mock. |
| `LLM_TIMEOUT_MS` | no | `12000` | Per-call provider timeout. |
| `ENABLE_ADJUDICATION` | no | `true` | Second-opinion call on type disagreement. |
| `PORT` | no | `8080` | Listen port; the server binds `0.0.0.0`. |

**Model and provider:** Google **Gemini 2.5 Flash** via the Generative Language API
`v1beta/models/{model}:generateContent`, called with `temperature: 0`,
`responseMimeType: application/json`, a strict `responseSchema`, and
`thinkingConfig.thinkingBudget: 0` to keep latency low.

---

## 4. Docker fallback

```bash
docker build -t <registry>/gridwise:<tag> .
docker push <registry>/gridwise:<tag>

docker run --rm -p 8080:8080 -e GEMINI_API_KEY="<your key>" <registry>/gridwise:<tag>
curl -s http://localhost:8080/health   # {"status":"ok"}
```

The image binds `0.0.0.0:8080`, runs as a non-root user (uid 10001), contains **no baked-in
credentials**, and is built `CGO_ENABLED=0` so the runtime layer is a bare Alpine plus CA
certificates.

---

## 5. Endpoints

| Method | Path | Behaviour |
|---|---|---|
| `GET` | `/health` | `200 {"status":"ok"}`. Does not touch the model, so readiness is immediate. |
| `POST` | `/optimize-energy` | `200` with interpretation + 24-hour plan. `400` malformed JSON or structurally invalid request. `422` well-formed but semantically impossible (duplicate hours, negative values, `initial_energy_kwh` outside `[minimum, capacity]`). `500` only for a controlled internal error — no stack traces, no secrets. |

Response fields follow the problem statement exactly: `scenario_id`,
`directive_interpretation`, `hourly_plan`, `total_grid_kwh`, `total_cost_bdt`,
`peak_grid_kwh`, `plan_summary`. Totals are always recomputed from the emitted
`hourly_plan`, never from optimizer internals.

---

## 6. Optimization model

Variables per hour `h`: `grid[h]`, `solar_used[h]`, `charge[h]`, `discharge[h]`.

```
min  Σ tariff[h] · grid[h]

s.t. grid[h] + solar_used[h] + discharge[h] − charge[h] = demand[h]
     Σ (charge[h] − discharge[h]) = 0                       (end-of-day neutrality)
     min_energy[h] ≤ E0 + Σ_{k≤h}(charge[k] − discharge[k]) ≤ capacity
     0 ≤ grid[h]        ≤ max_grid_kwh          (max_grid_window)
     0 ≤ solar_used[h]  ≤ solar[h] · factor     (solar_reduction)
     0 ≤ charge[h]      ≤ max_charge            (0 in no_charge_window)
     0 ≤ discharge[h]   ≤ max_discharge         (0 in no_discharge_window)
```

Round-trip efficiency is 1 and charging carries no reward, so any solution with simultaneous
charge and discharge nets to a single action at identical cost — the LP relaxation is exact
and no integer variables are needed. On the 10 public cases this reaches the organizer's
reference cost on every case (`ratio = 1.0000`).

---

## 7. Test artefacts

| Path | What it is |
|---|---|
| `testdata/public_cases.json` | The official public sample pack, unmodified. |
| `testdata/paraphrase_cases.json` | **Unofficial**, generated by us for local robustness testing. Twelve rewritten notes covering all six directive types plus distractors; reference costs computed by an independent solver. Not organizer data. |
| `optimizer_test.go` | Offline tests: LP vs reference cost, self-validator, auditor accuracy. |
| `cmd/harness` | Standalone judge replica. Posts cases to any base URL, scores interpretation, replays the plan against ground-truth directives, reports cost ratio and p95 latency. |

---

## 8. Known limitations

- The rule-based auditor is a fallback, not an interpreter; it reads roughly 40% of unseen
  paraphrases correctly on its own. Scoring quality depends on the model being reachable.
- Interpretations are cached in process, keyed by note text plus battery parameters. The
  cache is not shared across replicas.
- If the interpreted directives are mutually infeasible, the service drops directives
  (largest-subset-first) until the model is solvable. It never relaxes a physical constraint,
  so the returned plan is always valid, but a dropped directive will not be reflected in the
  schedule.
- `plan_summary` is generated deterministically in Go, not by the model.

---

## 9. Security

- No API keys, tokens, or `.env` files are committed; `.gitignore` and `.dockerignore`
  exclude them.
- Request bodies are capped at 1 MB. Panics are recovered and returned as a generic 500.
- Error responses carry a short message only — never provider payloads, stack traces, or
  configuration values. Logs record scenario id, note count, interpretation source, and cost.
- Only the synthetic challenge data supplied in the request is used.

## 10. Credits

Go standard library only (`net/http`, `encoding/json`). Language model: Google Gemini 2.5
Flash. The LP simplex, guardrails, optimizer, materializer, validator, and harness are the
team's own implementation.