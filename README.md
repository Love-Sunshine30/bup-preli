# GridWise — LLM-Assisted Campus Energy Optimizer

BUP CSE Fest 2026 Hackathon · Online Preliminary · Smart Campus Energy Optimization Challenge

A single Go HTTP service that reads natural-language campus operator notes with a language
model, validates the model's output deterministically, applies the resulting directives to a
linear program, and returns a cost-minimal, fully valid 24-hour energy schedule.

* * *

## 1. Quickstart (clean machine, ~2 minutes)

Requires **Go 1.22+** (the module targets a newer toolchain, which the Go tool fetches
automatically if yours is older) and a **Google AI Studio (Gemini) API key**. The only
third-party dependency is `github.com/joho/godotenv` (a local `.env` file is loaded if one
is present; it is never required).

```
git clone https://github.com/love-sunshine30/bup-preli.git && cd bup-preli
export GEMINI_API_KEY="<your key>"          # or put it in a local .env file
go run .                                    # listens on :5050 by default
```

> If `GEMINI_API_KEY` is not set the service still boots — it logs a warning and answers
> every request with the rule-based fallback interpreter only. Set the key for full accuracy.

Health check:

```
curl -s http://localhost:5050/health
# {"status":"ok"}
```

One public sample against the main endpoint:

```
curl -s -X POST http://localhost:5050/optimize-energy \
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

Offline smoke test (no API key, no network needed — serves `404`/`405` paths and
validation logic without the model):

```
go build ./... && go vet ./...
```

* * *

## 2. Architecture

```
POST /optimize-energy
   │
   ├─ [1] Request validation (main.go: ValidateRequest)
   │        400 malformed · missing fields · notes not 1–3
   │        422 semantically impossible (out-of-range, duplicates, inconsistent battery)
   │
   ├─ [2] LLM interpreter (interpret.go: Interpret)
   │        Gemini, temperature 0, forced responseSchema, thinkingBudget 0,
   │        maxOutputTokens 2048, ONE call for all notes
   │        retry ×1 with a validation-correction hint if pass 1 fails
   │        └─ emits LOOSE semantics: literal clock endpoints + a value_kind tag
   │
   ├─ [3] Deterministic normalizer (normalize.go: Normalize, ExpandHours)
   │        clock window  → hour list (start-inclusive, end-EXCLUSIVE, wrap-aware)
   │        "reduced by 80%" vs "drop to 25%" → factor (usable fraction remaining)
   │        "50% of capacity" → kWh reserve
   │
   ├─ [4] Guardrails (normalize.go: GuardDirectives)
   │        allowed types · one entry per note in index order · hours unique,
   │        0–23, strictly ascending · factor ∈ [0,1] · reserve ∈ [0, capacity] ·
   │        grid cap finite & non-negative
   │
   ├─ [5] LP optimizer (optimize.go + simplex.go)
   │        two-phase dense simplex, Dantzig pricing with Bland's-rule fallback,
   │        ~20 ms for all 10 public cases
   │        infeasible interpretation → directives progressively dropped
   │        (largest subset first); physical constraints never relaxed
   │
   ├─ [6] Plan materializer (plan.go: Materialize)
   │        nets charge/discharge into one action, DERIVES grid_kwh from the
   │        energy-balance identity, forces exact end-of-day neutrality
   │
   ├─ [7] Self replay validator (plan.go: Validate, tolerance 0.01)
   │        our own copy of the judge; on failure we fall back to SafePlan —
   │        a grid-only schedule that is expensive but guaranteed valid
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
paraphrase stress set (`paraphrase_case.json`) it manages only 5/12 — which is
exactly why phrase matching cannot be the interpreter and the LLM is mandatory.

### Interpretation contract sent to the model

The model reports **semantics, not arithmetic**. It returns `start_hour_24`, `end_hour_24`,
`end_inclusive`, a numeric `value`, and a `value_kind` tag from a fixed enum. Every convention
in the problem statement — end-exclusive windows, factor-as-remaining-fraction,
percent-of-capacity reserves, wrap-around midnight — is applied in Go, not by the model. This
removes the two most common failure modes (off-by-one windows and inverted solar factors)
from the probabilistic part of the system.

### Directive types

| `directive_type` | Meaning | `structured_adjustment` |
| --- | --- | --- |
| `solar_reduction` | Usable solar multiplied by `factor` in `hours` | `{hours, factor}` |
| `minimum_battery_reserve` | Battery must stay at or above `minimum_energy_kwh` in `hours` | `{hours, minimum_energy_kwh}` |
| `no_charge_window` | Battery cannot charge in `hours` | `{hours}` |
| `no_discharge_window` | Battery cannot discharge in `hours` | `{hours}` |
| `max_grid_window` | Grid import capped at `max_grid_kwh` per hour in `hours` | `{hours, max_grid_kwh}` |
| `no_op` | Note has nothing to do with today's schedule | `null` |

