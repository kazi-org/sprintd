// Package sprint parses and validates sprint files, the sole input to sprintd.
package sprint

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ErrInvalid is returned by Validate for any malformed sprint file. Callers
// branch on it with errors.Is; the wrapped message names the offending lane.
var ErrInvalid = errors.New("invalid sprint file")

// LocalHost is the machine host value meaning "run on this machine".
const LocalHost = "local"

// DefaultBase is the ref lanes branch from when a repo does not name one.
const DefaultBase = "origin/main"

// DefaultStaleThreshold is how many commits a checkout may sit behind its base
// before preflight fails.
//
// It is a hard failure rather than a warning on purpose. A checkout far behind
// its base makes a lane read stale source and branch from the wrong commit,
// and the lane reports success while doing it because nothing in its own view
// looks wrong. A warning for that gets scrolled past; the cost arrives later,
// as a confident but wrong result.
const DefaultStaleThreshold = 50

// refSafe matches names that are safe to embed in a git branch name. Lane ids
// and the sprint name both become part of one, so a name with a space or a
// leading dash would produce a branch git refuses to create -- or, worse, an
// argument git reads as a flag.
var refSafe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Duration wraps time.Duration so YAML scalars such as "90m" decode directly.
type Duration time.Duration

// UnmarshalYAML decodes a Go duration string into a Duration.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("decoding duration: %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("parsing duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalYAML renders the duration back to its string form.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// Std converts to a standard library duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Defaults supplies per-lane values omitted by individual lanes.
type Defaults struct {
	Model    string   `yaml:"model"`
	Deadline Duration `yaml:"deadline"`
	Retries  int      `yaml:"retries"`
}

// Machine is a dispatch target. Host is either LocalHost or an ssh destination.
type Machine struct {
	Host string `yaml:"host"`
}

// Local reports whether the machine runs commands without ssh.
func (m Machine) Local() bool { return m.Host == LocalHost }

// Account is one Claude account sprintd may pin a lane to.
//
// ConfigDir is the CLAUDE_CONFIG_DIR that holds the account's credentials; it
// is how Claude Code separates concurrently usable accounts. An empty
// ConfigDir means "the ambient default config dir" and at most one account may
// claim it, otherwise two accounts would be indistinguishable at dispatch.
//
// WeeklyTokenLimit is the denominator for utilisation percentages. ccusage
// reports consumption but knows nothing about plan entitlements, so the limit
// has to be declared here; without it sprintd cannot enforce the floor or the
// hard stop for that account and says so rather than guessing.
type Account struct {
	Name             string  `yaml:"name"`
	ReserveFloorPct  float64 `yaml:"reserve_floor_pct"`
	ConfigDir        string  `yaml:"config_dir"`
	WeeklyTokenLimit int64   `yaml:"weekly_token_limit"`
}

// Repo is a checkout lanes run inside, with its mergeable-throughput cap.
type Repo struct {
	Path          string `yaml:"path"`
	MaxConcurrent int    `yaml:"max_concurrent"`
	// Base is the ref every lane in this repo branches from, freshly fetched.
	// Defaults to DefaultBase.
	Base string `yaml:"base"`
	// StaleThreshold is how many commits the checkout may sit behind Base
	// before preflight fails. A pointer so that 0, meaning "must be exactly
	// current", is distinguishable from unset. Defaults to
	// DefaultStaleThreshold.
	StaleThreshold *int `yaml:"stale_threshold"`
}

// BaseRef returns the repo's base ref with the default applied.
func (r Repo) BaseRef() string {
	if r.Base == "" {
		return DefaultBase
	}
	return r.Base
}

// StaleLimit returns the repo's stale threshold with the default applied.
func (r Repo) StaleLimit() int {
	if r.StaleThreshold == nil {
		return DefaultStaleThreshold
	}
	return *r.StaleThreshold
}

// Lane is one deadline-bounded unit of agent work plus the predicate that
// decides, from outside the lane, whether it actually landed.
type Lane struct {
	ID   string `yaml:"id"`
	Repo string `yaml:"repo"`
	Goal string `yaml:"goal"`
	// Prompt is the text handed to claude -p. Mutually exclusive with Command.
	Prompt string `yaml:"prompt"`
	// Command, when set, is the lane's command verbatim, and Prompt and Model
	// are not used. It exists so a lane can run something that owns its own
	// agent loop -- kazi apply, a script, a build -- instead of sprintd
	// composing a claude invocation for it.
	Command   string   `yaml:"command"`
	Predicate string   `yaml:"predicate"`
	Machine   string   `yaml:"machine"`
	Model     string   `yaml:"model"`
	Deadline  Duration `yaml:"deadline"`
	Retries   *int     `yaml:"retries"`
	Needs     []string `yaml:"needs"`
}

