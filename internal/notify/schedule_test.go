package notify

import (
	"testing"
	"time"
)

func TestValidateSchedule(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		expr    string
		wantErr bool
	}{
		{"*/5 * * * *", false},
		{"*/2 * * * *", true},
		{"0-4 8 * * *", true},
		{"0 8 * * 1", false},
		{"bogus", true},
		{"0 8 31 2 *", true},
	}
	for _, tt := range tests {
		err := ValidateSchedule(tt.expr, 5*time.Minute, time.UTC, now)
		if (err != nil) != tt.wantErr {
			t.Errorf("ValidateSchedule(%q) err=%v, wantErr=%v", tt.expr, err, tt.wantErr)
		}
	}
}

func TestNextFires(t *testing.T) {
	now := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	got, err := NextFires("0 8 * * *", time.UTC, now, 2)
	if err != nil || len(got) != 2 {
		t.Fatalf("got %v, %v", got, err)
	}
	if !got[0].Equal(time.Date(2026, 1, 2, 8, 0, 0, 0, time.UTC)) || !got[1].Equal(got[0].Add(24*time.Hour)) {
		t.Errorf("unexpected fires %v", got)
	}
}
