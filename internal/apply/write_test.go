package apply

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mhvtl-control/internal/conffile"
)

// changeSet is a request touching every kind of step: mhvtl.conf, a
// library_contents written, device.conf, and a library_contents deleted.
func changeSet(e *env) *Request {
	dc := string(e.read("device.conf"))
	i := strings.Index(dc, "Library: 30")
	return &Request{
		Op: OpApply,
		Files: []File{
			e.edit("device.conf", dc[i:], ""),
			e.edit("library_contents.10", "Slot 1:", "Slot 1: "),
			e.edit("mhvtl.conf", "CAPACITY=500", "CAPACITY=1000"),
		},
		Delete: []Delete{{Name: "library_contents.30", BaseSHA256: e.sha("library_contents.30")}},
	}
}

func TestWriteOrder(t *testing.T) {
	e := newEnv(t)
	var got []string
	for _, c := range changesOf(changeSet(e)) {
		got = append(got, fmt.Sprintf("%s:%v", c.name, c.del))
	}
	want := "[mhvtl.conf:false library_contents.10:false device.conf:false library_contents.30:true]"
	if fmt.Sprint(got) != want {
		t.Errorf("order %v, want %v", got, want)
	}
}

// TestWriteKillPoints kills the helper after every step of a write. Each
// file is then whole, old or new, never partial; and the next request
// recovers every file to its original from the intent alone.
func TestWriteKillPoints(t *testing.T) {
	steps := []string{"intent", "wrote mhvtl.conf", "wrote library_contents.10", "wrote device.conf", "wrote library_contents.30"}
	for k, step := range steps {
		e := newEnv(t)
		req := changeSet(e)
		orig := map[string][]byte{}
		for _, n := range []string{"device.conf", "library_contents.10", "library_contents.30", "mhvtl.conf"} {
			orig[n] = e.read(n)
		}
		os.Chmod(filepath.Join(e.dir, "library_contents.30"), 0o640)
		e.h.killPoint = func(s string) bool { return s == step }
		err := e.h.writeFiles(context.Background(), &Intent{V: Version, Op: OpApply, Actor: "admin"}, changesOf(req), newFileSet(e.dir))
		if !errors.Is(err, errKilled) {
			t.Fatalf("%s: err %v, want killed", step, err)
		}
		written := map[string]bool{}
		for _, s := range steps[1 : k+1] {
			written[strings.TrimPrefix(s, "wrote ")] = true
		}
		for _, f := range req.Files {
			got := e.read(f.Name)
			want := orig[f.Name]
			if written[f.Name] {
				want = f.Content
			}
			if !bytes.Equal(got, want) {
				t.Errorf("killed after %q: %s is neither whole old nor whole new as expected", step, f.Name)
			}
		}
		_, err = os.Stat(filepath.Join(e.dir, "library_contents.30"))
		if gone := os.IsNotExist(err); gone != written["library_contents.30"] {
			t.Errorf("killed after %q: library_contents.30 gone %v", step, gone)
		}

		e.h.killPoint = func(string) bool { return false }
		if s := e.do(Request{Op: OpHello}).State; s != StateIncomplete {
			t.Errorf("killed after %q: hello %s, want incomplete_apply", step, s)
		}
		resp := e.do(Request{Op: OpPlan, Files: []File{e.edit("mhvtl.conf", "VERBOSE=1", "VERBOSE=2")}})
		if resp.Result != ResultRecovered || len(resp.Conflicts) != 0 {
			t.Fatalf("killed after %q: next plan %+v, want recovered", step, resp)
		}
		for n, b := range orig {
			if !bytes.Equal(e.read(n), b) {
				t.Errorf("killed after %q: %s not recovered", step, n)
			}
		}
		if st, err := os.Stat(filepath.Join(e.dir, "library_contents.30")); err != nil || st.Mode().Perm() != 0o640 {
			t.Errorf("killed after %q: restored library_contents.30 %v, %v; want mode 0640", step, st, err)
		}
		if _, err := os.Stat(e.h.intentPath()); !os.IsNotExist(err) {
			t.Errorf("killed after %q: intent left (%v)", step, err)
		}
		if s := e.do(Request{Op: OpHello}).State; s != StateReady {
			t.Errorf("killed after %q: hello %s after recovery", step, s)
		}
	}
}