// Composed reports whether sprintd builds this lane's command from a prompt,
// rather than the sprint file supplying it.
func (l Lane) Composed() bool { return strings.TrimSpace(l.Command) == "" }

// Sprint is a whole sprint file.
type Sprint struct {
	Name     string             `yaml:"sprint"`
	Opened   time.Time          `yaml:"opened"`
	Closes   time.Time          `yaml:"closes"`
	Defaults Defaults           `yaml:"defaults"`
	Machines map[string]Machine `yaml:"machines"`
	Accounts []Account          `yaml:"accounts"`
	Repos    map[string]Repo    `yaml:"repos"`
	Lanes    []Lane             `yaml:"lanes"`
}

// ResolvedLane is a lane with defaults applied and references dereferenced.
type ResolvedLane struct {
	Lane
	RepoPath      string
	MaxConcurrent int
	Host          string
	// Base is the ref this lane's worktree is branched from.
	Base string
	// Attempts is the total number of dispatches allowed: retries + 1.
	Attempts int
	Timeout  time.Duration
}

// Load reads, parses and validates the sprint file at path.
func Load(path string) (*Sprint, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading sprint file %s: %w", path, err)
	}
	return Parse(raw)
}

// Parse decodes and validates a sprint file's bytes.
func Parse(raw []byte) (*Sprint, error) {
	var s Sprint
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("parsing sprint file: %w", err)
	}
	if err := s.expandPaths(); err != nil {
		return nil, err
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

// expandPaths resolves a leading ~ in every filesystem path to the current
// user's home directory. Sprint files are written by hand and are shared
// across machines, so ~ is the portable spelling.
func (s *Sprint) expandPaths() error {
	for name, repo := range s.Repos {
		p, err := expandHome(repo.Path)
		if err != nil {
			return fmt.Errorf("repo %s: %w", name, err)
		}
		repo.Path = p
		s.Repos[name] = repo
	}
	for i, acct := range s.Accounts {
		if acct.ConfigDir == "" {
			continue
		}
		p, err := expandHome(acct.ConfigDir)
		if err != nil {
			return fmt.Errorf("account %s: %w", acct.Name, err)
		}
		s.Accounts[i].ConfigDir = p
	}
	return nil
}

func expandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expanding %q: %w", p, err)
	}
	return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/")), nil
}

// Validate reports the first structural problem with the sprint file.
//
// The predicate rule is the load-bearing one: a lane with no predicate cannot
// be verified by anything other than the session that claims to have done the
// work, which is the exact defect sprintd exists to prevent. It is rejected at
// parse time, by lane id, before a single lane is dispatched.
func (s *Sprint) Validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("%w: sprint name is empty", ErrInvalid)
	}
	if !refSafe.MatchString(s.Name) {
		return fmt.Errorf("%w: sprint name %q is not usable in a branch name; "+
			"use letters, digits, dot, dash or underscore", ErrInvalid, s.Name)
	}
	if !s.Opened.IsZero() && !s.Closes.IsZero() && !s.Closes.After(s.Opened) {
		return fmt.Errorf("%w: closes (%s) is not after opened (%s)",
			ErrInvalid, s.Closes.Format(time.RFC3339), s.Opened.Format(time.RFC3339))
	}
	if err := s.validateMachines(); err != nil {
		return err
	}
	if err := s.validateRepos(); err != nil {
		return err
	}
	if err := s.validateAccounts(); err != nil {
		return err
	}
	return s.validateLanes()
}

func (s *Sprint) validateMachines() error {
	if len(s.Machines) == 0 {
		return fmt.Errorf("%w: no machines declared", ErrInvalid)
	}
	for _, name := range sortedKeys(s.Machines) {
		if strings.TrimSpace(s.Machines[name].Host) == "" {
			return fmt.Errorf("%w: machine %s has an empty host", ErrInvalid, name)
		}
	}
	return nil
}

