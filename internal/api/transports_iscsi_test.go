package api

import (
	"bytes"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mhvtl-control/internal/apply"
	"mhvtl-control/internal/conffile"
)

const (
	chapSecret   = "S3cretChapOne"
	mutualSecret = "S3cretMutualTwo"
	baseName     = "iqn.2026-11.lan.example:vtl"
	pbsIQN       = "iqn.2024-01.com.example:pbs"
)

// fabricsSample binds library 10 with CHAP and mutual CHAP, and library 30
// under a Target name of its own.
const fabricsSample = "VERSION: 1\n" +
	"\n" +
	"iSCSI:\n" +
	" Portal: 192.0.2.10:3260\n" +
	" Base name: " + baseName + "\n" +
	"\n" +
	"iSCSI target: 10\n" +
	" Initiator name: " + pbsIQN + "\n" +
	" CHAP name: pbs\n" +
	" CHAP secret: " + chapSecret + "\n" +
	" Mutual CHAP name: vtl\n" +
	" Mutual CHAP secret: " + mutualSecret + "\n" +
	"\n" +
	"iSCSI target: 30\n" +
	" Initiator name: iqn.2024-01.com.example:bareos\n" +
	" Target name: iqn.2026-11.lan.example:custom30\n"

// iscsiEnv is newWriteEnv with fabrics.conf holding content ("" for none)
// and a helper that validates and applies with action.
func iscsiEnv(t *testing.T, content, action string, edit func(*Config)) *writeEnv {
	t.Helper()
	e := newWriteEnv(t, edit)
	if content != "" {
		if err := os.WriteFile(filepath.Join(e.dir, "fabrics.conf"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	e.h.answer = func(req apply.Request) apply.Response {
		if req.Op == apply.OpFabricsPlan {
			return apply.Response{Result: apply.ResultValidated, FabricsAction: action}
		}
		return apply.Response{Result: apply.ResultApplied, FabricsAction: action}
	}
	return e
}

// fabricsReqs are the helper requests sent so far.
func (e *writeEnv) fabricsReqs() []apply.Request {
	e.h.mu.Lock()
	defer e.h.mu.Unlock()
	return append([]apply.Request(nil), e.h.reqs...)
}

// noSecrets fails when body holds a CHAP secret.
func noSecrets(t *testing.T, what, body string, secrets ...string) {
	t.Helper()
	for _, s := range append([]string{chapSecret, mutualSecret}, secrets...) {
		if strings.Contains(body, s) {
			t.Errorf("%s holds secret %q: %s", what, s, body)
		}
	}
}

type fieldErrBody struct {
	Error fieldErrorDetail `json:"error"`
}

func TestISCSIConfigAbsent(t *testing.T) {
	e := iscsiEnv(t, "", "reload", nil)
	rec := e.call(http.MethodGet, "/api/v1/transports/iscsi/config", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	got := decodeBody[iscsiConfigDTO](t, rec)
	want := iscsiConfigDTO{ISCSI: iscsiRecordDTO{Portals: []string{}}, Targets: []iscsiTargetDTO{},
		Problems: []iscsiProblemDTO{}, Warnings: []string{}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if !strings.Contains(rec.Body.String(), `"portals":[]`) || !strings.Contains(rec.Body.String(), `"targets":[]`) {
		t.Errorf("lists not empty arrays: %s", rec.Body)
	}
}

func TestISCSIConfigSample(t *testing.T) {
	e := iscsiEnv(t, fabricsSample, "reload", nil)
	rec := e.call(http.MethodGet, "/api/v1/transports/iscsi/config", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	noSecrets(t, "GET config", rec.Body.String())
	got := decodeBody[iscsiConfigDTO](t, rec)
	if !got.Exists || got.SHA256 != conffile.Hash([]byte(fabricsSample)) {
		t.Errorf("exists %v sha %q", got.Exists, got.SHA256)
	}
	if !reflect.DeepEqual(got.ISCSI, iscsiRecordDTO{Present: true, Portals: []string{"192.0.2.10:3260"}, BaseName: baseName}) {
		t.Errorf("iscsi = %+v", got.ISCSI)
	}
	want := []iscsiTargetDTO{
		{ID: 10, WWN: baseName + ".lib10", Initiators: []string{pbsIQN}, CHAP: true, CHAPName: "pbs",
			MutualCHAP: true, MutualCHAPName: "vtl", Device: &iscsiDeviceDTO{Kind: "library", ID: 10}},
		{ID: 30, TargetName: "iqn.2026-11.lan.example:custom30", WWN: "iqn.2026-11.lan.example:custom30",
			Initiators: []string{"iqn.2024-01.com.example:bareos"}, Device: &iscsiDeviceDTO{Kind: "library", ID: 30}},
	}
	if !reflect.DeepEqual(got.Targets, want) {
		t.Errorf("targets = %+v\nwant %+v", got.Targets, want)
	}
	if len(got.Problems) != 0 || len(got.Warnings) != 0 {
		t.Errorf("problems %+v warnings %q", got.Problems, got.Warnings)
	}
}

// Targets naming a drive of a library, a standalone drive and no device:
// the device mapping, the derived names and CheckPlan's problems.
func TestISCSIConfigDevicesAndProblems(t *testing.T) {
	e := iscsiEnv(t, "VERSION: 1\niSCSI:\n Base name: "+baseName+"\n"+
		"iSCSI target: 11\n Initiator name: "+pbsIQN+"\n"+
		"iSCSI target: 50\n Initiator name: "+pbsIQN+"\n"+
		"iSCSI target: 99\n Initiator name: "+pbsIQN+"\n", "reload", nil)
	f, err := os.OpenFile(filepath.Join(e.dir, "device.conf"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("\nDrive: 50 CHANNEL: 00 TARGET: 13 LUN: 00\n")
	f.Close()
	got := decodeBody[iscsiConfigDTO](t, e.call(http.MethodGet, "/api/v1/transports/iscsi/config", nil))
	if len(got.Targets) != 3 {
		t.Fatalf("targets = %+v", got.Targets)
	}
	for k, want := range []struct {
		dev *iscsiDeviceDTO
		wwn string
	}{
		{&iscsiDeviceDTO{"drive", 11}, baseName + ".tape11"},
		{&iscsiDeviceDTO{"drive", 50}, baseName + ".tape50"},
		{nil, ""},
	} {
		if g := got.Targets[k]; !reflect.DeepEqual(g.Device, want.dev) || g.WWN != want.wwn {
			t.Errorf("target %d: device %+v wwn %q, want %+v %q", g.ID, g.Device, g.WWN, want.dev, want.wwn)
		}
	}
	if len(got.Problems) != 2 || got.Problems[0].Line != 4 || !strings.Contains(got.Problems[0].Message, "drive belongs to library 10") ||
		got.Problems[1].Line != 8 || !strings.Contains(got.Problems[1].Message, "no Library or Drive 99") {
		t.Errorf("problems = %+v", got.Problems)
	}

	// A parse problem is reported as it is, without CheckPlan.
	if err := os.WriteFile(filepath.Join(e.dir, "fabrics.conf"), []byte("VERSION: 1\nbogus\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got = decodeBody[iscsiConfigDTO](t, e.call(http.MethodGet, "/api/v1/transports/iscsi/config", nil))
	if len(got.Problems) != 1 || got.Problems[0].Line != 2 {
		t.Errorf("parse problems = %+v", got.Problems)
	}
}

// An unreadable fabrics.conf is a warning without the path, not an error.
func TestISCSIConfigUnreadable(t *testing.T) {
	e := iscsiEnv(t, "", "reload", nil)
	if err := os.Mkdir(filepath.Join(e.dir, "fabrics.conf"), 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	defer log.SetOutput(log.Writer())
	log.SetOutput(&buf)
	rec := e.call(http.MethodGet, "/api/v1/transports/iscsi/config", nil)
	got := decodeBody[iscsiConfigDTO](t, rec)
	if rec.Code != http.StatusOK || !reflect.DeepEqual(got.Warnings, []string{"fabrics.conf: unreadable"}) {
		t.Errorf("status %d warnings %q", rec.Code, got.Warnings)
	}
	if strings.Contains(rec.Body.String(), e.dir) || !strings.Contains(buf.String(), e.dir) {
		t.Errorf("path in body %s or not in log %q", rec.Body, buf.String())
	}
}

func TestISCSIPatchDryRunAndApply(t *testing.T) {
	e := iscsiEnv(t, fabricsSample, "reload", nil)
	rec := e.call(http.MethodPatch, "/api/v1/transports/iscsi", map[string]any{
		"portals": []string{"192.0.2.10:3260", "192.0.2.11"}, "dry_run": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("dry run: %d %s", rec.Code, rec.Body)
	}
	got := decodeBody[iscsiWriteDTO](t, rec)
	if !got.DryRun || got.Action != "reload" || got.Result != apply.ResultValidated || got.Error != nil ||
		!strings.Contains(got.Diff, "+ Portal: 192.0.2.11\n") {
		t.Errorf("dry run = %+v", got)
	}
	reqs := e.fabricsReqs()
	if len(reqs) != 1 || reqs[0].Op != apply.OpFabricsPlan || len(reqs[0].Files) != 1 ||
		reqs[0].Files[0].Name != "fabrics.conf" || reqs[0].Files[0].BaseSHA256 != conffile.Hash([]byte(fabricsSample)) {
		t.Fatalf("helper requests = %+v", reqs)
	}
	sent := string(reqs[0].Files[0].Content)
	if !strings.Contains(sent, " Portal: 192.0.2.11\n") || !strings.Contains(sent, chapSecret) {
		t.Errorf("content sent:\n%s", sent)
	}
	if b, _ := os.ReadFile(filepath.Join(e.dir, "fabrics.conf")); string(b) != fabricsSample {
		t.Errorf("dry run wrote fabrics.conf:\n%s", b)
	}

	rec = e.call(http.MethodPatch, "/api/v1/transports/iscsi", map[string]any{"base_name": "iqn.2026-12.lan.example:vtl2"})
	if rec.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body)
	}
	got = decodeBody[iscsiWriteDTO](t, rec)
	if got.DryRun || got.Result != apply.ResultApplied || got.Target != nil {
		t.Errorf("apply = %+v", got)
	}
	if reqs = e.fabricsReqs(); len(reqs) != 2 || reqs[1].Op != apply.OpFabrics ||
		!strings.Contains(string(reqs[1].Files[0].Content), " Base name: iqn.2026-12.lan.example:vtl2\n") ||
		!strings.HasPrefix(reqs[1].Actor, "mhvtl-control API client ") {
		t.Errorf("apply request = %+v", reqs[1:])
	}

	// Removing the base name leaves target 10 without a name: 422.
	rec = e.call(http.MethodPatch, "/api/v1/transports/iscsi", map[string]any{"base_name": ""})
	if body := decodeBody[fieldErrBody](t, rec); rec.Code != http.StatusUnprocessableEntity ||
		body.Error.Code != apply.ErrInvalid || body.Error.Field != "base_name" {
		t.Errorf("no base name: %d %s", rec.Code, rec.Body)
	}
	// An invalid portal: 422 field portals.
	rec = e.call(http.MethodPatch, "/api/v1/transports/iscsi", map[string]any{"portals": []string{"not-an-address"}})
	if body := decodeBody[fieldErrBody](t, rec); rec.Code != http.StatusUnprocessableEntity || body.Error.Field != "portals" {
		t.Errorf("bad portal: %d %s", rec.Code, rec.Body)
	}
	if rec = e.call(http.MethodPatch, "/api/v1/transports/iscsi", map[string]any{"dry_run": true}); rec.Code != http.StatusBadRequest {
		t.Errorf("nothing to change: %d %s", rec.Code, rec.Body)
	}
	if rec = e.call(http.MethodPatch, "/api/v1/transports/iscsi", map[string]any{"portal": []string{}}); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown field: %d %s", rec.Code, rec.Body)
	}
	if n := len(e.fabricsReqs()); n != 2 {
		t.Errorf("%d helper requests, want 2", n)
	}
}

// A first bind creates the iSCSI record; without a Base name it is
// refused on base_name, with one it goes to the helper.
func TestISCSIBindNewRecord(t *testing.T) {
	e := iscsiEnv(t, "", "start", nil)
	bind := map[string]any{"library": 10, "initiators": []string{pbsIQN}, "dry_run": true}
	rec := e.call(http.MethodPost, "/api/v1/transports/iscsi/bind", bind)
	if body := decodeBody[fieldErrBody](t, rec); rec.Code != http.StatusUnprocessableEntity ||
		body.Error.Code != apply.ErrInvalid || body.Error.Field != "base_name" {
		t.Fatalf("no base name: %d %s", rec.Code, rec.Body)
	}
	if n := len(e.fabricsReqs()); n != 0 {
		t.Fatalf("%d helper requests", n)
	}
	bind["target_name"] = "iqn.2026-11.lan.example:lib10"
	rec = e.call(http.MethodPost, "/api/v1/transports/iscsi/bind", bind)
	if rec.Code != http.StatusOK {
		t.Fatalf("with target name: %d %s", rec.Code, rec.Body)
	}
	reqs := e.fabricsReqs()
	if len(reqs) != 1 || reqs[0].Files[0].BaseSHA256 != "" {
		t.Fatalf("requests = %+v", reqs)
	}
	c := conffile.ParseFabricsConf(reqs[0].Files[0].Content)
	if _, _, present := c.ISCSI(); !present || len(c.Problems) != 0 {
		t.Errorf("no iSCSI record or problems %v:\n%s", c.Problems, reqs[0].Files[0].Content)
	}
	if ts := c.Targets(); len(ts) != 1 || ts[0].ID != 10 || !reflect.DeepEqual(ts[0].Initiators, []string{pbsIQN}) {
		t.Errorf("targets = %+v", ts)
	}

	// A new record needs an initiator.
	rec = e.call(http.MethodPost, "/api/v1/transports/iscsi/bind", map[string]any{"library": 10, "dry_run": true})
	if body := decodeBody[fieldErrBody](t, rec); rec.Code != http.StatusUnprocessableEntity || body.Error.Field != "initiators" {
		t.Errorf("no initiators: %d %s", rec.Code, rec.Body)
	}
}

func TestISCSIBindExisting(t *testing.T) {
	e := iscsiEnv(t, fabricsSample, "reload", nil)
	// Initiators omitted and CHAP omitted: both kept.
	rec := e.call(http.MethodPost, "/api/v1/transports/iscsi/bind", map[string]any{
		"library": 10, "target_name": "iqn.2026-11.lan.example:t10", "dry_run": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("bind: %d %s", rec.Code, rec.Body)
	}
	noSecrets(t, "bind dry run", rec.Body.String())
	sent := e.fabricsReqs()[0].Files[0].Content
	ts := conffile.ParseFabricsConf(sent).Targets()
	if !ts[0].HasCHAP || !ts[0].HasMutualCHAP || !reflect.DeepEqual(ts[0].Initiators, []string{pbsIQN}) ||
		ts[0].TargetName != "iqn.2026-11.lan.example:t10" || !bytes.Contains(sent, []byte(" CHAP secret: "+chapSecret+"\n")) {
		t.Errorf("CHAP or initiators lost:\n%s", sent)
	}

	// clear_chap drops CHAP and mutual CHAP.
	rec = e.call(http.MethodPost, "/api/v1/transports/iscsi/bind", map[string]any{"library": 10, "clear_chap": true, "dry_run": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("clear_chap: %d %s", rec.Code, rec.Body)
	}
	got := decodeBody[iscsiWriteDTO](t, rec)
	if !strings.Contains(got.Diff, "- CHAP secret: "+conffile.SecretMask+"\n") || !strings.Contains(got.Diff, "- Mutual CHAP name: vtl\n") {
		t.Errorf("diff:\n%s", got.Diff)
	}
	noSecrets(t, "clear_chap", rec.Body.String())
	sent = e.fabricsReqs()[1].Files[0].Content
	if ts := conffile.ParseFabricsConf(sent).Targets(); ts[0].HasCHAP || ts[0].HasMutualCHAP || conffile.ParseFabricsConf(sent).HasSecret() {
		t.Errorf("CHAP kept:\n%s", sent)
	}

	// A new CHAP secret goes to the helper, never into the answer.
	const newSecret = "BrandNewSecret77"
	rec = e.call(http.MethodPost, "/api/v1/transports/iscsi/bind", map[string]any{
		"library": 30, "chap": map[string]string{"name": "bareos", "secret": newSecret}, "dry_run": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("set chap: %d %s", rec.Code, rec.Body)
	}
	noSecrets(t, "set chap", rec.Body.String(), newSecret)
	if !bytes.Contains(e.fabricsReqs()[2].Files[0].Content, []byte(" CHAP secret: "+newSecret+"\n")) {
		t.Errorf("secret not sent")
	}
}

func TestISCSIBindInvalid(t *testing.T) {
	e := iscsiEnv(t, fabricsSample, "reload", nil)
	for _, c := range []struct {
		name   string
		body   map[string]any
		status int
		field  string
	}{
		{"bad initiator", map[string]any{"library": 10, "initiators": []string{"IQN.Upper"}}, 422, "initiators"},
		{"not in device.conf", map[string]any{"library": 77, "initiators": []string{pbsIQN}}, 422, "library"},
		{"drive of a library", map[string]any{"library": 11, "initiators": []string{pbsIQN}}, 422, "library"},
		{"bad target name", map[string]any{"library": 10, "target_name": "nope"}, 422, "target_name"},
		{"chap without secret", map[string]any{"library": 10, "chap": map[string]string{"name": "a"}}, 422, "chap"},
		{"mutual same secret", map[string]any{"library": 10, "mutual_chap": map[string]string{"name": "m", "secret": chapSecret}}, 422, "mutual_chap"},
		{"chap set and cleared", map[string]any{"library": 10, "clear_chap": true, "chap": map[string]string{"name": "a", "secret": "abcdefghijkl"}}, 422, "chap"},
		{"blank in secret", map[string]any{"library": 10, "chap": map[string]string{"name": "a", "secret": "Leaky Secret9"}}, 422, "chap"},
		{"negative library", map[string]any{"library": -1, "initiators": []string{pbsIQN}}, 400, ""},
		{"no library", map[string]any{"initiators": []string{pbsIQN}}, 400, ""},
	} {
		rec := e.call(http.MethodPost, "/api/v1/transports/iscsi/bind", c.body)
		body := decodeBody[fieldErrBody](t, rec)
		if rec.Code != c.status || body.Error.Field != c.field {
			t.Errorf("%s: %d %s, want %d field %q", c.name, rec.Code, rec.Body, c.status, c.field)
		}
		noSecrets(t, c.name, rec.Body.String(), "Leaky Secret9", "Leaky")
	}
	if n := len(e.fabricsReqs()); n != 0 {
		t.Errorf("%d helper requests", n)
	}
}

func TestISCSIUnbind(t *testing.T) {
	e := iscsiEnv(t, fabricsSample, "reload", nil)
	rec := e.call(http.MethodPost, "/api/v1/transports/iscsi/unbind", map[string]any{"library": 10, "dry_run": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("unbind: %d %s", rec.Code, rec.Body)
	}
	noSecrets(t, "unbind", rec.Body.String())
	got := decodeBody[iscsiWriteDTO](t, rec)
	if !strings.Contains(got.Diff, "-iSCSI target: 10\n") || !strings.Contains(got.Diff, "- Mutual CHAP secret: "+conffile.SecretMask+"\n") {
		t.Errorf("diff:\n%s", got.Diff)
	}
	sent := e.fabricsReqs()[0].Files[0].Content
	if ts := conffile.ParseFabricsConf(sent).Targets(); len(ts) != 1 || ts[0].ID != 30 || bytes.Contains(sent, []byte(chapSecret)) {
		t.Errorf("sent:\n%s", sent)
	}
	rec = e.call(http.MethodPost, "/api/v1/transports/iscsi/unbind", map[string]any{"library": 12})
	if body := decodeBody[fieldErrBody](t, rec); rec.Code != http.StatusNotFound || body.Error.Code != "not_bound" {
		t.Errorf("not bound: %d %s", rec.Code, rec.Body)
	}
	absent := iscsiEnv(t, "", "reload", nil)
	rec = absent.call(http.MethodPost, "/api/v1/transports/iscsi/unbind", map[string]any{"library": 10})
	if body := decodeBody[fieldErrBody](t, rec); rec.Code != http.StatusNotFound || body.Error.Code != "not_bound" {
		t.Errorf("no file: %d %s", rec.Code, rec.Body)
	}
	if n := len(e.fabricsReqs()) + len(absent.fabricsReqs()); n != 1 {
		t.Errorf("%d helper requests, want 1", n)
	}
}

// The helper's refusals and failures keep its status mapping, carry the
// diff, and are masked even when the helper quotes a secret.
func TestISCSIHelperAnswers(t *testing.T) {
	cur := "abc123"
	for _, c := range []struct {
		name   string
		resp   apply.Response
		status int
		code   string
	}{
		{"backend", apply.Response{Result: apply.ResultRefused, Error: apply.ErrFabricsBackend, Message: "MHVTL_BACKEND is mhvtl"}, 422, apply.ErrFabricsBackend},
		{"recovery", apply.Response{Result: apply.ResultRefused, Error: apply.ErrFabricsState, Message: "failed"}, 422, apply.ErrFabricsState},
		{"conflict", apply.Response{Result: apply.ResultConflict, File: "fabrics.conf", CurrentSHA256: &cur}, 409, apply.ResultConflict},
		{"rolled back", apply.Response{Result: apply.ResultRolledBack, Error: apply.ErrFabricsApply, FabricsAction: "reload",
			FailedUnit: apply.FabricsUnit, Message: "reload failed near " + chapSecret, JournalTail: "chap " + mutualSecret + " rejected",
			RollbackDir: "/var/lib/x"}, 500, apply.ResultRolledBack},
		{"error", apply.Response{Result: apply.ResultError, Error: apply.ErrInternal, Message: "boom " + chapSecret}, 500, apply.ErrInternal},
		{"in progress", apply.Response{Result: apply.ResultInProgress}, 409, apply.ResultInProgress},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := iscsiEnv(t, fabricsSample, "reload", nil)
			e.h.answer = func(apply.Request) apply.Response { return c.resp }
			rec := e.call(http.MethodPost, "/api/v1/transports/iscsi/bind", map[string]any{"library": 30, "clear_chap": false,
				"initiators": []string{"iqn.2024-01.com.example:bareos", pbsIQN}})
			if rec.Code != c.status {
				t.Fatalf("status %d, want %d: %s", rec.Code, c.status, rec.Body)
			}
			noSecrets(t, c.name, rec.Body.String())
			got := decodeBody[iscsiWriteDTO](t, rec)
			if got.Error == nil || got.Error.Code != c.code || got.Result != c.resp.Result || got.Diff == "" || got.Target != nil {
				t.Errorf("answer = %+v", got)
			}
			switch c.name {
			case "conflict":
				if got.CurrentSHA256 == nil || *got.CurrentSHA256 != cur || !strings.Contains(got.Error.Message, "read it again") {
					t.Errorf("conflict = %+v", got)
				}
			case "rolled back":
				if got.JournalTail != "chap "+conffile.SecretMask+" rejected" || got.RollbackDir != "/var/lib/x" || got.Action != "reload" {
					t.Errorf("rollback = %+v", got)
				}
			}
		})
	}
}

// After a real bind that reloads, the target is read from configfs; a
// target not there is present false.
func TestISCSIBindTargetState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "target")
	tpg := filepath.Join(root, "iscsi", baseName+".lib10", "tpgt_1")
	for _, d := range []string{"lun/lun_0", "lun/lun_1", "acls/" + pbsIQN, "np/0.0.0.0:3260"} {
		if err := os.MkdirAll(filepath.Join(tpg, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(tpg, "enable"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := iscsiEnv(t, fabricsSample, "reload", func(c *Config) { c.TargetRoot = root })
	rec := e.call(http.MethodPost, "/api/v1/transports/iscsi/bind", map[string]any{"library": 10,
		"initiators": []string{pbsIQN, "iqn.2024-01.com.example:new"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("bind: %d %s", rec.Code, rec.Body)
	}
	got := decodeBody[iscsiWriteDTO](t, rec)
	tr := got.Target
	if tr == nil || tr.WWN != baseName+".lib10" || !tr.Present || tr.Enabled == nil || !*tr.Enabled || tr.LUNs != 2 ||
		!reflect.DeepEqual(tr.ACLs, []string{pbsIQN}) {
		t.Errorf("target = %+v", tr)
	}

	rec = e.call(http.MethodPost, "/api/v1/transports/iscsi/bind", map[string]any{"library": 30, "initiators": []string{pbsIQN}})
	got = decodeBody[iscsiWriteDTO](t, rec)
	if rec.Code != http.StatusOK || got.Target == nil || got.Target.Present || got.Target.WWN != "iqn.2026-11.lan.example:custom30" ||
		got.Target.Enabled != nil || got.Target.ACLs == nil {
		t.Errorf("absent target: %d %+v", rec.Code, got.Target)
	}

	// Deferred (mhvtl.target stopped) and dry runs read nothing.
	deferred := iscsiEnv(t, fabricsSample, apply.FabricsDeferred, func(c *Config) { c.TargetRoot = root })
	rec = deferred.call(http.MethodPost, "/api/v1/transports/iscsi/bind", map[string]any{"library": 10, "initiators": []string{pbsIQN}})
	if got = decodeBody[iscsiWriteDTO](t, rec); got.Target != nil || got.Action != "none" {
		t.Errorf("deferred: %+v", got)
	}
}

// The handlers log no secret.
func TestISCSINoSecretsLogged(t *testing.T) {
	var buf bytes.Buffer
	defer log.SetOutput(log.Writer())
	log.SetOutput(&buf)
	e := iscsiEnv(t, fabricsSample, "reload", nil)
	e.h.answer = func(apply.Request) apply.Response {
		return apply.Response{Result: apply.ResultRolledBack, Error: apply.ErrFabricsApply, Message: chapSecret, JournalTail: mutualSecret}
	}
	const newSecret = "NeverLogThis42"
	e.call(http.MethodPost, "/api/v1/transports/iscsi/bind", map[string]any{"library": 30,
		"chap": map[string]string{"name": "x", "secret": newSecret}})
	e.call(http.MethodPost, "/api/v1/transports/iscsi/bind", map[string]any{"library": 30,
		"chap": map[string]string{"name": "x", "secret": "bad secret"}})
	e.call(http.MethodGet, "/api/v1/transports/iscsi/config", nil)
	e.call(http.MethodPost, "/api/v1/transports/iscsi/unbind", map[string]any{"library": 10})
	noSecrets(t, "log", buf.String(), newSecret, "bad secret")
}

func TestTransportUnsupported(t *testing.T) {
	e := iscsiEnv(t, fabricsSample, "reload", nil)
	for _, r := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/transports/fc/bind"},
		{http.MethodPost, "/api/v1/transports/usb/unbind"},
		{http.MethodPatch, "/api/v1/transports/local"},
	} {
		rec := e.call(r.method, r.path, map[string]any{"library": 10})
		if body := decodeBody[fieldErrBody](t, rec); rec.Code != http.StatusNotFound || body.Error.Code != "unsupported_transport" {
			t.Errorf("%s %s: %d %s", r.method, r.path, rec.Code, rec.Body)
		}
	}
	if n := len(e.fabricsReqs()); n != 0 {
		t.Errorf("%d helper requests", n)
	}
}

// TestISCSIRefusalNoSecretOracle: a refusal quoting request values reads
// the same whether or not one of them is a stored secret, so a client
// cannot test guesses (security review 2026-11-12, finding 2).
func TestISCSIRefusalNoSecretOracle(t *testing.T) {
	e := iscsiEnv(t, fabricsSample, "reload", nil)
	msg := func(guess string) string {
		t.Helper()
		rec := e.call(http.MethodPatch, "/api/v1/transports/iscsi",
			map[string]any{"portals": []string{"letmein " + guess + " qwerty"}, "dry_run": true})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
		return decodeBody[fieldErrBody](t, rec).Error.Message
	}
	wrong, right := msg("password1"), msg(chapSecret)
	if right != strings.ReplaceAll(wrong, "password1", chapSecret) {
		t.Errorf("refusal depends on the guess being a stored secret:\n%q\n%q", wrong, right)
	}
	if len(e.fabricsReqs()) != 0 {
		t.Error("refusal reached the helper")
	}
}

// TestISCSIProblemsNoSecretFragment: a malformed secret line's problem,
// which quotes the field name run into the value, is not returned as is
// (security review 2026-11-12).
func TestISCSIProblemsNoSecretFragment(t *testing.T) {
	bad := strings.Replace(fabricsSample, " CHAP secret: "+chapSecret, " CHAP secret= Pa:ss-word12", 1)
	e := iscsiEnv(t, bad, "reload", nil)
	rec := e.call(http.MethodGet, "/api/v1/transports/iscsi/config", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	got := decodeBody[iscsiConfigDTO](t, rec)
	if len(got.Problems) == 0 {
		t.Fatal("no problem reported for the malformed line")
	}
	for _, s := range []string{"pa:ss", "Pa:ss", "ss-word"} {
		if strings.Contains(rec.Body.String(), s) {
			t.Errorf("GET config holds %q: %s", s, rec.Body)
		}
	}
	// A write against that file is refused with the same message.
	rec = e.call(http.MethodPost, "/api/v1/transports/iscsi/unbind", map[string]any{"library": 30, "dry_run": true})
	if rec.Code != http.StatusUnprocessableEntity || strings.Contains(strings.ToLower(rec.Body.String()), "pa:ss") {
		t.Errorf("refusal %d: %s", rec.Code, rec.Body)
	}
}

// TestISCSIDiffMasksAnySpacing: a secret field spelled with \v between the
// words, which mhvtl-fabrics reads as "CHAP secret", is masked in the
// preview (security review 2026-11-12, finding 1).
func TestISCSIDiffMasksAnySpacing(t *testing.T) {
	odd := strings.Replace(fabricsSample, " CHAP secret: ", " CHAP\vsecret: ", 1)
	e := iscsiEnv(t, odd, "reload", nil)
	rec := e.call(http.MethodPost, "/api/v1/transports/iscsi/unbind", map[string]any{"library": 10, "dry_run": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	noSecrets(t, "unbind preview", rec.Body.String())
}
