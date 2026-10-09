package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/pcc-labs/tapes-test/internal/spend"
	"github.com/pcc-labs/tapes-test/internal/stack"
)

// spendCmd prints RoutableSpend over the imported sessions: the frontier
// spend that could move to a cheaper model. See internal/spend.
func spendCmd(ctx context.Context, args []string) error {
	fs := newFlags("spend")
	sinceDays := fs.Int("since-days", 0, "")
	smallOutput := fs.Int64("small-output", spend.DefaultSmallOutput, "")
	pricing := fs.String("pricing", "", "")
	asJSON := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	table, err := spend.LoadPricing(*pricing)
	if err != nil {
		return err
	}
	st, err := stack.New(false)
	if err != nil {
		return err
	}
	var since time.Time
	if *sinceDays > 0 {
		since = time.Now().AddDate(0, 0, -*sinceDays)
	}
	calls, err := loadCalls(ctx, st, since)
	if err != nil {
		return err
	}
	r := spend.Compute(calls, spend.Options{Pricing: table, SmallOutput: *smallOutput})
	if *asJSON {
		return printSpendJSON(r)
	}
	printSpend(r, len(calls) == 0)
	return nil
}

// callsQuery is the prototype's query: every llm span with usage, its
// token counts by type, and the tool_use blocks in its output. Postgres
// prints the whole result as one JSON array. %s is the union of partition
// tables, %s the earliest started_at.
const callsQuery = `
select coalesce(json_agg(r), '[]'::json) from (
  select session_id, thread_id, started_at, model, call_kind,
         coalesce((usage->>'prompt_tokens')::bigint, 0)               as prompt,
         coalesce((usage->>'completion_tokens')::bigint, 0)           as completion,
         coalesce((usage->>'cache_read_input_tokens')::bigint, 0)     as cache_read,
         coalesce((usage->>'cache_creation_input_tokens')::bigint, 0) as cache_write,
         (select coalesce(json_agg(json_build_object(
                    'name', b->>'tool_name',
                    'command', coalesce(b->'tool_input'->>'command', b->'tool_input'->>'cmd'))), '[]'::json)
            from jsonb_array_elements(case when jsonb_typeof(output) = 'array'
                                           then output else '[]'::jsonb end) b
           where b->>'type' = 'tool_use') as tools
    from (%s) spans
   where kind = 'llm' and usage ? 'completion_tokens' and started_at >= '%s'
) r`

// loadCalls reads the llm spans since the given time (zero = all) from
// the stack's database. tapes stores spans in dated partitions
// (spans_YYYYMMDD) without a parent table, so the query unions them.
func loadCalls(ctx context.Context, st *stack.Stack, since time.Time) ([]spend.Call, error) {
	out, err := st.PSQL(ctx, "select table_name from information_schema.tables where table_schema = 'public' and table_name ~ '^spans_[0-9]{8}$' order by 1")
	if err != nil {
		return nil, fmt.Errorf("cannot read the tapes database (is the stack up? run tapes-test first): %w", err)
	}
	parts := strings.Fields(out)
	if len(parts) == 0 {
		return nil, nil
	}
	selects := make([]string, len(parts))
	for i, p := range parts {
		selects[i] = "select * from " + p
	}
	if since.IsZero() {
		since = time.Unix(0, 0)
	}
	sql := fmt.Sprintf(callsQuery, strings.Join(selects, " union all "), since.UTC().Format(time.RFC3339))
	out, err = st.PSQL(ctx, sql)
	if err != nil {
		return nil, err
	}
	return parseCalls(out)
}

// parseCalls decodes the JSON array callsQuery prints.
func parseCalls(out string) ([]spend.Call, error) {
	var calls []spend.Call
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &calls); err != nil {
		return nil, fmt.Errorf("unexpected psql output: %w", err)
	}
	return calls, nil
}