// TestWriteCancel: a SIGTERM (the context ending) mid-write restores the
// files written so far and removes the intent.
func TestWriteCancel(t *testing.T) {
	e := newEnv(t)
	req := changeSet(e)
	before := e.sha("mhvtl.conf")
	ctx, cancel := context.WithCancel(context.Background())
	e.h.killPoint = func(s string) bool {
		if s == "wrote library_contents.10" {
			cancel()
		}
		return false
	}
	err := e.h.writeFiles(ctx, &Intent{V: Version}, changesOf(req), newFileSet(e.dir))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want canceled", err)
	}
	if e.sha("mhvtl.conf") != before || e.sha("library_contents.10") != req.Files[1].BaseSHA256 {
		t.Error("files not restored")
	}
	if _, err := os.Stat(e.h.intentPath()); !os.IsNotExist(err) {
		t.Errorf("intent left (%v)", err)
	}
}

// TestWriteRollbackCopies: a completed write keeps the replaced files and
// the deleted one under rollback/<time>/, and only the newest 50 such
// directories are kept; other entries there are left alone.
func TestWriteRollbackCopies(t *testing.T) {
	e := newEnv(t)
	rb := filepath.Join(e.state, rollbackDir)
	os.MkdirAll(filepath.Join(rb, "not-a-rollback"), 0o700)
	for i := 0; i < keepRollback+3; i++ {
		os.MkdirAll(filepath.Join(rb, fmt.Sprintf("20200101T000000.%09dZ", i)), 0o700)
	}
	req := changeSet(e)
	orig := map[string][]byte{}
	for _, n := range []string{"device.conf", "library_contents.10", "library_contents.30", "mhvtl.conf"} {
		orig[n] = e.read(n)
	}
	it := &Intent{V: Version}
	if err := e.h.writeFiles(context.Background(), it, changesOf(req), newFileSet(e.dir)); err != nil {
		t.Fatal(err)
	}
	for n, b := range orig {
		got, err := os.ReadFile(filepath.Join(rb, it.RollbackDir, n))
		if err != nil || !bytes.Equal(got, b) {
			t.Errorf("rollback copy of %s: %v", n, err)
		}
	}
	ents, _ := os.ReadDir(rb)
	if len(ents) != keepRollback+1 { // 50 stamps (the new one included) and not-a-rollback
		t.Errorf("%d entries in rollback/, want %d", len(ents), keepRollback+1)
	}
	if _, err := os.Stat(filepath.Join(rb, "not-a-rollback")); err != nil {
		t.Errorf("foreign entry removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rb, "20200101T000000.000000003Z")); !os.IsNotExist(err) {
		t.Errorf("old rollback kept: %v", err)
	}
}

// TestAuditFields checks the audit record of AC 4: who (actor, peer pid
// and cgroup), when, what (file hashes before and after, a diff), the
// units, the result, and that it reaches the journal.
func TestAuditFields(t *testing.T) {
	e := newEnv(t)
	dc := e.edit("device.conf", "Compression type: lzo", "Compression type: zlib")
	e.do(Request{Op: OpApply, Actor: "admin@203.0.113.7", Files: []File{dc}, ExpectUnits: []string{"vtllibrary@10.service", "vtltape@11.service"}})
	lines := e.auditLines()
	if len(lines) != 1 {
		t.Fatalf("audit lines %v", lines)
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"ts", "actor", "peer", "op", "files", "diff", "units", "result", "orphaned_media"} {
		if _, ok := rec[k]; !ok {
			t.Errorf("audit record has no %q: %s", k, lines[0])
		}
	}
	var r AuditRecord
	json.Unmarshal([]byte(lines[0]), &r)
	if r.Actor != "admin@203.0.113.7" || r.Peer != e.peer || r.Op != OpApply || r.TS.IsZero() ||
		len(r.Files) != 1 || r.Files[0] != (AuditFile{Name: "device.conf", Before: dc.BaseSHA256, After: conffile.Hash(dc.Content)}) ||
		!strings.Contains(r.Diff, "- Compression type: lzo") || !strings.Contains(r.Diff, "+ Compression type: zlib") ||
		fmt.Sprint(r.Units) != "[vtllibrary@10.service vtltape@11.service]" || r.Result != ResultRefused {
		t.Errorf("audit record %+v", r)
	}
	if len(e.journal) != 1 || e.journal[0]["MESSAGE_ID"] != msgRefused || e.journal[0]["MHVTL_APPLY_ACTOR"] != r.Actor ||
		e.journal[0]["MHVTL_APPLY_PEER_PID"] != "4242" {
		t.Errorf("journal %v", e.journal)
	}

	// A diff is capped.
	big := e.edit("device.conf", "# Each 'record'", strings.Repeat("# long\n", maxDiff/4))
	_, diff, cut := auditFiles(changesOf(&Request{Files: []File{big}}), newFileSet(e.dir))
	if !cut || len(diff) != maxDiff {
		t.Errorf("diff of %d bytes, truncated %v", len(diff), cut)
	}
}

// TestAuditNoDiffBeforeValidation: a request a guard refuses leaves its
// file hashes in the audit, not its content.
func TestAuditNoDiffBeforeValidation(t *testing.T) {
	e := newEnv(t)
	mc := e.edit("mhvtl.conf", "CAPACITY=500", "CAPACITY=500\n"+strings.Repeat("# filler\n", 1000))
	e.refused("not derived", Request{Op: OpPlan, Files: []File{mc}}, ErrMhvtlConfDerived)
	var r AuditRecord
	lines := e.auditLines()
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &r); err != nil {
		t.Fatal(err)
	}
	if r.Diff != "" || r.DiffTruncated || len(r.Files) != 1 || r.Files[0].After != conffile.Hash(mc.Content) {
		t.Errorf("audit record %+v", r)
	}
}

