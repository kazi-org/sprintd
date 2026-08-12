// Package allocator picks which Claude account a lane is dispatched against.
package allocator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/kazi-org/sprintd/internal/sprint"
)

// HardStopPct is the weekly utilisation at which an account stops receiving
// lanes outright, whatever its reserve floor says.
const HardStopPct = 85.0

// ErrNoAccount is returned when every account is either past the hard stop or
// below its reserve floor.
var ErrNoAccount = errors.New("no account is eligible")

// Usage is one account's observed consumption.
type Usage struct {
	WeeklyTokens       int64
	WeeklyCostUSD      float64
	ActiveBlockTokens  int64
	ActiveBlockCostUSD float64
}

// Reader reads one account's usage. sprintd's production implementation shells
// out to ccusage; tests supply their own.
type Reader interface {
	Read(ctx context.Context, acct sprint.Account) (Usage, error)
}

// Allocator hands out account assignments under the floor and hard-stop rules.
//
// It is safe for concurrent use: lanes are dispatched from separate goroutines
// and each asks for an account as it starts.
type Allocator struct {
	mu       sync.Mutex
	accounts []sprint.Account
	usage    map[string]Usage
	metered  map[string]bool
	assigned map[string]int
	next     int
	log      *slog.Logger
}

// New builds an Allocator and reads each account's current usage once.
//
// Usage is sampled at the start of the run rather than before every lane: a
// sprint is minutes-to-hours long, ccusage costs a subprocess and a JSONL scan
// per call, and the floor exists to protect a reserve, not to meter to the
// token. An account whose usage cannot be read is still usable, but only after
// every account with a readable, in-budget figure -- and the reason is logged.
func New(ctx context.Context, accounts []sprint.Account, reader Reader, log *slog.Logger) *Allocator {
	if log == nil {
		log = slog.Default()
	}
	a := &Allocator{
		accounts: accounts,
		usage:    make(map[string]Usage, len(accounts)),
		metered:  make(map[string]bool, len(accounts)),
		assigned: make(map[string]int, len(accounts)),
		log:      log,
	}
	for _, acct := range accounts {
		if acct.WeeklyTokenLimit <= 0 {
			log.Warn("account has no weekly_token_limit; reserve floor and hard stop cannot be enforced for it",
				"account", acct.Name)
			continue
		}
		if reader == nil {
			continue
		}
		usage, err := reader.Read(ctx, acct)
		if err != nil {
			log.Warn("could not read usage; falling back to round-robin for this account",
				"account", acct.Name, "error", err)
			continue
		}
		a.usage[acct.Name] = usage
		a.metered[acct.Name] = true
	}
	if len(a.metered) == 0 {
		log.Warn("no account has readable usage; assigning lanes round-robin without floor or hard-stop enforcement",
			"accounts", len(accounts))
	}
	return a
}

// WeeklyPct returns an account's weekly utilisation and whether it is known.
func (a *Allocator) WeeklyPct(name string) (float64, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.weeklyPct(name)
}

func (a *Allocator) weeklyPct(name string) (float64, bool) {
	if !a.metered[name] {
		return 0, false
	}
	for _, acct := range a.accounts {
		if acct.Name != name {
			continue
		}
		if acct.WeeklyTokenLimit <= 0 {
			return 0, false
		}
		return float64(a.usage[name].WeeklyTokens) / float64(acct.WeeklyTokenLimit) * 100, true
	}
	return 0, false
}

// Assign picks the account for the next lane.
//
// Metered accounts win: among those still under the hard stop and still above
// their reserve floor, the least-consumed one goes first, with ties broken by
// how many lanes each has already taken this run so a sprint spreads rather
// than piling onto one account. Unmetered accounts are the fallback, taken
// round-robin, and only once no metered account can take the work.
func (a *Allocator) Assign() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	type candidate struct {
		name     string
		pct      float64
		assigned int
	}
	var eligible []candidate
	var unmetered []string
	for _, acct := range a.accounts {
		pct, known := a.weeklyPct(acct.Name)
		if !known {
			unmetered = append(unmetered, acct.Name)
			continue
		}
		if pct >= HardStopPct {
			a.log.Debug("account past hard stop", "account", acct.Name, "weekly_pct", pct)
			continue
		}
		if remaining := 100 - pct; remaining <= acct.ReserveFloorPct {
			a.log.Debug("account below reserve floor", "account", acct.Name,
				"remaining_pct", remaining, "floor_pct", acct.ReserveFloorPct)
			continue
		}
		eligible = append(eligible, candidate{acct.Name, pct, a.assigned[acct.Name]})
	}

	if len(eligible) > 0 {
		sort.Slice(eligible, func(i, j int) bool {
			if eligible[i].pct != eligible[j].pct {
				return eligible[i].pct < eligible[j].pct
			}
			if eligible[i].assigned != eligible[j].assigned {
				return eligible[i].assigned < eligible[j].assigned
			}
			return eligible[i].name < eligible[j].name
		})
		name := eligible[0].name
		a.assigned[name]++
		return name, nil
	}
	if len(unmetered) > 0 {
		name := unmetered[a.next%len(unmetered)]
		a.next++
		a.assigned[name]++
		return name, nil
	}
	return "", fmt.Errorf("%w: all %d accounts are past the %.0f%% hard stop or below their reserve floor",
		ErrNoAccount, len(a.accounts), HardStopPct)
}

// Account returns the full account record for a name.
func (a *Allocator) Account(name string) (sprint.Account, bool) {
	for _, acct := range a.accounts {
		if acct.Name == name {
			return acct, true
		}
	}
	return sprint.Account{}, false
}