* * *

## 3. Configuration

All configuration is via environment variables (a local `.env` file is loaded automatically
by `godotenv` if present):

| Variable | Required | Default | Purpose |
| --- | --- | --- | --- |
| `GEMINI_API_KEY` | recommended | – | Google AI Studio key. If empty, the service boots on the rule-based fallback only. Never commit it. |
| `GEMINI_MODEL` | no | `gemini-3.6-flash` | Model identifier. |
| `GEMINI_ENDPOINT` | no | `https://generativelanguage.googleapis.com/v1beta` | Override for testing against a mock. |
| `LLM_TIMEOUT_MS` | no | `12000` | Per-call provider timeout (ms). |
| `ENABLE_ADJUDICATION` | no | `true` | Second-opinion call on type disagreement. |
| `PORT` | no | `5050` (Dockerfile sets `8080`) | Listen port; the server binds all interfaces. |

**Model and provider:** Google **Gemini** via the Generative Language API
`v1beta/models/{model}:generateContent`, called with `temperature: 0`,
`responseMimeType: application/json`, a strict `responseSchema`, and
`thinkingConfig.thinkingBudget: 0` to keep latency low.

* * *

## 4. Endpoints

| Method | Path | Behaviour |
| --- | --- | --- |
| `GET` | `/health` | `200 {"status":"ok"}`. Does not touch the model, so readiness is immediate. |
| `POST` | `/optimize-energy` | `200` with interpretation + 24-hour plan. See below. |

`POST /optimize-energy` status codes:

| Code | When |
| --- | --- |
| `200` | Interpretation + plan returned (always a valid plan — see fallback chain). |
| `400` | Malformed JSON, missing required fields, `operator_notes` not 1–3 entries, `hours` not exactly 24 entries. |
| `405` | Non-POST method. |
| `422` | Well-formed but semantically impossible: duplicate/out-of-range hours, negative or non-finite values, battery `initial` outside `[minimum, capacity]`. |
| `500` | Controlled internal error only — no stack traces, no secrets. |

Errors always carry a short JSON body: `{"error": "<message>"}`.

### Request

```json
{
  "scenario_id": "GRID-101",
  "operator_notes": ["1-3 non-empty strings"],
  "hours": [
    {"hour": 0, "demand_kwh": 90, "solar_kwh": 0, "tariff_bdt_per_kwh": 6}
  ],
  "battery": {
    "capacity_kwh": 220,
    "initial_energy_kwh": 110,
    "minimum_energy_kwh": 40,
    "max_charge_kwh_per_hour": 50,
    "max_discharge_kwh_per_hour": 50
  }
}
```

`hours` must contain exactly 24 entries covering hours 0–23 exactly once; all five battery
fields are required.

### Response

```json
{
  "scenario_id": "GRID-101",
  "directive_interpretation": [
    {
      "note_index": 0,
      "applies": true,
      "directive_type": "solar_reduction",
      "structured_adjustment": {"hours": [13, 14], "factor": 0.2},
      "explanation": "Solar output is reduced to 20% of forecast from 1 PM to 3 PM."
    }
  ],
  "hourly_plan": [
    {
      "hour": 0,
      "grid_kwh": 40,
      "solar_used_kwh": 50,
      "battery_action": "discharge",
      "battery_kwh": 30,
      "battery_energy_after_kwh": 80
    }
  ],
  "total_grid_kwh": 2500.5,
  "total_cost_bdt": 41230.75,
  "peak_grid_kwh": 215,
  "plan_summary": "Charged the battery in 6 low-tariff hours and discharged in 5 high-tariff hours, using available solar first; applied solar_reduction. Battery returns to its initial state of charge at hour 23. Total grid cost 41230.75 BDT."
}
```

Response fields follow the problem statement: `scenario_id`, `directive_interpretation`,
`hourly_plan`, `total_grid_kwh`, `total_cost_bdt`, `peak_grid_kwh`, `plan_summary`. Totals are
always recomputed from the emitted `hourly_plan`, never from optimizer internals.

Each `hourly_plan` entry: `hour`, `grid_kwh`, `solar_used_kwh`, `battery_action`
(`charge` | `discharge` | `idle`), `battery_kwh` (magnitude of the action), and
`battery_energy_after_kwh` (state of charge at the end of the hour).

* * *

## 5. Fallback chain (never a 5xx, never an invalid plan)

