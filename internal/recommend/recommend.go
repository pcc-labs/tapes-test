// Package recommend finds repeated work in a tapes corpus, the way the
// console's Skills page does.
//
// It is a port of the console's detector (`src/lib/skills/suggest-detect.ts`,
// console PR #268): tokenize each session, cluster on Jaccard >= 0.15 with
// union-find, apply the session floor, match clusters against the skills the
// deployment already holds, template the label. No model call; seconds for a
// week of sessions.
//
// One substitution: the console clusters on the summary cassette's topics.
// This tool has no summary cassette, so it tokenizes what the read API
// already holds for every session: each turn's user prompt and response
// preview and the tool names. Same pass, cheaper input, so the clusters are
// comparable but not identical to the console's. Two consequences: a list of
// conversational filler joins the console's stop words, and a session with
// fewer than MinTokens distinct tokens (a "hi") is left out. The project
// directory is not a token, for the console's reason: in a monorepo-shaped
// corpus every session shares it and one edge chains the whole window into a
// single cluster.
//
// The author floor is one: a laptop corpus is one person's sessions. The
// console's two-author floor is a team signal.
package recommend

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const (
	JaccardThreshold = 0.15
	MinSessions      = 3
	MinTokens        = 10
	skillMatchTokens = 3
	maxPerKind       = 3
	// A token in most of the corpus says nothing about any one session.
	// Codex runs `exec` in nearly every session, so without this the tool
	// names alone chain the whole window into one cluster.
	maxDocFrequency = 0.5
	dfMinSessions   = 10
	// A cluster has to be about something: this many tokens shared by two
	// or more of its sessions, or it is incidental overlap.
	minSharedTokens = 3
)

var procedural = set("install deploy clearing bring release migrate")

// autoReviewModels are Codex reviewing its own runs in sessions of their
// own: every LLM call on this model and every prompt "The following is the
// Codex agent history…". They are the harness talking to itself, not the
// person's work, and nine of them cluster on their shared preamble in any
// week.
var autoReviewModels = set("codex-auto-review")

var stopWords = set(
	"a an and the of for to in on with via into from by at as is are be or setup set up using use " +
		"new add adding support work workflow workflows improvement improvements integration " +
		"integrations strategy strategies method methods planning plan poc proof concept production " +
		"deployment paper tenant session sessions")

// promptNoise sits on top of stopWords. The console clusters curated
// summary topics, which hold no function words. Raw prompts are prose, so
// ordinary English carries no signal here and, worse, lands in the label,
// which ranks by frequency and breaks ties alphabetically ("Actions Added
// All Also").
var promptNoise = set(
	// pronouns, articles, prepositions, conjunctions, auxiliaries
	"i me my we our you your it its he she they them their this that these those there here " +
		"all also any both each few more most other some such own same than too very " +
		"about above after again against back before below between down during into off once only " +
		"out over under until upon while within without through across along around " +
		"and but nor yet so because if then else when where why how what which who whom whose " +
		"am are was were been being have has had having do does did doing done " +
		"can could may might must shall should will would let lets " +
		// conversational and prompt scaffolding
		"please help thanks thank hi hello hey yes yeah no not okay ok sure " +
		"want need like just make made making get got give given take taken go going come " +
		"look looking see seen say said tell told think thought know known try trying " +
		"now today tomorrow yesterday still already next last first second thing things " +
		"one two three good great better best nice sorry maybe actually really " +
		// artifacts of pasted context and links
		"image images path paths file files https http com www href src url link links " +
		"context source block blocks line lines")

// Letters only. The console tokenizes [a-z0-9]+ over curated topics; over
// raw prompts the digits are ids, hashes, PR numbers and pasted window
// dimensions ("853x863"), which cluster and label as if they meant something.
var word = regexp.MustCompile(`[a-z]+`)

// Set is a token set.
type Set map[string]struct{}

func set(words string) Set {
	out := Set{}
	for _, w := range strings.Fields(words) {
		out[w] = struct{}{}
	}
	return out
}

func (s Set) has(w string) bool { _, ok := s[w]; return ok }

