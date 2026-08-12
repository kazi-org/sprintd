package allocator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	"github.com/kazi-org/sprintd/internal/sprint"
)

// DefaultTimeout bounds a single ccusage invocation. The first npx-resolved
// run has to fetch the package, so the budget is generous.
const DefaultTimeout = 90 * time.Second

// CCUsage reads account usage by shelling out to ccusage.
//
// ccusage aggregates the JSONL transcripts under a config directory, so usage
// is read per account by pointing CLAUDE_CONFIG_DIR at that account's
// directory -- the same mechanism Claude Code itself uses to keep concurrently
// usable accounts apart. It follows that the figures only cover work done on
// this machine under that directory; usage from another host is invisible.
type CCUsage struct {
	// Argv is the ccusage command, for example ["ccusage"] or
	// ["npx", "-y", "ccusage@latest"].
	Argv []string
	// Timeout bounds each invocation. Zero means DefaultTimeout.
	Timeout time.Duration
}

// DetectCCUsage returns a CCUsage using the ccusage binary when it is on PATH,
// and npx otherwise.
func DetectCCUsage() *CCUsage {
	if path, err := exec.LookPath("ccusage"); err == nil {
		return &CCUsage{Argv: []string{path}}
	}
	return &CCUsage{Argv: []string{"npx", "-y", "ccusage@latest"}}
}

// weeklyReport mirrors the subset of `ccusage weekly --json` sprintd reads.
type weeklyReport struct {
	Weekly []struct {
		Period      string  `json:"period"`
		TotalTokens int64   `json:"totalTokens"`
		TotalCost   float64 `json:"totalCost"`
	} `json:"weekly"`
}

// blocksReport mirrors the subset of `ccusage blocks --active --json`.
type blocksReport struct {
	Blocks []struct {
		IsActive    bool    `json:"isActive"`
		TotalTokens int64   `json:"totalTokens"`
		CostUSD     float64 `json:"costUSD"`
	} `json:"blocks"`
}

// Read returns the account's current weekly total and active five-hour block.
func (c *CCUsage) Read(ctx context.Context, acct sprint.Account) (Usage, error) {
	var usage Usage

	var weekly weeklyReport
	if err := c.runJSON(ctx, acct, &weekly, "weekly", "--json"); err != nil {
		return Usage{}, err
	}
	if len(weekly.Weekly) == 0 {
		return Usage{}, fmt.Errorf("ccusage reported no weekly buckets for account %s", acct.Name)
	}
	latest := weekly.Weekly[0]
	for _, w := range weekly.Weekly[1:] {
		if w.Period > latest.Period {
			latest = w
		}
	}
	usage.WeeklyTokens = latest.TotalTokens
	usage.WeeklyCostUSD = latest.TotalCost

	// The active five-hour block is advisory: a run should still proceed on a
	// weekly figure alone if the blocks view is unavailable.
	var blocks blocksReport
	if err := c.runJSON(ctx, acct, &blocks, "blocks", "--active", "--json"); err == nil {
		for _, b := range blocks.Blocks {
			if b.IsActive {
				usage.ActiveBlockTokens = b.TotalTokens
				usage.ActiveBlockCostUSD = b.CostUSD
				break
			}
		}
	}
	return usage, nil
}

func (c *CCUsage) runJSON(ctx context.Context, acct sprint.Account, into any, args ...string) error {
	if len(c.Argv) == 0 {
		return fmt.Errorf("reading usage for account %s: no ccusage command configured", acct.Name)
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.Argv[0], append(append([]string{}, c.Argv[1:]...), args...)...)
	if acct.ConfigDir != "" {
		cmd.Env = append(cmd.Environ(), "CLAUDE_CONFIG_DIR="+acct.ConfigDir)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("running ccusage %v for account %s: %w: %s",
			args, acct.Name, err, trimTail(stderr.String(), 400))
	}
	if err := json.Unmarshal(stdout.Bytes(), into); err != nil {
		return fmt.Errorf("parsing ccusage %v output for account %s: %w", args, acct.Name, err)
	}
	return nil
}

func trimTail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return "…" + s[len(s)-max:]
}