The service degrades gracefully, in this order:

1. **LLM pass 1** — one call, all notes, schema-forced output.
2. **LLM pass 2** — same call with a validation-correction hint if pass 1 output failed
   guardrails.
3. **Rule-based fallback** — `auditor.go` phrase extraction (lower accuracy, keeps the
   service alive during a provider outage).
4. **All-`no_op` fallback** — if even the rules fail validation.

Independently, after solving:

- If the interpreted directives are mutually infeasible, directives are dropped
  largest-subset-first until the model solves. Physical constraints are never relaxed.
- If the final plan fails the self replay validator, it is replaced by `SafePlan`
  (grid-only, battery idle) — expensive but always valid.

The interpretation source is logged per request as one of: `llm`, `llm+adjudicated`,
`rule-fallback`, `noop-fallback`, `cache`.

* * *

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

* * *

## 7. Docker

```
docker build -t <registry>/gridwise:<tag> .
docker push <registry>/gridwise:<tag>

docker run --rm -p 8080:8080 -e GEMINI_API_KEY="<your key>" <registry>/gridwise:<tag>
curl -s http://localhost:8080/health   # {"status":"ok"}
```

The image binds `0.0.0.0:8080`, runs as a non-root user (uid 10001), contains **no baked-in**
credentials, and is built `CGO_ENABLED=0` so the runtime layer is a bare Alpine plus CA
certificates. Note: the Dockerfile pins `ENV PORT=8080`; the binary's own default is `5050`.

* * *

## 8. Deployment

Any platform that can run a container (or a Go binary) works. Minimal recipes:

**Render**
1. New → Web Service → connect the repo (Dockerfile is auto-detected).
2. Add environment variable `GEMINI_API_KEY`. Render injects `PORT` automatically —
   the binary honours it, so no extra config is needed.
3. Deploy → you get `https://<your-app>.onrender.com` with automatic HTTPS.

**Google Cloud Run**
```
gcloud run deploy gridwise --source . \
  --set-env-vars GEMINI_API_KEY=<your-key> \
  --allow-unauthenticated --region <region>
```

**Fly.io**
```
fly launch      # detects the Dockerfile
fly secrets set GEMINI_API_KEY=<your-key>
fly deploy
```

**Keep 1 replica**: the interpretation cache is in-process and keyed by note text + battery
parameters; it is not shared across replicas.

* * *

## 9. Project layout

```
main.go        HTTP server, request validation (400/422), orchestration, timeouts
interpret.go   Gemini client, system prompt, retry/correction, adjudication, cache
normalize.go   clock→hours expansion, percent/fraction conventions, guardrails
auditor.go     rule-based extractor (disagreement detection + outage fallback ONLY)
optimize.go    LP model builder + largest-subset-first infeasibility fallback
simplex.go     two-phase dense simplex (Dantzig + Bland's-rule fallback)
plan.go        plan materializer, totals, self replay validator, SafePlan, summary
types.go       request/response schemas, directive types, structured_adjustment
public_cases.json      official public sample pack, unmodified
paraphrase_case.json   unofficial paraphrase stress set (our own, not organizer data)
Dockerfile             multi-stage, non-root, CGO_ENABLED=0
```

* * *

## 10. Security

- No API keys, tokens, or `.env` files are committed; `.gitignore` and `.dockerignore`
  exclude them.
- Request bodies are capped at 1 MB. Server timeouts: header 10 s, read 30 s, write 35 s;
  per-request handler budget 25 s. Panics are recovered and returned as a generic 500.
- Error responses carry a short message only — never provider payloads, stack traces, or
  configuration values. Logs record scenario id, note count, interpretation source, applied
  directive count, and cost.
- Only the synthetic challenge data supplied in the request is used.

* * *

## 11. Known limitations

- The rule-based auditor is a fallback, not an interpreter; it reads roughly 40% of unseen
  paraphrases correctly on its own. Scoring quality depends on the model being reachable.
- Interpretations are cached in process, keyed by note text plus battery parameters. The
  cache is not shared across replicas.
- If the interpreted directives are mutually infeasible, the service drops directives
  (largest-subset-first) until the model is solvable. It never relaxes a physical constraint,
  so the returned plan is always valid, but a dropped directive will not be reflected in the
  schedule.
- `plan_summary` is generated deterministically in Go, not by the model.

* * *

## 12. Credits

Go (`net/http`, `encoding/json`) plus a single dependency for `.env` loading
(`github.com/joho/godotenv`). Language model: Google Gemini. The LP simplex, guardrails,
optimizer, materializer, validator, and auditor are the team's own implementation.
