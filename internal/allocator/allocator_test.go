package allocator_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/kazi-org/sprintd/internal/allocator"
	"github.com/kazi-org/sprintd/internal/sprint"
)

// fakeReader serves canned usage, or an error for accounts listed in fail.
type fakeReader struct {
	usage map[string]allocator.Usage
	fail  map[string]error
}

func (f fakeReader) Read(_ context.Context, acct sprint.Account) (allocator.Usage, error) {
	if err, ok := f.fail[acct.Name]; ok {
		return allocator.Usage{}, err
	}
	usage, ok := f.usage[acct.Name]
	if !ok {
		return allocator.Usage{}, errors.New("no usage recorded")
	}
	return usage, nil
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// acct is a metered account with a 1000-token weekly limit, so a usage figure
// reads directly as a percentage times ten.
func acct(name string, floorPct float64) sprint.Account {
	return sprint.Account{Name: name, ReserveFloorPct: floorPct, WeeklyTokenLimit: 1000, ConfigDir: "/tmp/" + name}
}

func TestAssign(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		accounts []sprint.Account
		usage    map[string]allocator.Usage
		fail     map[string]error
		assigns  int
		want     []string
		wantErr  error
	}{
		{
			name:     "least consumed account wins",
			accounts: []sprint.Account{acct("primary", 0), acct("secondary", 0)},
			usage: map[string]allocator.Usage{
				"primary":   {WeeklyTokens: 500},
				"secondary": {WeeklyTokens: 100},
			},
			assigns: 1,
			want:    []string{"secondary"},
		},
		{
			name:     "reserve floor keeps the coordinator's account in hand",
			accounts: []sprint.Account{acct("primary", 30), acct("secondary", 0)},
			usage: map[string]allocator.Usage{
				// primary has 25% remaining, below its 30% floor.
				"primary":   {WeeklyTokens: 750},
				"secondary": {WeeklyTokens: 800},
			},
			assigns: 1,
			want:    []string{"secondary"},
		},
		{
			name:     "floor is exclusive at exactly the floor",
			accounts: []sprint.Account{acct("primary", 30), acct("secondary", 0)},
			usage: map[string]allocator.Usage{
				// primary has exactly 30% remaining, which is not above 30.
				"primary":   {WeeklyTokens: 700},
				"secondary": {WeeklyTokens: 840},
			},
			assigns: 1,
			want:    []string{"secondary"},
		},
		{
			name:     "hard stop applies even with a zero floor",
			accounts: []sprint.Account{acct("primary", 0), acct("secondary", 0)},
			usage: map[string]allocator.Usage{
				"primary":   {WeeklyTokens: 860}, // 86%, past the 85% hard stop
				"secondary": {WeeklyTokens: 840}, // 84%, still eligible
			},
			assigns: 1,
			want:    []string{"secondary"},
		},
		{
			name:     "hard stop is inclusive at exactly 85 percent",
			accounts: []sprint.Account{acct("primary", 0), acct("secondary", 0)},
			usage: map[string]allocator.Usage{
				"primary":   {WeeklyTokens: 850},
				"secondary": {WeeklyTokens: 840},
			},
			assigns: 1,
			want:    []string{"secondary"},
		},
		{
			name:     "every account exhausted is an error, not a silent overrun",
			accounts: []sprint.Account{acct("primary", 30), acct("secondary", 0)},
			usage: map[string]allocator.Usage{
				"primary":   {WeeklyTokens: 900},
				"secondary": {WeeklyTokens: 990},
			},
			assigns: 1,
			wantErr: allocator.ErrNoAccount,
		},
		{
			name:     "ties spread across accounts rather than piling on one",
			accounts: []sprint.Account{acct("primary", 0), acct("secondary", 0), acct("tertiary", 0)},
			usage: map[string]allocator.Usage{
				"primary":   {WeeklyTokens: 100},
				"secondary": {WeeklyTokens: 100},
				"tertiary":  {WeeklyTokens: 100},
			},
			assigns: 4,
			want:    []string{"primary", "secondary", "tertiary", "primary"},
		},
		{
			name:     "unreadable usage degrades to round-robin",
			accounts: []sprint.Account{acct("primary", 30), acct("secondary", 0)},
			fail: map[string]error{
				"primary":   errors.New("ccusage not found"),
				"secondary": errors.New("ccusage not found"),
			},
			assigns: 3,
			want:    []string{"primary", "secondary", "primary"},
		},
		{
			name:     "a metered account is preferred over an unreadable one",
			accounts: []sprint.Account{acct("primary", 0), acct("secondary", 0)},
			usage:    map[string]allocator.Usage{"secondary": {WeeklyTokens: 400}},
			fail:     map[string]error{"primary": errors.New("ccusage exploded")},
			assigns:  2,
			want:     []string{"secondary", "secondary"},
		},
		{
			name: "an account with no declared limit cannot be metered",
			accounts: []sprint.Account{
				{Name: "primary", ReserveFloorPct: 30, ConfigDir: "/tmp/primary"},
				acct("secondary", 0),
			},
			usage:   map[string]allocator.Usage{"secondary": {WeeklyTokens: 400}},
			assigns: 2,
			want:    []string{"secondary", "secondary"},
		},
		{
			name:     "an exhausted metered account falls back to an unmetered one",
			accounts: []sprint.Account{acct("primary", 0), {Name: "spare", ConfigDir: "/tmp/spare"}},
			usage:    map[string]allocator.Usage{"primary": {WeeklyTokens: 900}},
			assigns:  1,
			want:     []string{"spare"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reader := fakeReader{usage: tc.usage, fail: tc.fail}
			a := allocator.New(context.Background(), tc.accounts, reader, quietLogger())

			var got []string
			for i := 0; i < tc.assigns; i++ {
				name, err := a.Assign()
				if tc.wantErr != nil {
					if !errors.Is(err, tc.wantErr) {
						t.Fatalf("Assign() error = %v, want %v", err, tc.wantErr)
					}
					return
				}
				if err != nil {
					t.Fatalf("Assign() error = %v, want nil", err)
				}
				got = append(got, name)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("Assign() gave %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("assignment %d = %q, want %q (full sequence %v)", i+1, got[i], tc.want[i], got)
				}
			}
		})
	}
}

