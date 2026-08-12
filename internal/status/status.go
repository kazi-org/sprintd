// Package status assembles the machine-readable view of a run.
//
// It exists so a consumer never has to parse results.jsonl. That file is an
// append-only event log whose record shapes belong to sprintd; a dashboard
// reading it would break silently the first time a field moved. This package
// is the interface instead, and it carries a schema version so a change to it
// is a deliberate break rather than an accident.
package status

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/kazi-org/sprintd/internal/results"
)

// SchemaVersion is the version of the JSON this package emits. Consumers
// should refuse a version they do not understand. Bump it for any change that
// removes or repurposes a field; adding a field is not a break.
const SchemaVersion = 1

// ManifestName is the run-directory-relative name of the run manifest.
const ManifestName = "run.json"

// RunStatus is the top-level answer to "is anything happening right now".
type RunStatus string

const (
	// StatusNoRun means no run was found at all. It is a legitimate, healthy
	// answer, not an error: a dashboard's honest empty state depends on it.
	StatusNoRun RunStatus = "no_run"
	// StatusRunning means a sprintd process is alive and still working.
	StatusRunning RunStatus = "running"
	// StatusFinished means the run reached its own end and recorded it.
	StatusFinished RunStatus = "finished"
	// StatusInterrupted means the run never recorded an end and its process is
	// gone -- killed, crashed, or the machine restarted. Lanes may be left
	// unresolved, and nothing is running now.
	StatusInterrupted RunStatus = "interrupted"
	// StatusUnknown means liveness could not be determined, which happens for
	// a run directory written before manifests existed, or one being read on a
	// different machine from the one that produced it.
	StatusUnknown RunStatus = "unknown"
)

// Machine is a dispatch target as declared by the sprint.
type Machine struct {
	Name string `json:"name"`
	Host string `json:"host"`
}

// LaneSpec is what the sprint file declared about a lane, which the event log
// does not carry on its own.
type LaneSpec struct {
	ID      string `json:"id"`
	Repo    string `json:"repo"`
	Machine string `json:"machine"`
	Goal    string `json:"goal,omitempty"`
	Model   string `json:"model,omitempty"`
	Command string `json:"command,omitempty"`
}

// AccountSample is one account's usage as read at run start.
//
// SampledAt is not decoration. sprintd reads usage once, when the run begins,
// and never again; without the timestamp a dashboard would render an hours-old
// figure as though it were live.
type AccountSample struct {
	Name            string     `json:"name"`
	ReserveFloorPct float64    `json:"reserve_floor_pct"`
	WeeklyTokens    int64      `json:"weekly_tokens,omitempty"`
	WeeklyLimit     int64      `json:"weekly_limit,omitempty"`
	WeeklyPct       *float64   `json:"weekly_pct,omitempty"`
	Metered         bool       `json:"metered"`
	SampledAt       *time.Time `json:"sampled_at,omitempty"`
	SampledOnce     bool       `json:"sampled_once"`
}

// Manifest is written once when a run starts and finalised when it ends.
//
// The event log alone cannot answer several questions a consumer needs: when
// the run began, what the sprint's window was, which machines and accounts
// were in play, and -- most importantly -- whether anything is running now.
// The manifest is where those live.
type Manifest struct {
	SchemaVersion int             `json:"schema_version"`
	Sprint        string          `json:"sprint"`
	Opened        *time.Time      `json:"opened,omitempty"`
	Closes        *time.Time      `json:"closes,omitempty"`
	StartedAt     time.Time       `json:"started_at"`
	CompletedAt   *time.Time      `json:"completed_at,omitempty"`
	PID           int             `json:"pid"`
	Hostname      string          `json:"hostname,omitempty"`
	Machines      []Machine       `json:"machines,omitempty"`
	Lanes         []LaneSpec      `json:"lanes,omitempty"`
	Accounts      []AccountSample `json:"accounts,omitempty"`
}

// Skeleton is the manifest a run writes the moment its directory exists,
// before anything slow happens.
//
// Reading per-account usage shells out to ccusage and can take the better part
// of a minute. Without this, a run would have no manifest for that whole
// window and anything watching would report "unknown" at the start of every
// sprint -- exactly when someone is most likely to be looking.
func Skeleton(sprintName string, opened, closes *time.Time, startedAt time.Time,
	machines []Machine, lanes []LaneSpec) Manifest {
	m := Manifest{
		Sprint:    sprintName,
		Opened:    opened,
		Closes:    closes,
		StartedAt: startedAt.UTC(),
		PID:       os.Getpid(),
		Machines:  machines,
		Lanes:     lanes,
	}
	if host, err := os.Hostname(); err == nil {
		m.Hostname = host
	}
	return m
}

