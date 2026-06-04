package progress

import "testing"

func TestRecomputeStatus(t *testing.T) {
	cases := []struct {
		name string
		in   []PlatformState
		want string
	}{
		{"empty", nil, StatusFailed},
		{"running until all terminal", []PlatformState{{Status: StatusSuccess}, {Status: StatusRunning}}, StatusRunning},
		{"all success", []PlatformState{{Status: StatusSuccess}, {Status: StatusSuccess}}, StatusSuccess},
		{"all failed", []PlatformState{{Status: StatusFailed}, {Status: StatusFailed}}, StatusFailed},
		{"mixed terminal -> partial", []PlatformState{{Status: StatusSuccess}, {Status: StatusFailed}}, StatusPartial},
		{"a partial platform -> partial", []PlatformState{{Status: StatusSuccess}, {Status: StatusPartial}}, StatusPartial},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := Snapshot{Platforms: c.in}
			s.recomputeStatus()
			if s.Status != c.want {
				t.Fatalf("got %q want %q", s.Status, c.want)
			}
		})
	}
}
