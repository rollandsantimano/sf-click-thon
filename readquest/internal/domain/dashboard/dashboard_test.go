package dashboard

import (
	"testing"
)

// assess and sortByRisk are pure, which is deliberate: the risk rules turn on
// how long ago a student read, and no integration test can reach "nine days of
// silence" without either waiting nine days or seeding fake history. These
// tests need neither a database nor time travel.

func days(n int) *int { return &n }

func TestAssess(t *testing.T) {
	tests := []struct {
		name       string
		standing   StudentStanding
		wantAtRisk bool
		wantReason string
	}{
		{
			name:       "never read outranks every other concern",
			standing:   StudentStanding{DaysSinceRead: nil},
			wantAtRisk: true,
			wantReason: "has never logged a reading session",
		},
		{
			name:       "silent for exactly the threshold is at risk",
			standing:   StudentStanding{DaysSinceRead: days(staleAfterDays), VelocityPerDay: 50},
			wantAtRisk: true,
			wantReason: "last read 7 days ago",
		},
		{
			name:       "silent beyond the threshold is at risk",
			standing:   StudentStanding{DaysSinceRead: days(30), VelocityPerDay: 99},
			wantAtRisk: true,
			wantReason: "last read 30 days ago",
		},
		{
			// Guards the off-by-one: one day inside the window must be safe,
			// otherwise every student is permanently flagged.
			name:       "one day inside the threshold is safe if pace is good",
			standing:   StudentStanding{DaysSinceRead: days(staleAfterDays - 1), VelocityPerDay: 20},
			wantAtRisk: false,
		},
		{
			name:       "reading recently but too slowly is at risk",
			standing:   StudentStanding{DaysSinceRead: days(1), VelocityPerDay: 4},
			wantAtRisk: true,
			wantReason: "averaging 4.0 pages/day over the last 7 days",
		},
		{
			name:       "exactly at the pace threshold is not at risk",
			standing:   StudentStanding{DaysSinceRead: days(0), VelocityPerDay: minPagesPerDay},
			wantAtRisk: false,
		},
		{
			name:       "just below the pace threshold is at risk",
			standing:   StudentStanding{DaysSinceRead: days(0), VelocityPerDay: minPagesPerDay - 0.1},
			wantAtRisk: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotAtRisk, gotReason := assess(tc.standing)
			if gotAtRisk != tc.wantAtRisk {
				t.Errorf("atRisk = %v, want %v (reason: %q)", gotAtRisk, tc.wantAtRisk, gotReason)
			}
			if tc.wantReason != "" && gotReason != tc.wantReason {
				t.Errorf("reason = %q, want %q", gotReason, tc.wantReason)
			}
		})
	}
}

func TestSortByRisk(t *testing.T) {
	// Deliberately shuffled: the healthiest student is first in the input, so
	// a no-op sort would fail.
	students := []StudentStanding{
		{Name: "Healthy", DaysSinceRead: days(0), VelocityPerDay: 40, AtRisk: false},
		{Name: "SlowPace", DaysSinceRead: days(1), VelocityPerDay: 3, AtRisk: true},
		{Name: "NeverRead", DaysSinceRead: nil, AtRisk: true},
		{Name: "SilentLong", DaysSinceRead: days(20), VelocityPerDay: 0, AtRisk: true},
		{Name: "SilentShort", DaysSinceRead: days(9), VelocityPerDay: 0, AtRisk: true},
	}

	sortByRisk(students)

	want := []string{"NeverRead", "SilentLong", "SilentShort", "SlowPace", "Healthy"}
	for i, name := range want {
		if students[i].Name != name {
			var got []string
			for _, s := range students {
				got = append(got, s.Name)
			}
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

// TestSortByRisk_HealthyStudentsNeverOutrankAtRisk is the property that makes
// the list usable: a teacher reading top-down under time pressure must never
// find a thriving student above one who needs help.
func TestSortByRisk_HealthyStudentsNeverOutrankAtRisk(t *testing.T) {
	students := []StudentStanding{
		{Name: "ThrivingA", DaysSinceRead: days(0), VelocityPerDay: 99, AtRisk: false},
		{Name: "Struggling", DaysSinceRead: days(3), VelocityPerDay: 2, AtRisk: true},
		{Name: "ThrivingB", DaysSinceRead: days(0), VelocityPerDay: 80, AtRisk: false},
	}

	sortByRisk(students)

	seenHealthy := false
	for _, s := range students {
		if !s.AtRisk {
			seenHealthy = true
			continue
		}
		if seenHealthy {
			t.Errorf("at-risk student %q sorted below a healthy one", s.Name)
		}
	}
}
