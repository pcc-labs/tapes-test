// Package spend computes RoutableSpend: the dollars of frontier-model spend
// that could move to a more efficient model without hurting outcomes.
//
//	RoutableSpend = Σ cost(c) × (1 − price_efficient / price_frontier)
//	                over frontier calls c whose task class is efficient-proven
//
// It is the Go form of pcc-labs/derived-metrics' routable_spend.py (issue
// #1, PR #2), and computes two of its three signals:
//
//  1. overhead turns (call_kind offshoot:* or injected:*) on a frontier
//     model, routed to Haiku 4.5;
//  3. frontier calls with small output whose tool calls are all read-only,
//     routed to Sonnet 5.5.
//
// Signal 2, recurring task clusters proven on efficient models, needs an
// outcome comparison per cluster and is not computed.
//
// Cost is priced per token type (fresh input, cache write, cache read,
// output). Caches are per model, so a routed call that switches models
// mid-thread cannot read the frontier model's cached prefix: it pays the
// target's cache-write rate on its whole input. Consecutive routed calls in
// the same thread on the same target read that cache normally. A call only
// counts when routing it saves money after the switch cost. The switch back
// to the frontier model is not charged (its cache is often still warm), so
// the estimate leans high.
package spend

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/papercomputeco/tapes/pkg/sessions"
)

// Signal is why a call is routable.
type Signal string

const (
	// Overhead is a harness-generated turn (offshoot:*, injected:*).
	Overhead Signal = "overhead"
	// ReadOnly is a small-output call whose tools only read.
	ReadOnly Signal = "read_only"
)

// Signals in report order.
var Signals = []Signal{Overhead, ReadOnly}

// NotComputed names the signal this version leaves out.
const NotComputed = "signal 2: recurring task clusters proven on efficient models"

// Tool is one tool_use block in a call's output.
type Tool struct {
	Name    string `json:"name"`
	Command string `json:"command"` // Bash / exec_command only
}

// Call is one kind='llm' span.
type Call struct {
	SessionID  string    `json:"session_id"`
	ThreadID   string    `json:"thread_id"`
	StartedAt  time.Time `json:"started_at"`
	Model      string    `json:"model"`
	CallKind   string    `json:"call_kind"`
	Prompt     int64     `json:"prompt"`
	Completion int64     `json:"completion"`
	CacheRead  int64     `json:"cache_read"`
	CacheWrite int64     `json:"cache_write"`
	Tools      []Tool    `json:"tools"`
}

// Options tune the computation. Zero values take the defaults below.
type Options struct {
	Pricing  sessions.PricingTable
	Frontier map[string]bool   // normalized model names
	Targets  map[Signal]string // normalized model name per signal
	// SmallOutput is the most completion tokens a read-only call may have
	// (default 400).
	SmallOutput int64
}

// Frontier is the frontier set, the same as D2 (frontier-carry ratio).
var Frontier = map[string]bool{
	"claude-fable-5":   true,
	"claude-fable-5.1": true,
	"claude-opus-5":    true,
	"claude-opus-5.5":  true,
}

// Targets is the efficient model each signal routes to.
var Targets = map[Signal]string{
	Overhead: "claude-haiku-4.5",
	ReadOnly: "claude-sonnet-5.5",
}

// DefaultSmallOutput is the default Options.SmallOutput.
const DefaultSmallOutput = 400

// DefaultPricing is tapes' table with the Claude rates this metric prices
// brought up to date. Checked against the Claude pricing page on
// 2026-10-09; check again before quoting a number.
func DefaultPricing() sessions.PricingTable {
	p := sessions.DefaultPricing()
	p["claude-opus-5.5"] = sessions.Pricing{Input: 4.00, Output: 20.00, CacheRead: 0.20, CacheWrite: 5.00}
	p["claude-sonnet-5.5"] = sessions.Pricing{Input: 2.00, Output: 10.00, CacheRead: 0.20, CacheWrite: 2.50}
	p["claude-sonnet-5"] = sessions.Pricing{Input: 2.00, Output: 10.00, CacheRead: 0.20, CacheWrite: 2.50}
	return p
}

// LoadPricing is DefaultPricing with JSON overrides from path, the same
// file format `tapes-test deck --pricing` takes. An empty path is the
// default table.
func LoadPricing(path string) (sessions.PricingTable, error) {
	p := DefaultPricing()
	if path == "" {
		return p, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read pricing file: %w", err)
	}
	var overrides map[string]sessions.Pricing
	if err := json.Unmarshal(data, &overrides); err != nil {
		return nil, fmt.Errorf("parse pricing file: %w", err)
	}
	maps.Copy(p, overrides)
	return p, nil
}

// Bucket totals one slice of the routable calls.
type Bucket struct {
	Calls    int     `json:"calls"`
	Switches int     `json:"switches"` // calls that paid the cache switch
	Cost     float64 `json:"cost"`     // what they cost on the frontier model
	Routable float64 `json:"routable"` // what routing them would save
}

// Result is one RoutableSpend computation.
type Result struct {
	Calls             int
	TotalCost         float64 // every priced call
	FrontierCost      float64 // the priced calls on a frontier model
	Routable          float64
	BySignal          map[Signal]*Bucket
	ByModel           map[string]*Bucket // by normalized frontier model
	NotWorthSwitching int                // candidates the switch cost ruled out
	Unpriced          map[string]int     // calls skipped, by model
}

