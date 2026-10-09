// Package deckui is the session dashboard behind `tapes-test deck`.
//
// It is the deck TUI from pcc-labs/paper-console, which is private and
// published nowhere else, so it is carried here as source rather than
// imported. paper-console reads an org's sessions through paperd; this copy
// reads the tapes stack tapes-test runs, and drops what only made sense
// there: the paperd preflight hints, the paper-chat pane and its `c` key,
// --web, and the paper branding. The data layer is the public
// github.com/papercomputeco/tapes pkg/deck, unmodified.
package deckui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/papercomputeco/tapes/pkg/deck"
	"github.com/papercomputeco/tapes/pkg/sessions"
)

const sortDirDesc = "desc"

// TUI branding, read by the overview header and the session breadcrumb.
var (
	brandTitle      = "tapes-test deck"
	brandBreadcrumb = "tapes"
)

// Options are the deck's flags. Empty strings take the defaults.
type Options struct {
	APITarget string // tapes read API
	Pricing   string // pricing JSON overrides
	Since     string // look-back duration, e.g. 24h
	From, To  string // YYYY-MM-DD or RFC3339
	Sort      string // cost|date|tokens|duration
	SortDir   string // asc|desc
	Model     string
	Status    string // completed|failed|abandoned
	Project   string
	Session   string // open straight into one session
	Refresh   uint   // auto-refresh seconds, 0 to disable
	Theme     string // dark|light, auto-detected when empty
}

// Run checks the API answers, then hands the terminal to the TUI until the
// user quits.
func Run(ctx context.Context, o Options) error {
	switch o.Theme {
	case "":
	case "dark", "light":
		themeOverride = o.Theme
		if isDarkTheme() {
			applyPalette(darkPalette)
		} else {
			applyPalette(lightPalette)
		}
	default:
		return fmt.Errorf("invalid --theme value %q: expected dark or light", o.Theme)
	}

	pricing, err := sessions.LoadPricing(o.Pricing)
	if err != nil {
		return err
	}

	apiTarget := normalizeAPITarget(o.APITarget)
	// Fail before the TUI grabs the terminal, not as an opaque fetch error
	// inside an empty dashboard.
	if err := preflight(ctx, apiTarget); err != nil {
		return err
	}

	filters, err := parseFilters(o)
	if err != nil {
		return err
	}
	refresh, err := refreshDuration(o.Refresh)
	if err != nil {
		return err
	}
	return RunDeckTUI(ctx, deck.NewHTTPQuery(apiTarget, pricing), filters, refresh)
}

// preflight probes the stats endpoint the overview opens with.
func preflight(ctx context.Context, apiTarget string) error {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, apiTarget+"/v1/stats", nil)
	if err != nil {
		return fmt.Errorf("invalid --api-target %q: %w", apiTarget, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach the tapes API at %s: %w\n\nThe stack is not up. Run `tapes-test` to start it and import your sessions.", apiTarget, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the tapes API at %s answered HTTP %d for /v1/stats\n\nCheck that %s is a tapes server and that it is healthy.",
			apiTarget, resp.StatusCode, apiTarget)
	}
	return nil
}

func refreshDuration(refresh uint) (time.Duration, error) {
	if refresh == 0 {
		return 0, nil
	}

	maxSeconds := uint64(int64(^uint64(0)>>1) / int64(time.Second))
	refreshSeconds := uint64(refresh)
	if refreshSeconds > maxSeconds {
		return 0, errors.New("refresh exceeds maximum duration")
	}

	return time.Duration(int64(refreshSeconds)) * time.Second, nil
}

func parseFilters(o Options) (deck.Filters, error) {
	filters := deck.Filters{
		Sort:    strings.ToLower(strings.TrimSpace(o.Sort)),
		SortDir: strings.ToLower(strings.TrimSpace(o.SortDir)),
		Model:   strings.TrimSpace(o.Model),
		Status:  strings.TrimSpace(o.Status),
		Project: strings.TrimSpace(o.Project),
		Session: strings.TrimSpace(o.Session),
	}

	if filters.Sort == "" {
		filters.Sort = "date"
	}
	if filters.SortDir == "" {
		filters.SortDir = sortDirDesc
	}

	if o.Since != "" {
		duration, err := time.ParseDuration(o.Since)
		if err != nil {
			return filters, fmt.Errorf("invalid since duration: %w", err)
		}
		filters.Since = duration
	} else if o.From == "" && o.To == "" {
		// Bound the default overview to a recent window so the API-backed deck
		// stays snappy on large stores when no explicit time filter is provided.
		filters.Since = 30 * 24 * time.Hour
	}

	if o.From != "" {
		parsed, err := parseTime(o.From)
		if err != nil {
			return filters, fmt.Errorf("invalid from time: %w", err)
		}
		filters.From = &parsed
	}

	if o.To != "" {
		parsed, err := parseTime(o.To)
		if err != nil {
			return filters, fmt.Errorf("invalid to time: %w", err)
		}
		filters.To = &parsed
	}

	return filters, nil
}

func normalizeAPITarget(target string) string {
	target = strings.TrimRight(strings.TrimSpace(target), "/")
	if !strings.Contains(target, "://") {
		return "http://" + target
	}
	return target
}

func parseTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, errors.New("empty time")
	}

	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed, nil
	}

	if parsed, err := time.Parse("2006-01-02", value); err == nil {
		return parsed, nil
	}

	return time.Time{}, errors.New("expected RFC3339 or YYYY-MM-DD")
}