// Tokenize lowercases texts and keeps words over two letters that are not
// stop words or noise.
func Tokenize(texts []string, noise Set) Set {
	out := Set{}
	for _, text := range texts {
		for _, w := range word.FindAllString(strings.ToLower(text), -1) {
			if len(w) > 2 && !stopWords.has(w) && !noise.has(w) {
				out[w] = struct{}{}
			}
		}
	}
	return out
}

func jaccard(a, b Set) float64 {
	shared := 0
	for w := range a {
		if b.has(w) {
			shared++
		}
	}
	union := len(a) + len(b) - shared
	if union == 0 {
		return 0
	}
	return float64(shared) / float64(union)
}

// Session is what the detector needs to know about one session.
type Session struct {
	ID      string
	Title   string
	Author  string
	Project string
	Tokens  Set
}

// Skill is an existing skill on the deployment.
type Skill struct {
	ID, Slug, Name, Description string
	Tags                        []string
	SourceSessionIDs            []string
}

// Suggestion is one cluster worth acting on.
type Suggestion struct {
	Kind       string // "new" or "evaluate"
	Title      string
	Why        string
	TypeHint   string
	SessionIDs []string
	Skill      *Skill // set for "evaluate"
	Topic      []string
}

// TraceText is the input SessionTokens reads: one turn's prompt, preview,
// and its spans' tool names and models.
type TraceText struct {
	UserPrompt      string
	ResponsePreview string
	ToolNames       []string
	Models          []string
}

// SessionTokens builds a session's token set from its turns. Prompts,
// previews, and tool names stand in for summary topics. Empty when the
// session is only the harness reviewing itself.
func SessionTokens(turns []TraceText) Set {
	var texts []string
	models := Set{}
	for _, t := range turns {
		texts = append(texts, t.UserPrompt, t.ResponsePreview)
		texts = append(texts, t.ToolNames...)
		for _, m := range t.Models {
			models[m] = struct{}{}
		}
	}
	if len(models) > 0 {
		onlyAuto := true
		for m := range models {
			if !autoReviewModels.has(m) {
				onlyAuto = false
			}
		}
		if onlyAuto {
			return Set{}
		}
	}
	return Tokenize(texts, promptNoise)
}

func documentFrequency(pool map[string]Set) map[string]int {
	df := map[string]int{}
	for _, tokens := range pool {
		for t := range tokens {
			df[t]++
		}
	}
	return df
}

// commonTokens are in more than maxDocFrequency of the sessions and carry
// no signal. Too few sessions and a common word cannot be told from a
// shared topic, so nothing is dropped.
func commonTokens(pool map[string]Set) Set {
	out := Set{}
	if len(pool) < dfMinSessions {
		return out
	}
	limit := maxDocFrequency * float64(len(pool))
	for t, n := range documentFrequency(pool) {
		if float64(n) > limit {
			out[t] = struct{}{}
		}
	}
	return out
}

