package state

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestLoadMissingReturnsEmpty(t *testing.T) {
	st, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.Base != nil || len(st.Nodes) != 0 {
		t.Errorf("want empty state, got %+v", st)
	}
}

func TestSaveLoadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().Truncate(time.Second)
	want := &State{
		Version:    1,
		ConfigHash: "abc123",
		Base: &BaseRecord{
			Path: filepath.Join(dir, "arch"), Packages: []string{"base", "paru"},
			CreatedAt: now, UpdatedAt: now,
		},
		Nodes: []*NodeRecord{{
			Name: "tool", Path: filepath.Join(dir, "tool"), Kind: KindFinal,
			Parent: "arch", InstallSet: []string{"vim"}, CreatedAt: now, UpdatedAt: now,
		}},
	}
	if err := want.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.ConfigHash != "abc123" || got.Base == nil || len(got.Nodes) != 1 {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if !reflect.DeepEqual(got.Nodes[0].InstallSet, []string{"vim"}) {
		t.Errorf("install_set = %v", got.Nodes[0].InstallSet)
	}
}

func TestMarkerRoundtrip(t *testing.T) {
	dir := t.TempDir()
	m := &Marker{Kind: KindIntermediate, Name: "code+dev-1a2b3c", Parent: "arch",
		Groups: []string{"code", "dev"}, InstallSet: []string{"cargo", "rust"}}
	if err := WriteMarker(dir, m); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	got, err := ReadMarker(dir)
	if err != nil {
		t.Fatalf("ReadMarker: %v", err)
	}
	if got == nil || got.Name != m.Name || got.Parent != "arch" ||
		!reflect.DeepEqual(got.InstallSet, m.InstallSet) {
		t.Fatalf("marker mismatch: %+v", got)
	}
	if err := RemoveMarker(dir); err != nil {
		t.Fatalf("RemoveMarker: %v", err)
	}
	got, err = ReadMarker(dir)
	if err != nil || got != nil {
		t.Fatalf("after remove want nil,nil got %+v,%v", got, err)
	}
}

func TestReadMarkerMissingNil(t *testing.T) {
	got, err := ReadMarker(t.TempDir())
	if err != nil || got != nil {
		t.Fatalf("want nil,nil got %+v,%v", got, err)
	}
}

func TestSaveCreatesMetaDir(t *testing.T) {
	dir := t.TempDir()
	st := &State{Version: 1}
	if err := st.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".meowsub", "state.json")); err != nil {
		t.Fatalf("state.json missing: %v", err)
	}
}