func (s *Sprint) validateRepos() error {
	if len(s.Repos) == 0 {
		return fmt.Errorf("%w: no repos declared", ErrInvalid)
	}
	for _, name := range sortedKeys(s.Repos) {
		repo := s.Repos[name]
		if strings.TrimSpace(repo.Path) == "" {
			return fmt.Errorf("%w: repo %s has an empty path", ErrInvalid, name)
		}
		if repo.MaxConcurrent < 1 {
			return fmt.Errorf("%w: repo %s has max_concurrent %d, want at least 1",
				ErrInvalid, name, repo.MaxConcurrent)
		}
		if strings.TrimSpace(repo.Base) != repo.Base {
			return fmt.Errorf("%w: repo %s has a base with surrounding whitespace: %q",
				ErrInvalid, name, repo.Base)
		}
		if repo.StaleThreshold != nil && *repo.StaleThreshold < 0 {
			return fmt.Errorf("%w: repo %s has stale_threshold %d, want zero or more",
				ErrInvalid, name, *repo.StaleThreshold)
		}
	}
	return nil
}

func (s *Sprint) validateAccounts() error {
	if len(s.Accounts) == 0 {
		return fmt.Errorf("%w: no accounts declared", ErrInvalid)
	}
	seenName := map[string]bool{}
	seenDir := map[string]string{}
	for _, acct := range s.Accounts {
		if strings.TrimSpace(acct.Name) == "" {
			return fmt.Errorf("%w: an account has an empty name", ErrInvalid)
		}
		if seenName[acct.Name] {
			return fmt.Errorf("%w: duplicate account name %s", ErrInvalid, acct.Name)
		}
		seenName[acct.Name] = true
		if acct.ReserveFloorPct < 0 || acct.ReserveFloorPct > 100 {
			return fmt.Errorf("%w: account %s has reserve_floor_pct %.1f, want 0-100",
				ErrInvalid, acct.Name, acct.ReserveFloorPct)
		}
		if acct.WeeklyTokenLimit < 0 {
			return fmt.Errorf("%w: account %s has a negative weekly_token_limit", ErrInvalid, acct.Name)
		}
		if prev, ok := seenDir[acct.ConfigDir]; ok {
			return fmt.Errorf("%w: accounts %s and %s share config_dir %q, so their credentials cannot be told apart",
				ErrInvalid, prev, acct.Name, acct.ConfigDir)
		}
		seenDir[acct.ConfigDir] = acct.Name
	}
	return nil
}

func (s *Sprint) validateLanes() error {
	if len(s.Lanes) == 0 {
		return fmt.Errorf("%w: no lanes declared", ErrInvalid)
	}
	ids := make(map[string]bool, len(s.Lanes))
	for _, ln := range s.Lanes {
		if strings.TrimSpace(ln.ID) == "" {
			return fmt.Errorf("%w: a lane has an empty id", ErrInvalid)
		}
		if ids[ln.ID] {
			return fmt.Errorf("%w: duplicate lane id %s", ErrInvalid, ln.ID)
		}
		if !refSafe.MatchString(ln.ID) {
			return fmt.Errorf("%w: lane id %q is not usable in a branch name; "+
				"use letters, digits, dot, dash or underscore", ErrInvalid, ln.ID)
		}
		ids[ln.ID] = true
	}
	for _, ln := range s.Lanes {
		if strings.TrimSpace(ln.Predicate) == "" {
			return fmt.Errorf("%w: lane %s has no predicate; a lane whose completion "+
				"cannot be observed by a separate process is not a lane", ErrInvalid, ln.ID)
		}
		hasPrompt := strings.TrimSpace(ln.Prompt) != ""
		hasCommand := strings.TrimSpace(ln.Command) != ""
		switch {
		case hasPrompt && hasCommand:
			return fmt.Errorf("%w: lane %s sets both prompt and command; it can have one or the other, "+
				"since command replaces the claude invocation sprintd would compose", ErrInvalid, ln.ID)
		case !hasPrompt && !hasCommand:
			return fmt.Errorf("%w: lane %s has neither a prompt nor a command, so there is nothing to run",
				ErrInvalid, ln.ID)
		}
		if strings.TrimSpace(ln.Goal) == "" {
			return fmt.Errorf("%w: lane %s has an empty goal", ErrInvalid, ln.ID)
		}
		if _, ok := s.Repos[ln.Repo]; !ok {
			return fmt.Errorf("%w: lane %s references unknown repo %q", ErrInvalid, ln.ID, ln.Repo)
		}
		if _, ok := s.Machines[ln.Machine]; !ok {
			return fmt.Errorf("%w: lane %s references unknown machine %q", ErrInvalid, ln.ID, ln.Machine)
		}
		// A model is only meaningful for a lane whose command sprintd composes.
		// A command lane picks its own model, or has no concept of one.
		if ln.Composed() {
			if model := s.modelFor(ln); model == "" {
				return fmt.Errorf("%w: lane %s has no model and defaults.model is unset", ErrInvalid, ln.ID)
			}
		}
		if d := s.deadlineFor(ln); d <= 0 {
			return fmt.Errorf("%w: lane %s has deadline %s, want a positive duration", ErrInvalid, ln.ID, d)
		}
		if r := s.retriesFor(ln); r < 0 {
			return fmt.Errorf("%w: lane %s has negative retries %d", ErrInvalid, ln.ID, r)
		}
		for _, need := range ln.Needs {
			if need == ln.ID {
				return fmt.Errorf("%w: lane %s depends on itself", ErrInvalid, ln.ID)
			}
			if !ids[need] {
				return fmt.Errorf("%w: lane %s needs unknown lane %q", ErrInvalid, ln.ID, need)
			}
		}
	}
	return s.validateNoCycles()
}