// TestAuditRotation: audit.jsonl is rotated to audit.jsonl.1 before it
// would pass maxAudit, and only one older file is kept.
func TestAuditRotation(t *testing.T) {
	defer func(m int64) { maxAudit = m }(maxAudit)
	maxAudit = 2048
	e := newEnv(t)
	path := filepath.Join(e.state, auditFile)
	const n = 40
	for i := 0; i < n; i++ {
		e.do(Request{Op: OpPlan, Actor: fmt.Sprint("actor-", i), ForceToken: "x"})
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() > maxAudit {
			t.Fatalf("audit.jsonl is %d bytes, over %d", fi.Size(), maxAudit)
		}
	}
	old, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if len(old) == 0 || int64(len(old)) > maxAudit {
		t.Errorf("audit.jsonl.1 is %d bytes", len(old))
	}
	if _, err := os.Stat(path + ".2"); !os.IsNotExist(err) {
		t.Errorf("audit.jsonl.2: %v", err)
	}
	// The newest record is last in audit.jsonl; the rotated file ends
	// where audit.jsonl begins.
	cur := e.auditLines()
	if !strings.Contains(cur[len(cur)-1], fmt.Sprintf(`"actor-%d"`, n-1)) {
		t.Errorf("last record %s", cur[len(cur)-1])
	}
	var first, last AuditRecord
	olds := strings.Split(strings.TrimSpace(string(old)), "\n")
	json.Unmarshal([]byte(olds[len(olds)-1]), &last)
	json.Unmarshal([]byte(cur[0]), &first)
	var a, b int
	fmt.Sscanf(last.Actor, "actor-%d", &a)
	fmt.Sscanf(first.Actor, "actor-%d", &b)
	if b != a+1 {
		t.Errorf("audit.jsonl.1 ends at %s, audit.jsonl starts at %s", last.Actor, first.Actor)
	}
}