func TestWeeklyPct(t *testing.T) {
	t.Parallel()

	a := allocator.New(context.Background(),
		[]sprint.Account{acct("primary", 0), {Name: "spare", ConfigDir: "/tmp/spare"}},
		fakeReader{usage: map[string]allocator.Usage{"primary": {WeeklyTokens: 425}}},
		quietLogger())

	pct, known := a.WeeklyPct("primary")
	if !known {
		t.Fatal("WeeklyPct(primary) known = false, want true")
	}
	if pct != 42.5 {
		t.Errorf("WeeklyPct(primary) = %v, want 42.5", pct)
	}
	if _, known := a.WeeklyPct("spare"); known {
		t.Error("WeeklyPct(spare) known = true, want false for an account with no limit")
	}
}

func TestAccountLookup(t *testing.T) {
	t.Parallel()

	a := allocator.New(context.Background(), []sprint.Account{acct("primary", 30)},
		fakeReader{usage: map[string]allocator.Usage{"primary": {WeeklyTokens: 1}}}, quietLogger())

	got, ok := a.Account("primary")
	if !ok {
		t.Fatal("Account(primary) ok = false, want true")
	}
	if got.ConfigDir != "/tmp/primary" {
		t.Errorf("Account(primary).ConfigDir = %q, want /tmp/primary", got.ConfigDir)
	}
	if _, ok := a.Account("missing"); ok {
		t.Error("Account(missing) ok = true, want false")
	}
}

func TestAssignIsConcurrencySafe(t *testing.T) {
	t.Parallel()

	a := allocator.New(context.Background(),
		[]sprint.Account{acct("primary", 0), acct("secondary", 0)},
		fakeReader{usage: map[string]allocator.Usage{
			"primary":   {WeeklyTokens: 100},
			"secondary": {WeeklyTokens: 100},
		}}, quietLogger())

	const workers = 32
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		go func() {
			_, err := a.Assign()
			errs <- err
		}()
	}
	for i := 0; i < workers; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent Assign() error = %v, want nil", err)
		}
	}
}