// clusterSessions is union-find over pairs whose overlap clears the
// threshold. Members and clusters come back in id order.
func clusterSessions(pool map[string]Set, threshold float64) [][]string {
	ids := make([]string, 0, len(pool))
	for id := range pool {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	parent := map[string]string{}
	for _, id := range ids {
		parent[id] = id
	}
	var find func(string) string
	find = func(x string) string {
		if parent[x] != x {
			parent[x] = find(parent[x])
		}
		return parent[x]
	}
	for i, a := range ids {
		for _, b := range ids[i+1:] {
			if jaccard(pool[a], pool[b]) >= threshold {
				parent[find(a)] = find(b)
			}
		}
	}
	groups := map[string][]string{}
	var order []string
	for _, id := range ids {
		root := find(id)
		if _, seen := groups[root]; !seen {
			order = append(order, root)
		}
		groups[root] = append(groups[root], id)
	}
	out := make([][]string, 0, len(order))
	for _, root := range order {
		out = append(out, groups[root])
	}
	return out
}

// Detect returns at most three "evaluate" and three "new" suggestions,
// largest clusters first.
func Detect(sessions []Session, skills []Skill, minSessions int) []Suggestion {
	byID := map[string]Session{}
	for _, s := range sessions {
		byID[s.ID] = s
	}
	sourced := Set{}
	for _, sk := range skills {
		for _, id := range sk.SourceSessionIDs {
			sourced[id] = struct{}{}
		}
	}
	pool := map[string]Set{}
	for _, s := range sessions {
		if !sourced.has(s.ID) && len(s.Tokens) > 0 {
			pool[s.ID] = s.Tokens
		}
	}
	everywhere := commonTokens(pool)
	for id, tokens := range pool {
		kept := Set{}
		for t := range tokens {
			if !everywhere.has(t) {
				kept[t] = struct{}{}
			}
		}
		if len(kept) == 0 {
			delete(pool, id)
		} else {
			pool[id] = kept
		}
	}
	corpusDF := documentFrequency(pool)

	var out []Suggestion
	covered := Set{}
	for i := range skills {
		sk := &skills[i]
		skillTokens := Tokenize(append([]string{sk.Name, sk.Description}, sk.Tags...), nil)
		if len(skillTokens) == 0 {
			continue
		}
		var matched []string
		for id, tokens := range pool {
			shared := 0
			for t := range skillTokens {
				if tokens.has(t) {
					shared++
				}
			}
			if jaccard(skillTokens, tokens) >= JaccardThreshold || shared >= skillMatchTokens {
				matched = append(matched, id)
			}
		}
		if len(matched) < 2 {
			continue
		}
		sort.Strings(matched)
		for _, id := range matched {
			covered[id] = struct{}{}
		}
		out = append(out, Suggestion{
			Kind:       "evaluate",
			Title:      truncate("Evaluate "+sk.Name, 60),
			Why:        fmt.Sprintf("%d sessions did work %s covers. Evaluate it against them.", len(matched), sk.Name),
			TypeHint:   "workflow",
			SessionIDs: matched,
			Skill:      sk,
		})
	}

	// New skills only from work no existing skill covers.
	for _, members := range clusterSessions(pool, JaccardThreshold) {
		if len(members) < minSessions {
			continue
		}
		skip := false
		for _, id := range members {
			if covered.has(id) {
				skip = true
			}
		}
		if skip {
			continue
		}
		counts := map[string]int{}
		for _, id := range members {
			for t := range pool[id] {
				counts[t]++
			}
		}
		// A token in one session of the cluster is that session's own word.
		// What the cluster is about is what at least two of them said. Rank
		// by how much of the cluster said it, then by how little of the rest
		// of the corpus did: without the second key an alphabetical tie
		// break names every cluster "Actions Added All Also".
		type tokenCount struct {
			token string
			n     int
		}
		var shared []tokenCount
		for t, n := range counts {
			if n >= 2 {
				shared = append(shared, tokenCount{t, n})
			}
		}
		if len(shared) < minSharedTokens {
			continue // incidental overlap, not repeated work
		}
		sort.Slice(shared, func(i, j int) bool {
			a, b := shared[i], shared[j]
			if a.n != b.n {
				return a.n > b.n
			}
			if corpusDF[a.token] != corpusDF[b.token] {
				return corpusDF[a.token] < corpusDF[b.token]
			}
			return a.token < b.token
		})
		top := make([]string, 0, 4)
		for _, tc := range shared[:min(4, len(shared))] {
			top = append(top, tc.token)
		}
		topic := strings.Join(top, " ")
		people := Set{}
		for _, id := range members {
			people[byID[id].Author] = struct{}{}
		}
		who := "people"
		if len(people) == 1 {
			who = "person"
		}
		hint := "workflow"
		for _, t := range top {
			if procedural.has(t) {
				hint = "procedure"
			}
		}
		out = append(out, Suggestion{
			Kind:       "new",
			Title:      truncate(titleCase(topic), 60),
			Why:        fmt.Sprintf("%d sessions by %d %s worked on %s with no skill to hand off.", len(members), len(people), who, topic),
			TypeHint:   hint,
			SessionIDs: members,
			Topic:      top,
		})
	}

	sort.SliceStable(out, func(i, j int) bool { return len(out[i].SessionIDs) > len(out[j].SessionIDs) })
	perKind := map[string]int{}
	var kept []Suggestion
	for _, s := range out {
		if perKind[s.Kind] >= maxPerKind {
			continue
		}
		perKind[s.Kind]++
		kept = append(kept, s)
	}
	return kept
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func titleCase(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}
