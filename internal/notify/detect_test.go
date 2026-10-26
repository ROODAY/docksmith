package notify

import (
	"testing"
	"time"

	"github.com/chis/docksmith/internal/storage"
)

func snap(status, cur, latest string) Snapshot {
	return Snapshot{Name: "web", Stack: "s", Status: status, CurrentVersion: cur, LatestVersion: latest}
}

func kinds(evs []Event) []EventKind {
	var k []EventKind
	for _, e := range evs {
		k = append(k, e.Kind)
	}
	return k
}

func TestDetect(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := DefaultSettings("h")
	type step struct {
		snaps []Snapshot
		want  []EventKind
	}
	lbl := func(k, v string) map[string]string { return map[string]string{k: v} }
	withLabels := func(sn Snapshot, l map[string]string) Snapshot { sn.Labels = l; return sn }
	latestSnap := func(digest string, l map[string]string) Snapshot {
		return Snapshot{Name: "web", Status: "UPDATE_AVAILABLE", CurrentVersion: "latest", LatestVersion: "latest", CurrentDigest: "sha:a", LatestDigest: digest, Labels: l}
	}
	nv := []EventKind{EventNewVersion}
	tests := []struct {
		name  string
		steps []step
		check func(t *testing.T, st map[string]storage.NotifyContainerState)
	}{
		{"patch once", []step{
			{[]Snapshot{snap("UPDATE_AVAILABLE", "4.0.9", "4.0.10")}, nv},
			{[]Snapshot{snap("UPDATE_AVAILABLE", "4.0.9", "4.0.10")}, nil},
		}, nil},
		{"supersede", []step{
			{[]Snapshot{snap("UPDATE_AVAILABLE", "4.0.9", "4.0.10")}, nv},
			{[]Snapshot{snap("UPDATE_AVAILABLE", "4.0.9", "4.0.11")}, nv},
			{[]Snapshot{snap("UPDATE_AVAILABLE", "4.0.9", "4.0.11")}, nil},
		}, func(t *testing.T, st map[string]storage.NotifyContainerState) {
			if st["web"].LastNotifiedVersion != "4.0.11" {
				t.Fatalf("got %+v", st["web"])
			}
		}},
		{"build bump below min", []step{
			{[]Snapshot{snap("UPDATE_AVAILABLE", "1.2.3-ls10", "1.2.3-ls11")}, nil},
		}, nil},
		{"build bump label", []step{
			{[]Snapshot{withLabels(snap("UPDATE_AVAILABLE", "1.2.3-ls10", "1.2.3-ls11"), lbl("docksmith.notify-level", "build"))}, nv},
		}, nil},
		{"invalid level label falls back", []step{
			{[]Snapshot{withLabels(snap("UPDATE_AVAILABLE", "1.2.3-ls10", "1.2.3-ls11"), lbl("docksmith.notify-level", "bogus"))}, nil},
		}, nil},
		{"latest resolved", []step{
			{[]Snapshot{{Name: "web", Status: "UPDATE_AVAILABLE", CurrentVersion: "1.2.3", LatestVersion: "latest", LatestResolvedVersion: "1.3.0"}}, nv},
		}, nil},
		{"digest off", []step{{[]Snapshot{latestSnap("sha:b", nil)}, nil}}, nil},
		{"digest on", []step{
			{[]Snapshot{latestSnap("sha:b", lbl("docksmith.notify-digest", "true"))}, []EventKind{EventDigestChanged}},
			{[]Snapshot{latestSnap("sha:b", lbl("docksmith.notify-digest", "true"))}, nil},
		}, nil},
		{"muted still tracks", []step{
			{[]Snapshot{withLabels(snap("UPDATE_AVAILABLE", "4.0.9", "4.0.10"), lbl("docksmith.notify", "false"))}, nil},
			{[]Snapshot{snap("UPDATE_AVAILABLE", "4.0.9", "4.0.10")}, nil},
		}, func(t *testing.T, st map[string]storage.NotifyContainerState) {
			if st["web"].LastNotifiedVersion != "4.0.10" {
				t.Fatalf("got %+v", st["web"])
			}
		}},
		{"blocked once", []step{
			{[]Snapshot{snap("UPDATE_AVAILABLE_BLOCKED", "4.0.9", "4.0.10")}, []EventKind{EventNewVersion, EventUpdateBlocked}},
			{[]Snapshot{snap("UPDATE_AVAILABLE_BLOCKED", "4.0.9", "4.0.10")}, nil},
			{[]Snapshot{snap("UPDATE_AVAILABLE_BLOCKED", "4.0.9", "4.0.10")}, nil},
		}, nil},
		{"check failing", []step{
			{[]Snapshot{snap("CHECK_FAILED", "", "")}, nil},
			{[]Snapshot{snap("METADATA_UNAVAILABLE", "", "")}, nil},
			{[]Snapshot{snap("CHECK_FAILED", "", "")}, []EventKind{EventCheckFailing}},
			{[]Snapshot{snap("CHECK_FAILED", "", "")}, nil},
			{[]Snapshot{snap("UP_TO_DATE", "", "")}, nil},
			{[]Snapshot{snap("CHECK_FAILED", "", "")}, nil},
			{[]Snapshot{snap("CHECK_FAILED", "", "")}, nil},
			{[]Snapshot{snap("CHECK_FAILED", "", "")}, []EventKind{EventCheckFailing}},
		}, nil},
		{"compose mismatch transition", []step{
			{[]Snapshot{snap("COMPOSE_MISMATCH", "", "")}, []EventKind{EventComposeMismatch}},
			{[]Snapshot{snap("COMPOSE_MISMATCH", "", "")}, nil},
			{[]Snapshot{snap("UP_TO_DATE", "", "")}, nil},
			{[]Snapshot{snap("COMPOSE_MISMATCH", "", "")}, []EventKind{EventComposeMismatch}},
		}, nil},
		{"missing dropped", []step{
			{[]Snapshot{snap("UPDATE_AVAILABLE", "4.0.9", "4.0.10")}, nv},
			{nil, nil},
		}, func(t *testing.T, st map[string]storage.NotifyContainerState) {
			if len(st) != 0 {
				t.Fatalf("got %+v", st)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prev := map[string]storage.NotifyContainerState{}
			for i, st := range tc.steps {
				before := len(prev)
				evs, next := Detect(prev, st.snaps, s, now)
				if len(prev) != before {
					t.Fatal("prev mutated")
				}
				got := kinds(evs)
				if len(got) != len(st.want) {
					t.Fatalf("step %d: got %v want %v", i, got, st.want)
				}
				for j := range got {
					if got[j] != st.want[j] {
						t.Fatalf("step %d: got %v want %v", i, got, st.want)
					}
				}
				prev = next
			}
			if tc.check != nil {
				tc.check(t, prev)
			}
		})
	}
}

func TestDetectEventFields(t *testing.T) {
	now := time.Now()
	evs, _ := Detect(nil, []Snapshot{{Name: "web", Stack: "s", Status: "UPDATE_AVAILABLE", CurrentVersion: "1.2.3", LatestVersion: "latest", LatestResolvedVersion: "1.3.0"}}, DefaultSettings("h"), now)
	if len(evs) != 1 {
		t.Fatalf("got %v", evs)
	}
	e := evs[0]
	if e.Level != LevelMinor || e.ToVersion != "1.3.0" || e.FromVersion != "1.2.3" || e.Stack != "s" || e.Container != "web" || !e.At.Equal(now) {
		t.Fatalf("bad event %+v", e)
	}
}

func TestMuted(t *testing.T) {
	if !Muted(map[string]string{"docksmith.notify": "false"}) || Muted(nil) || Muted(map[string]string{"docksmith.notify": "true"}) {
		t.Fatal("Muted wrong")
	}
}
