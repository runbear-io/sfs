package webapp

// Manual demo harness: a seeded hub (a realistic company wiki, zipf-ish read
// data, four agent devices) for exploring the web UI by hand and for taking
// product screenshots. Not part of the test suite: it only runs with
// BDRIVE_MANUAL_SERVE=1.
//
// State lives in a STABLE directory (os.TempDir()/bdrive-demo-hub, override
// with BDRIVE_MANUAL_STATE), so restarting the harness — e.g. after a
// frontend change — keeps accounts, browser sessions, the project id, and
// all seeded/demo data. Delete the directory to reset the demo.
//
// The seed is deliberately realistic: real-looking runbooks, ADRs, dated
// meeting notes and product docs, with bodies that render as proper markdown
// (headings, tables, code, callouts). Screenshots taken here end up on the
// website, and "run-015.md" in a treemap tells a visitor nothing.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
)

func TestManualServe(t *testing.T) {
	if os.Getenv("BDRIVE_MANUAL_SERVE") == "" {
		t.Skip("manual demo harness; set BDRIVE_MANUAL_SERVE=1 to run")
	}
	ln := listenHub(t) // before any state is touched — same port as the e2e harness
	state := os.Getenv("BDRIVE_MANUAL_STATE")
	if state == "" {
		state = filepath.Join(os.TempDir(), "bdrive-demo-hub")
	}
	if err := os.MkdirAll(filepath.Join(state, "storage"), 0o755); err != nil {
		t.Fatal(err)
	}

	be, err := remote.Open(t.Context(), "file://"+filepath.Join(state, "storage"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := OpenProjectDB(filepath.Join(state, "projects.json"))
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := db.GetOrCreate("acme-wiki", "") // create-or-join: id is stable across restarts
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{Root: be, Projects: db, Device: webDevice, Refresh: 0, Upload: UploadConfig{Enabled: true}}

	prefix := filepath.Join(state, "storage", p.ID)
	seedMark := filepath.Join(prefix, "journal", "seed.jsonl")
	if _, err := os.Stat(seedMark); os.IsNotExist(err) {
		seedDemo(t, state, prefix, p.ID)
	}

	srv.Reads, err = OpenReadLedger(filepath.Join(state, "reads.json"), 0)
	if err != nil {
		t.Fatal(err)
	}
	srv.Devices, _ = OpenDeviceRegistry(filepath.Join(state, "devices.json"))
	// One agent per teammate, plus shared CI — which is what a real team's
	// coverage matrix looks like once everyone is running their own.
	for _, d := range demoDevices {
		srv.Devices.Observe(d)
	}

	srv.Shares, _ = OpenShareDB(filepath.Join(state, "shares.json"))

	auth, err := OpenBuiltinAuth(filepath.Join(state, "auth.json"), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	auth.signup("snow@runbear.io", "Snow", "password1") // no-op if the account exists
	auth.Admins = map[string]bool{"snow@runbear.io": true}
	srv.Auth = auth

	t.Logf("serving on http://%s (state: %s) — snow@runbear.io / password1", hubAddr(), state)
	serveHub(t, ln, srv.Handler(), 8*time.Hour)
}

// demoDevices is the agent fleet: one per teammate plus shared CI. Each has a
// bias in seedDemo so the coverage matrix shows agents specialising rather than
// eight identical rows.
var demoDevices = []DeviceInfo{
	{ID: "claude-snow", Name: "claude-snow", OS: "darwin/arm64"},
	{ID: "claude-priya", Name: "claude-priya", OS: "darwin/arm64"},
	{ID: "claude-marco", Name: "claude-marco", OS: "darwin/arm64"},
	{ID: "claude-ana", Name: "claude-ana", OS: "linux/amd64"},
	{ID: "codex-mia", Name: "codex-mia", OS: "darwin/arm64"},
	{ID: "codex-sam", Name: "codex-sam", OS: "linux/amd64"},
	{ID: "gemini-doc", Name: "gemini-doc", OS: "linux/amd64"},
	{ID: "claude-ci", Name: "claude-ci", OS: "linux/amd64"},
}

// agentBias is how much each agent over- or under-reads a given top-level
// area. Anything unlisted reads it at 1.0.
var agentBias = map[string]map[string]float64{
	"claude-snow":  {"wiki": 2.4, "docs": 1.6, "notes": 0.4},
	"claude-priya": {"wiki": 1.8, "shared": 2.2, "notes": 0.5},
	"claude-marco": {"docs": 2.6, "shared": 1.4, "wiki": 0.6},
	"claude-ana":   {"notes": 2.8, "wiki": 0.7, "docs": 0.5},
	"codex-mia":    {"wiki": 3.0, "notes": 0.3, "shared": 0.4},
	"codex-sam":    {"wiki": 1.5, "docs": 0.4, "shared": 1.9},
	"gemini-doc":   {"docs": 3.2, "notes": 1.2, "wiki": 0.5},
	"claude-ci":    {"wiki": 2.0, "shared": 0.3, "docs": 0.8},
}

// doc is one seeded file: a path and the markdown body behind it.
type doc struct {
	path string
	body string
}

// seedDemo writes the demo wiki (journal + blobs) and its read buckets. Runs
// once per state dir.
func seedDemo(t *testing.T, state, prefix, projectID string) {
	t.Helper()
	os.MkdirAll(filepath.Join(prefix, "journal"), 0o755)
	os.MkdirAll(filepath.Join(prefix, "blobs"), 0o755)

	seed := int64(42)
	rnd := func() float64 {
		seed = (seed*16807 + 7) % 2147483647
		return float64(seed) / 2147483647
	}

	docs := demoDocs()
	humans := []string{"alice@x.io", "bob@x.io", "carol@x.io"}
	agents := make([]string, 0, len(demoDevices))
	for _, d := range demoDevices {
		agents = append(agents, d.ID)
	}

	now := time.Now().UTC()
	var ops []journal.Op
	var stats []ReadStat
	var lam, seq int64

	for _, d := range docs {
		sum := sha256.Sum256([]byte(d.body))
		blob := hex.EncodeToString(sum[:])
		os.WriteFile(filepath.Join(prefix, "blobs", blob), []byte(d.body), 0o644)
		// Most of a live wiki is current — roughly a sixth is genuinely stale.
		// A warning triangle on every second file just reads as noise.
		//
		// Staleness is correlated with reads on purpose: a heavily-read file is
		// far likelier to have rotted, because it's the one everybody trusts
		// and nobody owns. That puts the red where the story is — big cells in
		// the treemap, top-right in the scatter — instead of scattering it
		// across a hundred files nobody opens.
		hot := rnd() < 0.15
		staleChance := 0.09
		if hot {
			staleChance = 0.5
		}
		stale := int(24 * rnd())
		if rnd() < staleChance {
			stale = 34 + int(260*rnd()*rnd())
		}
		lam++
		seq++
		ops = append(ops, journal.Op{
			Seq: seq, Lamport: lam, Time: now.AddDate(0, 0, -stale),
			Device: "seed", DeviceName: "seed", Author: "alice@x.io",
			User: "alice@x.io", UserName: "Alice",
			Kind: journal.KindPut, Path: d.path, Blob: blob,
			Size: int64(len(d.body)), Mode: 0o644,
		})

		dir := filepath.Dir(d.path)
		day := now.AddDate(0, 0, -int(rnd()*20)).Format("2006-01-02")
		for _, a := range humans {
			n := int64(math.Floor((map[bool]float64{true: 30, false: 3}[hot]) * rnd() * rnd()))
			if n > 0 {
				stats = append(stats, ReadStat{Project: projectID, Path: d.path, Day: day,
					Kind: ReadKindHuman, Actor: a, Count: n, Last: now})
			}
		}
		top, _, _ := strings.Cut(d.path, "/")
		for _, a := range agents {
			// Each agent has its own areas (agentBias); on top of that,
			// runbooks are what everyone's agent lives in and research notes
			// are written for humans and barely read by anything.
			boost := 1.0
			if b, ok := agentBias[a][top]; ok {
				boost = b
			}
			if dir == "wiki/runbooks" {
				boost *= 2.2
			}
			if dir == "notes/research" {
				boost *= 0.05
			}
			n := int64(math.Floor((map[bool]float64{true: 60, false: 5}[hot]) * rnd() * rnd() * boost))
			if n > 0 {
				stats = append(stats, ReadStat{Project: projectID, Path: d.path, Day: day,
					Kind: ReadKindAgent, Actor: a, Count: n, Last: now})
			}
		}
	}

	if err := journal.Append(filepath.Join(prefix, "journal", "seed.jsonl"), ops); err != nil {
		t.Fatal(err)
	}
	if err := newFileReadRepo(filepath.Join(state, "reads.json")).PutBatch(stats); err != nil {
		t.Fatal(err)
	}
}

// demoDocs builds the seeded wiki: a handful of hand-written documents that
// render well enough to screenshot, plus plausible filler so the folder
// listings and the insights treemap look like a real company's knowledge base.
func demoDocs() []doc {
	var out []doc
	add := func(path, body string) { out = append(out, doc{path, body}) }

	add("wiki/q3-findings.md", q3Findings)
	add("wiki/runbooks/incident-response.md", incidentResponse)
	add("wiki/onboarding/first-week.md", firstWeek)

	// Filler with real-looking names. Bodies follow a per-folder shape so a
	// reader who opens one sees something plausible rather than lorem ipsum.
	type group struct {
		dir     string
		heading string
		names   []string
	}
	groups := []group{
		{"wiki/onboarding", "Onboarding", []string{
			"engineering-setup", "who-does-what", "glossary", "tools-and-access",
			"first-pull-request", "how-we-write-docs", "meeting-culture", "expenses",
			"security-basics", "support-rotation", "vacation-policy",
		}},
		{"wiki/architecture", "Architecture", []string{
			"system-overview", "auth-and-sessions", "data-model", "event-pipeline",
			"storage-layout", "caching-strategy", "multi-region", "rate-limiting",
			"background-jobs", "search-indexing", "webhooks-delivery", "observability",
			"secrets-management", "migration-strategy",
		}},
		{"wiki/runbooks", "Runbook", []string{
			"deploy-and-rollback", "database-failover", "oncall-handoff",
			"restore-from-backup", "rotate-credentials", "scale-up-workers",
			"clear-stuck-queue", "expired-certificate", "region-evacuation",
			"data-export-request", "hotfix-process", "postmortem-template",
			"paging-policy", "load-shedding", "cache-flush",
		}},
		{"wiki/api", "API", []string{
			"rest-conventions", "authentication", "errors", "pagination",
			"rate-limits", "webhooks", "versioning", "idempotency",
			"batch-endpoints", "sdk-guidelines", "deprecation-policy",
			"changelog", "sandbox-environment",
		}},
		{"wiki/decisions", "Decision record", []string{
			"0001-postgres-over-dynamo", "0002-monorepo", "0003-typescript-everywhere",
			"0004-no-graphql", "0005-queue-choice", "0006-feature-flags",
			"0007-tenant-isolation", "0008-append-only-journals", "0009-object-storage",
			"0010-auth-provider", "0011-observability-stack", "0012-release-cadence",
			"0013-pricing-model", "0014-support-tiers", "0015-data-retention",
			"0016-mobile-strategy", "0017-i18n", "0018-schema-migrations",
		}},
		{"docs/product", "Product", []string{
			"pricing-v2", "roadmap-h2", "personas", "activation-metrics",
			"onboarding-flow", "trial-experiment", "churn-drivers", "packaging",
			"competitive-positioning", "feature-requests", "beta-program",
			"launch-checklist", "success-metrics", "pricing-faq",
		}},
		{"docs/design", "Design", []string{
			"design-system", "brand-voice", "iconography", "empty-states",
			"motion-principles", "accessibility", "dark-mode", "form-patterns",
			"illustration-style",
		}},
		{"notes/research", "Research", []string{
			"competitive-landscape", "user-interviews-q2", "pricing-sensitivity",
			"churn-interviews", "market-sizing", "buyer-personas", "win-loss-review",
			"support-ticket-themes", "nps-verbatims", "usability-round-3",
			"agent-usage-patterns", "enterprise-requirements",
		}},
		{"shared/reports", "Report", []string{
			"board-update-q3", "churn-analysis", "revenue-review", "hiring-plan",
			"security-review", "uptime-report", "cost-breakdown", "growth-review",
			"customer-health", "quarterly-okrs", "annual-plan", "budget-forecast",
			"partner-review",
		}},
	}
	for _, g := range groups {
		for _, n := range g.names {
			out = append(out, doc{
				path: g.dir + "/" + n + ".md",
				body: fillerDoc(g.heading, n),
			})
		}
	}

	// Dated meeting notes: the long tail every wiki has.
	start := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	kinds := []string{"standup", "retro", "planning", "design-review", "incident-review"}
	for i := 0; i < 60; i++ {
		d := start.AddDate(0, 0, i*3)
		kind := kinds[i%len(kinds)]
		out = append(out, doc{
			path: fmt.Sprintf("notes/meetings/%s-%s.md", d.Format("2006-01-02"), kind),
			body: fillerDoc("Meeting", fmt.Sprintf("%s %s", d.Format("Jan 2"), kind)),
		})
	}
	return out
}

// fillerDoc renders a plausible short document for the long tail.
func fillerDoc(heading, slug string) string {
	title := strings.ToUpper(slug[:1]) + strings.ReplaceAll(slug[1:], "-", " ")
	return fmt.Sprintf(`# %s

_%s · owned by the platform team_

## Summary

Short context on %s so anyone — human or agent — can act without asking.

## Details

- What it covers and who it affects
- The constraint that made us choose this
- What to do when it changes

## Related

See the rest of the %s section.
`, title, heading, title, strings.ToLower(heading))
}

// --- hand-written documents (these are the ones that get screenshotted) ---

const q3Findings = `# Q3 findings

_Written by Claude on Snow's machine · reviewed by Alice_

Churn is concentrated in self-serve accounts that never reach a second
seat. Everything below follows from that one fact.

## Headline numbers

| Metric | Q2 | Q3 | Change |
| --- | --- | --- | --- |
| Net revenue retention | 104% | 97% | **−7 pts** |
| Self-serve churn | 4.1% | 6.8% | +2.7 pts |
| Team-plan churn | 1.9% | 1.7% | −0.2 pts |
| Median seats at churn | 1.0 | 1.0 | — |

## What we learned

1. **Single-seat accounts churn 4× faster.** Accounts that never invite a
   teammate leave within 40 days on average. Accounts that add a second seat
   in week one almost never leave.
2. **The aha requires two people.** Every retained account has at least one
   shared folder with activity from more than one machine.
3. **Price is not the driver.** Only 6% of exit surveys mention cost;
   41% say "never really got started".

## What we are doing about it

- Make the second seat reachable without a credit card
- Move the invite step into the first session, not the settings page
- Instrument time-to-second-device as the activation metric

## Open questions

- Does a 3-seat free tier cannibalize Team, or feed it?
- Can onboarding create the second seat automatically for an agent?
`

const incidentResponse = `# Incident response runbook

How we handle a production incident, start to finish. **Agents: read this
before touching anything during an active incident.**

## Severity levels

| Level | Meaning | Response |
| --- | --- | --- |
| SEV-1 | Customer-facing outage | Page on-call, all hands |
| SEV-2 | Degraded service | On-call handles, updates hourly |
| SEV-3 | Internal breakage | Ticket, fix within the week |

## First 15 minutes

1. Acknowledge the page and open an incident channel.
2. Assign an incident commander — one voice, one timeline.
3. Freeze deploys:

` + "```sh\ndeployctl freeze --reason \"SEV-1 in progress\"\n```" + `

4. Post the first status update **before** debugging.

## After the incident

- Write the retro within 48 hours (blameless, timeline-first)
- File follow-ups as issues with the ` + "`incident`" + ` label
- Update this runbook if reality disagreed with it
`

const firstWeek = `# Your first week

Welcome. This page is the short version; everything else is linked from here.

## Day one

- Get access to the wiki, the repo, and the on-call rotation
- Run the setup script and open a pull request that changes one line
- Say hello in the team channel

## Day two to five

1. Pair with someone on a real ticket
2. Read the [architecture overview](../architecture/system-overview.md)
3. Shadow an on-call handoff

## How we work

We write things down. If you asked a question and the answer was not in the
wiki, the answer belongs in the wiki — your agent can add it for you.
`
