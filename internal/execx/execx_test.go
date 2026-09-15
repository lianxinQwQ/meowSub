package execx

import (
	"errors"
	"strings"
	"testing"
)

func TestRecordRunnerRecords(t *testing.T) {
	r := &RecordRunner{}
	if err := r.Run("cp", "-a", "x", "y"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	out, err := r.RunOutput("paru", "-S", "vim")
	if err != nil || out != "" {
		t.Fatalf("RunOutput = %q,%v", out, err)
	}
	if len(r.Cmds) != 2 {
		t.Fatalf("cmds = %d", len(r.Cmds))
	}
	if r.Cmds[0].Name != "cp" || strings.Join(r.Cmds[0].Args, " ") != "-a x y" {
		t.Errorf("cmd0 = %+v", r.Cmds[0])
	}
}

func TestRecordRunnerFailOn(t *testing.T) {
	r := &RecordRunner{FailOn: map[string]error{"cp": errors.New("boom")}}
	if err := r.Run("cp", "a", "b"); err == nil {
		t.Fatal("want error")
	}
	// 其他命令不受影响
	if err := r.Run("ls"); err != nil {
		t.Fatalf("Run ls: %v", err)
	}
}

func TestRecordRunnerOutputQueue(t *testing.T) {
	r := &RecordRunner{OutputQueue: []OutputEntry{
		{Out: "vim\nneovim\n"},
		{Err: errors.New("net down")},
	}}
	o1, e1 := r.RunOutput("paru", "a")
	if e1 != nil || o1 != "vim\nneovim\n" {
		t.Fatalf("first = %q,%v", o1, e1)
	}
	if _, e2 := r.RunOutput("paru", "b"); e2 == nil {
		t.Fatal("second should fail")
	}
	o3, e3 := r.RunOutput("paru", "c")
	if e3 != nil || o3 != "" {
		t.Fatalf("after queue drained want empty ok, got %q,%v", o3, e3)
	}
}