// validateNoCycles rejects a needs graph that can never drain.
func (s *Sprint) validateNoCycles() error {
	const (
		unvisited = 0
		onStack   = 1
		done      = 2
	)
	state := make(map[string]int, len(s.Lanes))
	byID := make(map[string]Lane, len(s.Lanes))
	for _, ln := range s.Lanes {
		byID[ln.ID] = ln
	}
	var walk func(id string, path []string) error
	walk = func(id string, path []string) error {
		switch state[id] {
		case done:
			return nil
		case onStack:
			return fmt.Errorf("%w: lanes form a needs cycle: %s", ErrInvalid,
				strings.Join(append(path, id), " -> "))
		}
		state[id] = onStack
		for _, need := range byID[id].Needs {
			if err := walk(need, append(path, id)); err != nil {
				return err
			}
		}
		state[id] = done
		return nil
	}
	for _, ln := range s.Lanes {
		if err := walk(ln.ID, nil); err != nil {
			return err
		}
	}
	return nil
}

func (s *Sprint) modelFor(ln Lane) string {
	if ln.Model != "" {
		return ln.Model
	}
	return s.Defaults.Model
}

func (s *Sprint) deadlineFor(ln Lane) time.Duration {
	if ln.Deadline != 0 {
		return ln.Deadline.Std()
	}
	return s.Defaults.Deadline.Std()
}

func (s *Sprint) retriesFor(ln Lane) int {
	if ln.Retries != nil {
		return *ln.Retries
	}
	return s.Defaults.Retries
}

// ResolvedLanes returns every lane with defaults applied, in file order.
func (s *Sprint) ResolvedLanes() []ResolvedLane {
	out := make([]ResolvedLane, 0, len(s.Lanes))
	for _, ln := range s.Lanes {
		repo := s.Repos[ln.Repo]
		resolved := ResolvedLane{
			Lane:          ln,
			RepoPath:      repo.Path,
			MaxConcurrent: repo.MaxConcurrent,
			Host:          s.Machines[ln.Machine].Host,
			Base:          repo.BaseRef(),
			Attempts:      s.retriesFor(ln) + 1,
			Timeout:       s.deadlineFor(ln),
		}
		if ln.Composed() {
			resolved.Model = s.modelFor(ln)
		}
		out = append(out, resolved)
	}
	return out
}

// RepoTarget is a repo as preflight needs to see it on one machine.
type RepoTarget struct {
	Name           string
	Path           string
	Base           string
	StaleThreshold int
}

// UsesClaude reports whether any lane has its claude invocation composed by
// sprintd. When none do, sprintd never runs claude itself, and preflight has
// no reason to hold up the sprint on a claude probe.
func (s *Sprint) UsesClaude() bool {
	for _, ln := range s.Lanes {
		if ln.Composed() {
			return true
		}
	}
	return false
}

// MachineRepos maps each machine name to the repos lanes use on it.
func (s *Sprint) MachineRepos() map[string][]RepoTarget {
	out := map[string][]RepoTarget{}
	for _, ln := range s.Lanes {
		if hasRepo(out[ln.Machine], ln.Repo) {
			continue
		}
		repo := s.Repos[ln.Repo]
		out[ln.Machine] = append(out[ln.Machine], RepoTarget{
			Name:           ln.Repo,
			Path:           repo.Path,
			Base:           repo.BaseRef(),
			StaleThreshold: repo.StaleLimit(),
		})
	}
	for name := range out {
		sort.Slice(out[name], func(i, j int) bool { return out[name][i].Name < out[name][j].Name })
	}
	return out
}

func hasRepo(targets []RepoTarget, name string) bool {
	for _, t := range targets {
		if t.Name == name {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