// WriteManifest saves the manifest into the run directory.
func WriteManifest(runDir string, m Manifest) error {
	m.SchemaVersion = SchemaVersion
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding run manifest: %w", err)
	}
	path := filepath.Join(runDir, ManifestName)
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		return fmt.Errorf("writing run manifest %s: %w", path, err)
	}
	return nil
}

// ReadManifest loads a run's manifest. A missing manifest is not an error; the
// second return value reports whether one was found.
func ReadManifest(runDir string) (Manifest, bool, error) {
	body, err := os.ReadFile(filepath.Join(runDir, ManifestName))
	if errors.Is(err, fs.ErrNotExist) {
		return Manifest{}, false, nil
	}
	if err != nil {
		return Manifest{}, false, fmt.Errorf("reading run manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return Manifest{}, false, fmt.Errorf("parsing run manifest: %w", err)
	}
	return m, true, nil
}

// SprintInfo identifies the run.
type SprintInfo struct {
	Name           string     `json:"name"`
	Opened         *time.Time `json:"opened,omitempty"`
	Closes         *time.Time `json:"closes,omitempty"`
	RunDir         string     `json:"run_dir"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
	ElapsedSeconds *float64   `json:"elapsed_seconds,omitempty"`
}

// Lane is one lane's current state.
type Lane struct {
	ID              string     `json:"id"`
	Repo            string     `json:"repo,omitempty"`
	Machine         string     `json:"machine,omitempty"`
	Account         string     `json:"account,omitempty"`
	Model           string     `json:"model,omitempty"`
	Command         string     `json:"command,omitempty"`
	Goal            string     `json:"goal,omitempty"`
	State           string     `json:"state"`
	Terminal        bool       `json:"terminal"`
	Attempt         int        `json:"attempt"`
	DispatchedAt    *time.Time `json:"dispatched_at,omitempty"`
	ResolvedAt      *time.Time `json:"resolved_at,omitempty"`
	DurationSeconds *float64   `json:"duration_seconds,omitempty"`
	Reason          string     `json:"reason,omitempty"`
	PredicateOutput string     `json:"predicate_output,omitempty"`
	Worktree        string     `json:"worktree,omitempty"`
	Branch          string     `json:"branch,omitempty"`
}

// Totals are the aggregates a consumer should not have to compute.
type Totals struct {
	Lanes      int            `json:"lanes"`
	Dispatched int            `json:"dispatched"`
	Converged  int            `json:"converged"`
	Escalated  int            `json:"escalated"`
	Unresolved int            `json:"unresolved"`
	ByState    map[string]int `json:"by_state"`
}

// MachineStatus reports whether a machine is doing anything.
//
// There is deliberately no utilisation percentage. sprintd knows which lanes
// it dispatched where; it does not measure load, and a number it cannot
// observe would be worse on a dashboard than no number at all.
type MachineStatus struct {
	Name string `json:"name"`
	Host string `json:"host"`
	// Busy is true only when the run is live and a lane is in flight here.
	Busy bool `json:"busy"`
	// RunningLanes is populated only while the run is live.
	RunningLanes []string `json:"running_lanes"`
	// UnresolvedLanes are lanes that never reached a terminal state, whatever
	// the run's status. On an interrupted run these are what was abandoned.
	UnresolvedLanes []string `json:"unresolved_lanes"`
}

// Report is the whole payload.
type Report struct {
	SchemaVersion int             `json:"schema_version"`
	Status        RunStatus       `json:"status"`
	GeneratedAt   time.Time       `json:"generated_at"`
	Sprint        *SprintInfo     `json:"sprint,omitempty"`
	Totals        Totals          `json:"totals"`
	Lanes         []Lane          `json:"lanes"`
	Machines      []MachineStatus `json:"machines"`
	Accounts      []AccountSample `json:"accounts"`
	// Notes carry caveats that apply to this payload, so a consumer can render
	// them rather than infer confidence sprintd does not have.
	Notes []string `json:"notes,omitempty"`
}

// Build assembles the report for a run directory.
//
// Every part is optional: a directory with a manifest and no events, or events
// and no manifest, both produce a usable report. Only a directory with neither
// is "no run".
func Build(runDir string, now time.Time) (Report, error) {
	report := Report{
		SchemaVersion: SchemaVersion,
		Status:        StatusNoRun,
		GeneratedAt:   now.UTC(),
		Lanes:         []Lane{},
		Machines:      []MachineStatus{},
		Accounts:      []AccountSample{},
		Totals:        Totals{ByState: map[string]int{}},
	}
	if runDir == "" {
		return report, nil
	}

	manifest, hasManifest, err := ReadManifest(runDir)
	if err != nil {
		return report, err
	}

	recs, hasEvents, eventNotes, err := readEvents(runDir)
	if err != nil {
		return report, err
	}
	if !hasManifest && !hasEvents {
		return report, nil
	}

	report.Status, report.Notes = runStatus(manifest, hasManifest, recs, now)
	report.Notes = append(report.Notes, eventNotes...)
	report.Sprint = sprintInfo(manifest, hasManifest, recs, runDir, now)
	report.Lanes = buildLanes(manifest, recs)
	report.Totals = totals(report.Lanes)
	report.Machines = machineStatuses(manifest, report.Lanes, report.Status)
	report.Accounts = manifest.Accounts
	if len(report.Accounts) > 0 {
		report.Notes = append(report.Notes,
			"account usage is sampled once when the run starts and is not refreshed; see sampled_at")
	}
	return report, nil
}

// readEvents loads the event log, tolerating a truncated final line -- what
// a process killed mid-write leaves behind. Every earlier record was already
// flushed by its own Write call, so at most one trailing line can be
// incomplete; that one is dropped rather than failing the whole read.
func readEvents(runDir string) ([]results.Record, bool, []string, error) {
	f, err := os.Open(filepath.Join(runDir, results.FileName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil, nil
	}
	if err != nil {
		return nil, false, nil, fmt.Errorf("opening event log: %w", err)
	}
	defer f.Close()

	recs, truncated, err := results.ReadTolerant(f)
	if err != nil {
		return nil, false, nil, err
	}
	var notes []string
	if truncated {
		notes = append(notes, "the event log's last line was incomplete (the run was killed mid-write) "+
			"and was dropped; every earlier event is intact")
	}
	return recs, true, notes, nil
}

// runStatus answers the question the whole payload exists for.
//
// It is decided by recorded facts, never inferred from timestamps: a run that
// recorded its own completion is finished; otherwise the run's process either
// still exists or it does not.
func runStatus(m Manifest, hasManifest bool, recs []results.Record, now time.Time) (RunStatus, []string) {
	var notes []string
	if !hasManifest {
		if allTerminal(recs) {
			return StatusFinished, append(notes,
				"no run manifest: this run predates manifests or the file was removed, "+
					"so 'finished' is inferred from every lane having reached a terminal state")
		}
		return StatusUnknown, append(notes,
			"no run manifest, and lanes are unresolved: sprintd cannot tell whether this run is still going")
	}
	if m.CompletedAt != nil {
		return StatusFinished, notes
	}
	host, err := os.Hostname()
	if err == nil && m.Hostname != "" && host != m.Hostname {
		return StatusUnknown, append(notes, fmt.Sprintf(
			"this run was started on %s and is being read on %s, so its process cannot be checked",
			m.Hostname, host))
	}
	if m.PID <= 0 {
		return StatusUnknown, append(notes, "the run recorded no process id, so liveness cannot be checked")
	}
	if processAlive(m.PID) {
		return StatusRunning, append(notes,
			"liveness is a check on the recorded process id; a reused pid could in principle read as running")
	}
	return StatusInterrupted, append(notes,
		"the run never recorded completion and its process is gone: it was killed or it crashed, "+
			"and any unresolved lanes were abandoned")
}

func allTerminal(recs []results.Record) bool {
	statuses := results.Summarise(recs)
	if len(statuses) == 0 {
		return false
	}
	for _, s := range statuses {
		if !s.State.Terminal() {
			return false
		}
	}
	return true
}

func sprintInfo(m Manifest, hasManifest bool, recs []results.Record, runDir string, now time.Time) *SprintInfo {
	info := &SprintInfo{RunDir: runDir}
	if hasManifest {
		info.Name = m.Sprint
		info.Opened = m.Opened
		info.Closes = m.Closes
		started := m.StartedAt
		info.StartedAt = &started
		info.CompletedAt = m.CompletedAt
	}
	if info.Name == "" && len(recs) > 0 {
		info.Name = recs[0].Sprint
	}
	if info.StartedAt == nil && len(recs) > 0 {
		first := recs[0].Time
		info.StartedAt = &first
	}
	if info.StartedAt != nil {
		end := now
		if info.CompletedAt != nil {
			end = *info.CompletedAt
		} else if len(recs) > 0 && info.CompletedAt == nil {
			// For a run with no recorded completion, the last event is the
			// last thing we actually know happened.
			if last := recs[len(recs)-1].Time; last.After(*info.StartedAt) && last.After(now) {
				end = last
			}
		}
		elapsed := end.Sub(*info.StartedAt).Seconds()
		if elapsed < 0 {
			elapsed = 0
		}
		info.ElapsedSeconds = &elapsed
	}
	return info
}

// buildLanes merges what the sprint declared with what the run recorded, so a
// lane that was never dispatched still appears rather than silently vanishing.
func buildLanes(m Manifest, recs []results.Record) []Lane {
	byLane := map[string][]results.Record{}
	var order []string
	for _, rec := range recs {
		if _, seen := byLane[rec.Lane]; !seen {
			order = append(order, rec.Lane)
		}
		byLane[rec.Lane] = append(byLane[rec.Lane], rec)
	}

	specs := map[string]LaneSpec{}
	var declared []string
	for _, spec := range m.Lanes {
		specs[spec.ID] = spec
		declared = append(declared, spec.ID)
	}
	// Declared order first, then anything the log knows about that the
	// manifest does not.
	ids := append([]string{}, declared...)
	for _, id := range order {
		if _, ok := specs[id]; !ok {
			ids = append(ids, id)
		}
	}

	lanes := make([]Lane, 0, len(ids))
	for _, id := range ids {
		spec := specs[id]
		lane := Lane{
			ID:      id,
			Repo:    spec.Repo,
			Machine: spec.Machine,
			Goal:    spec.Goal,
			Model:   spec.Model,
			Command: spec.Command,
			State:   "pending",
		}
		history := byLane[id]
		if len(history) == 0 {
			lanes = append(lanes, lane)
			continue
		}
		last := history[len(history)-1]
		lane.State = string(last.State)
		lane.Terminal = last.State.Terminal()
		lane.Attempt = last.Attempt
		lane.Reason = last.Reason
		lane.PredicateOutput = last.PredicateOutput
		lane.Worktree = last.Worktree
		lane.Branch = last.Branch
		if last.Account != "" {
			lane.Account = last.Account
		}
		if lane.Repo == "" {
			lane.Repo = last.Repo
		}
		if lane.Machine == "" {
			lane.Machine = last.Machine
		}
		if lane.Model == "" {
			lane.Model = last.Model
		}
		for _, rec := range history {
			if rec.State == results.StateDispatched {
				at := rec.Time
				lane.DispatchedAt = &at
				break
			}
		}
		if lane.Terminal {
			at := last.Time
			lane.ResolvedAt = &at
		}
		if lane.DispatchedAt != nil {
			end := last.Time
			d := end.Sub(*lane.DispatchedAt).Seconds()
			if d >= 0 {
				lane.DurationSeconds = &d
			}
		}
		lanes = append(lanes, lane)
	}
	return lanes
}

func totals(lanes []Lane) Totals {
	t := Totals{Lanes: len(lanes), ByState: map[string]int{}}
	for _, lane := range lanes {
		t.ByState[lane.State]++
		if lane.DispatchedAt != nil {
			t.Dispatched++
		}
		switch lane.State {
		case string(results.StateComplete):
			t.Converged++
		case string(results.StateEscalated):
			t.Escalated++
		}
		if !lane.Terminal {
			t.Unresolved++
		}
	}
	return t
}

func machineStatuses(m Manifest, lanes []Lane, status RunStatus) []MachineStatus {
	byName := map[string]*MachineStatus{}
	var order []string
	add := func(name, host string) *MachineStatus {
		if existing, ok := byName[name]; ok {
			return existing
		}
		byName[name] = &MachineStatus{
			Name: name, Host: host,
			RunningLanes: []string{}, UnresolvedLanes: []string{},
		}
		order = append(order, name)
		return byName[name]
	}
	for _, machine := range m.Machines {
		add(machine.Name, machine.Host)
	}
	for _, lane := range lanes {
		if lane.Machine == "" {
			continue
		}
		ms := add(lane.Machine, "")
		if lane.Terminal || lane.DispatchedAt == nil {
			continue
		}
		ms.UnresolvedLanes = append(ms.UnresolvedLanes, lane.ID)
		// A lane only counts as running when the run itself is live. On an
		// interrupted run nothing is running, however the log ended.
		if status == StatusRunning {
			ms.RunningLanes = append(ms.RunningLanes, lane.ID)
			ms.Busy = true
		}
	}
	sort.Strings(order)
	out := make([]MachineStatus, 0, len(order))
	for _, name := range order {
		out = append(out, *byName[name])
	}
	return out
}

// FindLatestRun returns the most recently written run directory under root.
// An absent root is not an error: it means no sprint has run here.
func FindLatestRun(root string) (string, bool, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading run root %s: %w", root, err)
	}
	var newest string
	var newestAt time.Time
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		at, ok := runTimestamp(dir)
		if !ok {
			continue
		}
		if newest == "" || at.After(newestAt) {
			newest, newestAt = dir, at
		}
	}
	return newest, newest != "", nil
}

// runTimestamp is when a run directory was last written to, taken from the
// event log where there is one and the manifest otherwise.
func runTimestamp(dir string) (time.Time, bool) {
	var newest time.Time
	var found bool
	for _, name := range []string{results.FileName, ManifestName} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		found = true
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	return newest, found
}