func printSpend(r spend.Result, empty bool) {
	if empty {
		fmt.Println("no LLM calls imported yet; run tapes-test first")
		return
	}
	dim, strong := outUI.dim, outUI.strong
	fmt.Printf("%-18s %s\n", "LLM calls:", commas(r.Calls))
	fmt.Printf("%-18s %s\n", "Total cost:", money(r.TotalCost))
	share := 0.0
	if r.TotalCost > 0 {
		share = r.FrontierCost / r.TotalCost
	}
	fmt.Printf("%-18s %s %s\n", "Frontier cost:", money(r.FrontierCost), dim.Render(fmt.Sprintf("(%.1f%% of total)", 100*share)))
	fmt.Printf("%-18s %s %s\n", strong.Render("RoutableSpend:"), strong.Render(money(r.Routable)), dim.Render(fmt.Sprintf("(%.1f%% of frontier)", 100*r.RoutableShare())))

	fmt.Printf("\n%s\n", dim.Render("By signal (target model):"))
	for _, s := range spend.Signals {
		b := r.BySignal[s]
		if b == nil {
			b = &spend.Bucket{}
		}
		fmt.Printf("  %-10s -> %-18s %8s calls %s  %12s spent  %12s routable\n",
			s, spend.Targets[s], commas(b.Calls), dim.Render(fmt.Sprintf("(%s switches)", commas(b.Switches))), money(b.Cost), money(b.Routable))
	}
	fmt.Printf("\n%s\n", dim.Render("By frontier model:"))
	models := make([]string, 0, len(r.ByModel))
	for m := range r.ByModel {
		models = append(models, m)
	}
	sort.Slice(models, func(i, j int) bool { return r.ByModel[models[i]].Routable > r.ByModel[models[j]].Routable })
	if len(models) == 0 {
		fmt.Printf("  %s\n", dim.Render("none of the frontier calls is routable"))
	}
	for _, m := range models {
		b := r.ByModel[m]
		fmt.Printf("  %-32s %8s calls  %12s spent  %12s routable\n", m, commas(b.Calls), money(b.Cost), money(b.Routable))
	}
	fmt.Printf("\nSkipped %s candidate call(s) where the switch cost ate the savings.\n", commas(r.NotWorthSwitching))
	if len(r.Unpriced) > 0 {
		fmt.Printf("Skipped (no price): %s\n", unpricedList(r.Unpriced))
	}
	fmt.Printf("Not computed: %s.\n", spend.NotComputed)
	fmt.Println(dim.Render("List prices; check them against the pricing page before quoting a number. The switch back to the frontier model is not charged, so this leans high."))
}

// spendJSON is the shape of `spend --json`, the same keys as the
// derived-metrics prototype prints.
type spendJSON struct {
	Calls                   int                     `json:"calls"`
	TotalCost               float64                 `json:"total_cost"`
	FrontierCost            float64                 `json:"frontier_cost"`
	RoutableSpend           float64                 `json:"routable_spend"`
	RoutableShareOfFrontier float64                 `json:"routable_share_of_frontier"`
	BySignal                map[spend.Signal]bucket `json:"by_signal"`
	ByModel                 map[string]bucket       `json:"by_model"`
	Targets                 map[spend.Signal]string `json:"targets"`
	NotWorthSwitching       int                     `json:"not_worth_switching"`
	UnpricedModels          map[string]int          `json:"unpriced_models"`
	NotComputed             []string                `json:"not_computed"`
}

type bucket struct {
	Calls    int     `json:"calls"`
	Switches int     `json:"switches"`
	Cost     float64 `json:"cost"`
	Routable float64 `json:"routable"`
}

func printSpendJSON(r spend.Result) error {
	round := func(f float64) float64 { return math.Round(f*100) / 100 }
	j := spendJSON{
		Calls:                   r.Calls,
		TotalCost:               round(r.TotalCost),
		FrontierCost:            round(r.FrontierCost),
		RoutableSpend:           round(r.Routable),
		RoutableShareOfFrontier: math.Round(r.RoutableShare()*10000) / 10000,
		BySignal:                map[spend.Signal]bucket{},
		ByModel:                 map[string]bucket{},
		Targets:                 spend.Targets,
		NotWorthSwitching:       r.NotWorthSwitching,
		UnpricedModels:          r.Unpriced,
		NotComputed:             []string{spend.NotComputed},
	}
	for s, b := range r.BySignal {
		j.BySignal[s] = bucket{b.Calls, b.Switches, round(b.Cost), round(b.Routable)}
	}
	for m, b := range r.ByModel {
		j.ByModel[m] = bucket{b.Calls, b.Switches, round(b.Cost), round(b.Routable)}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(j)
}

func unpricedList(m map[string]int) string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool { return m[names[i]] > m[names[j]] })
	parts := make([]string, len(names))
	for i, n := range names {
		if n == "" {
			n = "(blank)"
		}
		parts[i] = fmt.Sprintf("%s %s", n, commas(m[n]))
	}
	return strings.Join(parts, ", ")
}

// money formats dollars with thousands separators and cents.
func money(f float64) string {
	whole := int(math.Floor(math.Abs(f)))
	cents := int(math.Round((math.Abs(f) - float64(whole)) * 100))
	if cents == 100 {
		whole, cents = whole+1, 0
	}
	sign := ""
	if f < 0 {
		sign = "-"
	}
	return fmt.Sprintf("%s$%s.%02d", sign, commas(whole), cents)
}

// commas is n with thousands separators.
func commas(n int) string {
	s := fmt.Sprint(n)
	if n < 0 {
		return "-" + commas(-n)
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