// RoutableShare is Routable as a fraction of FrontierCost.
func (r Result) RoutableShare() float64 {
	if r.FrontierCost == 0 {
		return 0
	}
	return r.Routable / r.FrontierCost
}

// Compute prices every call and finds the routable ones. Calls may come in
// any order; the switch-cost logic needs them by thread and time, so they
// are sorted first.
func Compute(calls []Call, o Options) Result {
	if o.Pricing == nil {
		o.Pricing = DefaultPricing()
	}
	if o.Frontier == nil {
		o.Frontier = Frontier
	}
	if o.Targets == nil {
		o.Targets = Targets
	}
	if o.SmallOutput == 0 {
		o.SmallOutput = DefaultSmallOutput
	}
	sorted := make([]Call, len(calls))
	copy(sorted, calls)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.SessionID != b.SessionID {
			return a.SessionID < b.SessionID
		}
		if a.ThreadID != b.ThreadID {
			return a.ThreadID < b.ThreadID
		}
		return a.StartedAt.Before(b.StartedAt)
	})

	r := Result{
		Calls:    len(calls),
		BySignal: map[Signal]*Bucket{},
		ByModel:  map[string]*Bucket{},
		Unpriced: map[string]int{},
	}
	type thread struct{ session, id string }
	warm := map[thread]string{} // thread -> target model holding a warm cache
	for _, c := range sorted {
		t := thread{c.SessionID, c.ThreadID}
		target := warm[t]
		delete(warm, t)
		price, ok := sessions.PricingForModel(o.Pricing, c.Model)
		if !ok {
			r.Unpriced[c.Model]++
			continue
		}
		spent := Cost(price, c)
		r.TotalCost += spent
		model := sessions.NormalizeModel(c.Model)
		if !o.Frontier[model] {
			continue
		}
		r.FrontierCost += spent
		s, ok := Classify(c, o.SmallOutput)
		if !ok {
			continue
		}
		to := o.Targets[s]
		toPrice, ok := o.Pricing[to]
		if !ok {
			continue
		}
		switched := target != to
		var routed float64
		if switched {
			routed = SwitchCost(toPrice, c)
		} else {
			routed = Cost(toPrice, c)
		}
		saved := spent - routed
		if saved <= 0 {
			r.NotWorthSwitching++
			continue
		}
		warm[t] = to
		r.Routable += saved
		for _, b := range []*Bucket{bucket(r.BySignal, s), bucket(r.ByModel, model)} {
			b.Calls++
			if switched {
				b.Switches++
			}
			b.Cost += spent
			b.Routable += saved
		}
	}
	return r
}

func bucket[K comparable](m map[K]*Bucket, k K) *Bucket {
	b := m[k]
	if b == nil {
		b = &Bucket{}
		m[k] = b
	}
	return b
}

// Cost is what c cost at price, each token type at its own rate.
func Cost(price sessions.Pricing, c Call) float64 {
	_, _, total := sessions.CostForTokensWithCache(price, c.Prompt, c.Completion, c.CacheWrite, c.CacheRead)
	return total
}

// SwitchCost is what c costs on a model with nothing cached: the whole
// input is a cache write.
func SwitchCost(price sessions.Pricing, c Call) float64 {
	input := max(c.Prompt, c.CacheRead+c.CacheWrite)
	return (float64(input)*price.CacheWrite + float64(c.Completion)*price.Output) / 1e6
}

// Classify says which signal, if any, makes c routable.
func Classify(c Call, smallOutput int64) (Signal, bool) {
	if strings.HasPrefix(c.CallKind, "offshoot:") || strings.HasPrefix(c.CallKind, "injected:") {
		return Overhead, true
	}
	if c.Completion <= smallOutput && IsReadOnly(c.Tools) {
		return ReadOnly, true
	}
	return "", false
}

var readOnlyTools = map[string]bool{
	"Read": true, "Grep": true, "Glob": true, "LS": true,
	"WebFetch": true, "WebSearch": true, "ToolSearch": true,
}

// shellTools run a command the heuristic can read: Claude Code's Bash and
// Codex's exec_command.
var shellTools = map[string]bool{"Bash": true, "exec_command": true}

var (
	readOnlyCommand = regexp.MustCompile(`^\s*(ls|cat|head|tail|wc|grep|rg|find|pwd|which|file|stat|tree|jq|sed -n|git (status|log|diff|show|branch|remote|ls-files|blame))\b`)
	shellSideEffect = regexp.MustCompile(`[>|;&]|\$\(`)
)

// IsReadOnly is true when every tool call only reads: a read tool, or a
// shell command from a list of read-only ones with no redirection, pipe,
// chaining, or substitution. A call with no tools is not read-only: it is
// a text answer, which this signal does not cover.
func IsReadOnly(tools []Tool) bool {
	if len(tools) == 0 {
		return false
	}
	for _, t := range tools {
		switch {
		case readOnlyTools[t.Name]:
		case shellTools[t.Name] && t.Command != "" && readOnlyCommand.MatchString(t.Command) && !shellSideEffect.MatchString(t.Command):
		default:
			return false
		}
	}
	return true
}
