package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"mhvtl-control/internal/apply"
	"mhvtl-control/internal/applyclient"
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

// version is the opaque version e's daemon answers for fabrics.conf
// content ("" for no file).
func (e *writeEnv) version(content string) string {
	if content == "" {
		return ""
	}
	return versionNow(e.srv, []byte(content))
}

// versionNow is s's version of fabrics.conf content data under its
// current revision.
func versionNow(s *Server, data []byte) string {
	s.fabricsMu.Lock()
	rev := s.fabricsRev
	s.fabricsMu.Unlock()
	return s.fabricsVersion(rev, data)
}

// str is *p, "<absent>" for nil.
func str(p *string) string {
	if p == nil {
		return "<absent>"
	}
	return *p
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
	empty := ""
	want := iscsiConfigDTO{SHA256: &empty, ISCSI: iscsiRecordDTO{Portals: []string{}}, Targets: []iscsiTargetDTO{},
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
	if !got.Exists || got.SHA256 == nil || *got.SHA256 != e.version(fabricsSample) {
		t.Errorf("exists %v sha %v", got.Exists, got.SHA256)
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

// TestISCSIConfigNoTokenNoCHAPNames: without an API token anyone local
// reads GET config, so it answers whether CHAP is set but not the names.
func TestISCSIConfigNoTokenNoCHAPNames(t *testing.T) {
	e := iscsiEnv(t, fabricsSample, "reload", func(c *Config) { c.Token = "" })
	rec := e.call(http.MethodGet, "/api/v1/transports/iscsi/config", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if b := rec.Body.String(); strings.Contains(b, "chap_name") || strings.Contains(b, `"pbs"`) || strings.Contains(b, `"vtl"`) {
		t.Errorf("CHAP names answered without a token: %s", b)
	}
	if strings.Contains(rec.Body.String(), `"sha256"`) {
		t.Errorf("version answered without a token: %s", rec.Body)
	}
	got := decodeBody[iscsiConfigDTO](t, rec)
	if len(got.Targets) != 2 || !got.Targets[0].CHAP || !got.Targets[0].MutualCHAP || got.Targets[1].CHAP {
		t.Errorf("targets = %+v", got.Targets)
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
	for k, id := range []int{11, 99} {
		if p := got.Problems[k]; p.Kind != "plan" || p.Target == nil || *p.Target != id {
			t.Errorf("problem %d: kind %q target %v, want plan %d", k, p.Kind, p.Target, id)
		}
	}

	// Unbinding a target that does not plan is accepted while another that
	// does not plan stays (the unbind adds no problem; the helper writes the
	// file without applying it), so each can be unbound in turn; binding
	// another while one does not plan is refused.
	rec := e.call(http.MethodPost, "/api/v1/transports/iscsi/unbind", map[string]any{"library": 11, "dry_run": true})
	if rec.Code != http.StatusOK || decodeBody[iscsiWriteDTO](t, rec).Result != apply.ResultValidated {
		t.Errorf("unbind 11 with 99 left: %d %s", rec.Code, rec.Body)
	}
	if reqs := e.fabricsReqs(); len(reqs) != 1 || !bytes.Contains(reqs[0].Files[0].Content, []byte("iSCSI target: 99\n")) ||
		bytes.Contains(reqs[0].Files[0].Content, []byte("iSCSI target: 11\n")) {
		t.Errorf("helper requests = %+v", reqs)
	}
	rec = e.call(http.MethodPost, "/api/v1/transports/iscsi/unbind", map[string]any{"library": 99, "dry_run": true})
	if rec.Code != http.StatusOK || decodeBody[iscsiWriteDTO](t, rec).Result != apply.ResultValidated {
		t.Errorf("unbind 99 with 11 left: %d %s", rec.Code, rec.Body)
	}
	rec = e.call(http.MethodPost, "/api/v1/transports/iscsi/bind", map[string]any{"library": 30, "initiators": []string{pbsIQN}, "dry_run": true})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "no Library or Drive 99") {
		t.Errorf("bind 30 with 11 and 99 broken: %d %s", rec.Code, rec.Body)
	}
	data, err := os.ReadFile(filepath.Join(e.dir, "fabrics.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, "fabrics.conf"), []byte(strings.Replace(string(data), "iSCSI target: 11\n Initiator name: "+pbsIQN+"\n", "", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	rec = e.call(http.MethodPost, "/api/v1/transports/iscsi/unbind", map[string]any{"library": 99})
	if rec.Code != http.StatusOK || decodeBody[iscsiWriteDTO](t, rec).Result != apply.ResultApplied {
		t.Errorf("unbind 99, the target that does not plan: %d %s", rec.Code, rec.Body)
	}

	// A parse problem is reported as it is, without CheckPlan.
	if err := os.WriteFile(filepath.Join(e.dir, "fabrics.conf"), []byte("VERSION: 1\nbogus\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got = decodeBody[iscsiConfigDTO](t, e.call(http.MethodGet, "/api/v1/transports/iscsi/config", nil))
	if len(got.Problems) != 1 || got.Problems[0].Line != 2 || got.Problems[0].Kind != "parse" || got.Problems[0].Target != nil {
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
// refused naming no field (base_name is not a bind field), with a Target
// name it goes to the helper.
func TestISCSIBindNewRecord(t *testing.T) {
	e := iscsiEnv(t, "", "start", nil)
	bind := map[string]any{"library": 10, "initiators": []string{pbsIQN}, "dry_run": true}
	rec := e.call(http.MethodPost, "/api/v1/transports/iscsi/bind", bind)
	if body := decodeBody[fieldErrBody](t, rec); rec.Code != http.StatusUnprocessableEntity ||
		body.Error.Code != apply.ErrInvalid || body.Error.Field != "" {
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
		{"chap without secret", map[string]any{"library": 30, "chap": map[string]string{"name": "a"}}, 422, "chap"},
		{"mutual same secret", map[string]any{"library": 30, "chap": map[string]string{"name": "a", "secret": "Same-secret-01"},
			"mutual_chap": map[string]string{"name": "m", "secret": "Same-secret-01"}}, 422, "mutual_chap"},
		{"mutual only, chap stored", map[string]any{"library": 10, "mutual_chap": map[string]string{"name": "m", "secret": chapSecret}}, 422, "chap"},
		{"chap set and cleared", map[string]any{"library": 10, "clear_chap": true, "chap": map[string]string{"name": "a", "secret": "abcdefghijkl"}}, 422, "chap"},
		{"blank in secret", map[string]any{"library": 30, "chap": map[string]string{"name": "a", "secret": "Leaky Secret9"}}, 422, "chap"},
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
		{"foreign target", apply.Response{Result: apply.ResultRefused, Error: apply.ErrInvalid,
			Message: "iSCSI target iqn.2026-11.lan.example:x already exists in configfs and mhvtl-fabrics does not own it"}, 422, apply.ErrInvalid},
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
				// The helper's real hash is not passed on: the client gets
				// the version of the file as it is.
				if got.CurrentSHA256 == nil || *got.CurrentSHA256 != e.version(fabricsSample) || !strings.Contains(got.Error.Message, "read it again") {
					t.Errorf("conflict = %+v", got)
				}
			case "foreign target":
				if got.Error.Message != c.resp.Message {
					t.Errorf("foreign target message = %q", got.Error.Message)
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
	rec = deferred.call(http.MethodPost, "/api/v1/transports/iscsi/bind", map[string]any{"library": 10, "initiators": []string{pbsIQN, "iqn.2024-01.com.example:new"}})
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

// Every write answers the sha256 it read; base_sha256 other than that is a
// 409 that never reaches the helper, and an equal one is what the helper
// gets.
func TestISCSIBaseSHA256(t *testing.T) {
	e := iscsiEnv(t, fabricsSample, "reload", nil)
	sha := conffile.Hash([]byte(fabricsSample))
	bind := map[string]any{"library": 30, "initiators": []string{pbsIQN}, "dry_run": true}
	got := decodeBody[iscsiWriteDTO](t, e.call(http.MethodPost, "/api/v1/transports/iscsi/bind", bind))
	// A dry run takes the helper's lock, and may recover: it too advances
	// the version, and answers the one after it.
	ver := e.version(fabricsSample)
	if str(got.SHA256) != ver || got.Result != apply.ResultValidated {
		t.Fatalf("dry run = %+v, want sha256 %s", got, ver)
	}

	// The client sends the version back; the helper gets the real hash.
	bind["dry_run"], bind["base_sha256"] = false, ver
	rec := e.call(http.MethodPost, "/api/v1/transports/iscsi/bind", bind)
	if rec.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body)
	}
	if reqs := e.fabricsReqs(); len(reqs) != 2 || reqs[1].Op != apply.OpFabrics || reqs[1].Files[0].BaseSHA256 != sha {
		t.Fatalf("helper requests = %+v", reqs)
	}
	// The apply advanced the version, though this helper left the file
	// as it was (fabricsWritten): the version read before it is stale.
	if now := e.version(fabricsSample); now == ver {
		t.Errorf("version %s unchanged after the apply", ver)
	} else {
		ver = now
	}

	for name, body := range map[string]map[string]any{
		"stale":     {"library": 30, "initiators": []string{pbsIQN}, "base_sha256": str(got.SHA256)},
		"real hash": {"library": 30, "initiators": []string{pbsIQN}, "base_sha256": sha},
		"bind":      {"library": 30, "initiators": []string{pbsIQN}, "base_sha256": strings.Repeat("0", 64)},
		"patch":     {"base_name": "iqn.2026-12.lan.example:x", "base_sha256": "", "dry_run": true},
		"unbind":    {"library": 30, "base_sha256": "stale"},
	} {
		method, path := http.MethodPost, "/api/v1/transports/iscsi/"+name
		if name == "real hash" || name == "stale" {
			path = "/api/v1/transports/iscsi/bind"
		}
		if name == "patch" {
			method, path = http.MethodPatch, "/api/v1/transports/iscsi"
		}
		rec := e.call(method, path, body)
		got := decodeBody[iscsiWriteDTO](t, rec)
		if rec.Code != http.StatusConflict || got.Result != apply.ResultConflict || got.Error == nil || got.Error.Code != apply.ResultConflict ||
			got.CurrentSHA256 == nil || *got.CurrentSHA256 != ver || str(got.SHA256) != ver {
			t.Errorf("%s with a stale base: %d %s", name, rec.Code, rec.Body)
		}
	}
	if n := len(e.fabricsReqs()); n != 2 {
		t.Errorf("%d helper requests, want 2: a conflict reached the helper", n)
	}

	// No file: base_sha256 "" matches, and is what the helper gets.
	absent := iscsiEnv(t, "", "start", nil)
	rec = absent.call(http.MethodPost, "/api/v1/transports/iscsi/bind", map[string]any{"library": 10, "initiators": []string{pbsIQN},
		"target_name": "iqn.2026-11.lan.example:lib10", "base_sha256": ""})
	if got := decodeBody[iscsiWriteDTO](t, rec); rec.Code != http.StatusOK || str(got.SHA256) != "" || !strings.Contains(rec.Body.String(), `"sha256":""`) {
		t.Errorf("no file: %d %s", rec.Code, rec.Body)
	}
	if reqs := absent.fabricsReqs(); len(reqs) != 1 || reqs[0].Files[0].BaseSHA256 != "" {
		t.Errorf("helper requests = %+v", reqs)
	}
}

// An edit that leaves fabrics.conf as it is answers unchanged, dry run or
// not, without the helper.
func TestISCSIUnchanged(t *testing.T) {
	e := iscsiEnv(t, fabricsSample, "reload", nil)
	for _, dry := range []bool{true, false} {
		rec := e.call(http.MethodPost, "/api/v1/transports/iscsi/bind", map[string]any{"library": 10, "initiators": []string{pbsIQN}, "dry_run": dry})
		got := decodeBody[iscsiWriteDTO](t, rec)
		if rec.Code != http.StatusOK || got.Result != "unchanged" || got.Diff != "" || got.DryRun != dry ||
			str(got.SHA256) != e.version(fabricsSample) || got.Error != nil {
			t.Errorf("dry run %v: %d %s", dry, rec.Code, rec.Body)
		}
	}
	rec := e.call(http.MethodPatch, "/api/v1/transports/iscsi", map[string]any{"portals": []string{"192.0.2.10:3260"}})
	if got := decodeBody[iscsiWriteDTO](t, rec); rec.Code != http.StatusOK || got.Result != "unchanged" {
		t.Errorf("patch: %d %s", rec.Code, rec.Body)
	}
	if n := len(e.fabricsReqs()); n != 0 {
		t.Errorf("%d helper requests", n)
	}
}

// An explicit empty initiator list is refused: a record without one
// exports nothing (fabrics.conf(5)).
func TestISCSIBindEmptyInitiators(t *testing.T) {
	e := iscsiEnv(t, fabricsSample, "reload", nil)
	rec := e.call(http.MethodPost, "/api/v1/transports/iscsi/bind", map[string]any{"library": 10, "initiators": []string{}, "dry_run": true})
	body := decodeBody[fieldErrBody](t, rec)
	if rec.Code != http.StatusUnprocessableEntity || body.Error.Field != "initiators" || !strings.Contains(body.Error.Message, "unbind") ||
		!strings.Contains(body.Error.Message, "exports nothing") {
		t.Errorf("empty initiators: %d %s", rec.Code, rec.Body)
	}
	if n := len(e.fabricsReqs()); n != 0 {
		t.Errorf("%d helper requests", n)
	}
}

// A 422's field comes from the message before any quoted value, and a
// CheckPlan problem of another target names no field.
func TestISCSIFieldAttribution(t *testing.T) {
	e := iscsiEnv(t, fabricsSample, "reload", nil)
	for _, c := range []struct {
		name  string
		body  map[string]any
		field string
	}{
		{"chap in a quoted initiator", map[string]any{"library": 10, "initiators": []string{"iqn.2024-01.com.Chapman:pbs"}}, "initiators"},
		{"initiators in a quoted target name", map[string]any{"library": 10, "target_name": "IQN.initiators.x"}, "target_name"},
	} {
		rec := e.call(http.MethodPost, "/api/v1/transports/iscsi/bind", c.body)
		if body := decodeBody[fieldErrBody](t, rec); rec.Code != http.StatusUnprocessableEntity || body.Error.Field != c.field {
			t.Errorf("%s: %d %s, want field %q", c.name, rec.Code, rec.Body, c.field)
		}
	}

	other := iscsiEnv(t, fabricsSample+"\niSCSI target: 99\n Initiator name: "+pbsIQN+"\n", "reload", nil)
	rec := other.call(http.MethodPost, "/api/v1/transports/iscsi/bind", map[string]any{"library": 30, "initiators": []string{pbsIQN}})
	if body := decodeBody[fieldErrBody](t, rec); rec.Code != http.StatusUnprocessableEntity || body.Error.Field != "" ||
		!strings.Contains(body.Error.Message, "no Library or Drive 99") {
		t.Errorf("problem of target 99 on a bind of 30: %d %s", rec.Code, rec.Body)
	}
	if n := len(e.fabricsReqs()) + len(other.fabricsReqs()); n != 0 {
		t.Errorf("%d helper requests", n)
	}
	// An unbind of a healthy target while another is broken would only be
	// written, not applied, and leave the target exported: refused, naming
	// the broken one. The broken target's own unbind goes ahead.
	rec = other.call(http.MethodPost, "/api/v1/transports/iscsi/unbind", map[string]any{"library": 30})
	if body := decodeBody[fieldErrBody](t, rec); rec.Code != http.StatusUnprocessableEntity || body.Error.Field != "library" ||
		!strings.Contains(body.Error.Message, "unbind the broken targets first (99)") || len(other.fabricsReqs()) != 0 {
		t.Errorf("unbind of 30 with target 99 broken: %d %s", rec.Code, rec.Body)
	}
	rec = other.call(http.MethodPost, "/api/v1/transports/iscsi/unbind", map[string]any{"library": 99})
	if rec.Code != http.StatusOK || len(other.fabricsReqs()) != 1 {
		t.Errorf("unbind of the broken 99: %d %s", rec.Code, rec.Body)
	}

	if f := fieldOf("initiator name 'IQN.2024-01.com.Chapman:pbs' is not an iSCSI name", []string{"chap", "initiators"}); f != "initiators" {
		t.Errorf("fieldOf = %q", f)
	}
	if f := fieldOf(`target name "x.initiators" is bad`, []string{"initiators", "target_name"}); f != "target_name" {
		t.Errorf("fieldOf double quote = %q", f)
	}
}

// TestISCSIPreviewNoSecretOracle: a CHAP name equal to another target's
// stored secret shows in the preview as sent, like any other guess
// (architect review 2026-11-12).
func TestISCSIPreviewNoSecretOracle(t *testing.T) {
	e := iscsiEnv(t, fabricsSample, "reload", nil)
	diff := func(name string) string {
		t.Helper()
		rec := e.call(http.MethodPost, "/api/v1/transports/iscsi/bind", map[string]any{"library": 30,
			"chap": map[string]string{"name": name, "secret": "fresh-secret-0123"}, "dry_run": true})
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
		return decodeBody[iscsiWriteDTO](t, rec).Diff
	}
	wrong, right := diff("password1"), diff(chapSecret)
	if right != strings.ReplaceAll(wrong, "password1", chapSecret) {
		t.Errorf("preview depends on the name being a stored secret:\n%q\n%q", wrong, right)
	}
	if strings.Contains(wrong, "fresh-secret-0123") {
		t.Errorf("own secret shown: %q", wrong)
	}
	// Removing target 10 still masks its secrets.
	rec := e.call(http.MethodPost, "/api/v1/transports/iscsi/unbind", map[string]any{"library": 10, "dry_run": true})
	noSecrets(t, "unbind preview", rec.Body.String())
}

// TestISCSIPreviewMasksFileLines: a line of the file as read that shows on
// the added side, between two changed lines, has stored secrets masked
// even outside a secret field, such as a hand-written comment.
func TestISCSIPreviewMasksFileLines(t *testing.T) {
	content := strings.Replace(fabricsSample, " Target name: iqn.2026-11.lan.example:custom30\n",
		"# was: "+chapSecret+"\n Target name: iqn.2026-11.lan.example:custom30\n", 1)
	e := iscsiEnv(t, content, "reload", nil)
	// The initiator (above the comment) and the target name (below it)
	// change, so the comment is inside the diff.
	rec := e.call(http.MethodPost, "/api/v1/transports/iscsi/bind", map[string]any{"library": 30,
		"initiators": []string{"iqn.2024-01.com.example:other"}, "target_name": "iqn.2026-11.lan.example:renamed30",
		"dry_run": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if d := decodeBody[iscsiWriteDTO](t, rec).Diff; !strings.Contains(d, "# was: ") {
		t.Fatalf("comment not in the diff, test does not cover the case: %q", d)
	}
	noSecrets(t, "bind preview", rec.Body.String())
}

// TestISCSISecretGuessNoOracle: a bind that sends a CHAP or mutual CHAP
// secret gets the same answer whether or not it is the stored one, dry run
// or not: no "unchanged", and the same preview. Target 10 has both, so the
// bind sends both (TestISCSISecretPair).
func TestISCSISecretGuessNoOracle(t *testing.T) {
	for _, c := range []struct{ field, name, stored, other, otherName string }{
		{"chap", "pbs", chapSecret, "mutual_chap", "vtl"},
		{"mutual_chap", "vtl", mutualSecret, "chap", "pbs"},
	} {
		for _, dry := range []bool{true, false} {
			// Each guess goes to a daemon of its own, with the same key:
			// a write advances the version (fabricsWritten), so a second
			// guess to the same daemon answers another one either way.
			key := bytes.Repeat([]byte{7}, 32)
			answer := func(secret string) *httptest.ResponseRecorder {
				t.Helper()
				e := iscsiEnv(t, fabricsSample, "reload", nil)
				e.srv.fabricsKey = key
				rec := e.call(http.MethodPost, "/api/v1/transports/iscsi/bind", map[string]any{"library": 10,
					c.field: map[string]string{"name": c.name, "secret": secret},
					c.other: map[string]string{"name": c.otherName, "secret": "Other-fresh-0123"}, "dry_run": dry})
				if n := len(e.fabricsReqs()); n != 1 {
					t.Errorf("%s dry run %v: %d helper requests", c.field, dry, n)
				}
				return rec
			}
			rightRec, wrongRec := answer(c.stored), answer("WrongGuess-0123")
			right, wrong := rightRec.Body.String(), wrongRec.Body.String()
			if rightRec.Code != wrongRec.Code || right != wrong {
				t.Errorf("%s dry run %v: answers differ:\nright %d %s\nwrong %d %s", c.field, dry, rightRec.Code, right, wrongRec.Code, wrong)
			}
			got := decodeBody[iscsiWriteDTO](t, rightRec)
			if rightRec.Code != http.StatusOK || got.Result == "unchanged" || !strings.Contains(got.Diff, "\n-") {
				t.Errorf("%s dry run %v: %d %s", c.field, dry, rightRec.Code, right)
			}
			noSecrets(t, c.field, right+wrong, "WrongGuess-0123", "Other-fresh-0123")
		}
	}
}

// TestISCSISecretGuessVersionAdvances: a bind that sets a CHAP secret
// to a guess, applied by a helper that writes the file, changes the
// version GET /config answers whether or not the guess is the stored
// secret. A right guess leaves the file byte-identical; were the version
// only the file's, it would stay the same exactly then. A base_sha256
// from before the write is refused after it, and the fresh version is
// accepted.
func TestISCSISecretGuessVersionAdvances(t *testing.T) {
	chapOnly := strings.Replace(fabricsSample, " Mutual CHAP name: vtl\n Mutual CHAP secret: "+mutualSecret+"\n", "", 1)
	for _, guess := range []string{chapSecret, "WrongGuess-0123"} {
		e := iscsiEnv(t, chapOnly, "reload", nil)
		e.h.answer = func(req apply.Request) apply.Response {
			if req.Op == apply.OpFabricsPlan {
				return apply.Response{Result: apply.ResultValidated, FabricsAction: "reload"}
			}
			if err := os.WriteFile(filepath.Join(e.dir, "fabrics.conf"), req.Files[0].Content, 0o600); err != nil {
				t.Error(err)
			}
			return apply.Response{Result: apply.ResultApplied, FabricsAction: "reload"}
		}
		version := func() string {
			t.Helper()
			rec := e.call(http.MethodGet, "/api/v1/transports/iscsi/config", nil)
			got := decodeBody[iscsiConfigDTO](t, rec)
			if rec.Code != http.StatusOK || got.SHA256 == nil {
				t.Fatalf("GET config: %d %s", rec.Code, rec.Body)
			}
			return *got.SHA256
		}
		before := version()
		rec := e.call(http.MethodPost, "/api/v1/transports/iscsi/bind", map[string]any{"library": 10,
			"chap": map[string]string{"name": "pbs", "secret": guess}, "base_sha256": before})
		if rec.Code != http.StatusOK || decodeBody[iscsiWriteDTO](t, rec).Result != apply.ResultApplied {
			t.Fatalf("guess %q: %d %s", guess, rec.Code, rec.Body)
		}
		data, err := os.ReadFile(filepath.Join(e.dir, "fabrics.conf"))
		if err != nil {
			t.Fatal(err)
		}
		if identical := string(data) == chapOnly; identical != (guess == chapSecret) {
			t.Fatalf("guess %q: file identical %v, test does not cover the case", guess, identical)
		}
		after := version()
		if after == before {
			t.Errorf("guess %q: version unchanged after the write", guess)
		}
		rec = e.call(http.MethodPost, "/api/v1/transports/iscsi/bind", map[string]any{"library": 10,
			"initiators": []string{pbsIQN}, "base_sha256": before, "dry_run": true})
		if rec.Code != http.StatusConflict {
			t.Errorf("guess %q: base from before the write: %d %s", guess, rec.Code, rec.Body)
		}
		rec = e.call(http.MethodPost, "/api/v1/transports/iscsi/bind", map[string]any{"library": 10,
			"initiators": []string{pbsIQN}, "base_sha256": after, "dry_run": true})
		if rec.Code != http.StatusOK {
			t.Errorf("guess %q: fresh base: %d %s", guess, rec.Code, rec.Body)
		}
	}
}

// TestISCSISecretPair: a bind that sets one CHAP secret of a target whose
// other-direction secret is stored, without sending that one, is refused
// whatever the value, so the "must differ" rule of fabrics.conf(5) never
// compares a sent secret with a stored one: a guess equal to the stored
// secret and a wrong one get byte-identical answers, dry run or not.
// Clearing mutual CHAP still works, and so does a CHAP change where no
// mutual CHAP is stored.
func TestISCSISecretPair(t *testing.T) {
	chapOnly := strings.Replace(fabricsSample, " Mutual CHAP name: vtl\n Mutual CHAP secret: "+mutualSecret+"\n", "", 1)
	for _, c := range []struct {
		name, content, field, secretName, stored, missing string
	}{
		{"chap with mutual stored", fabricsSample, "chap", "pbs", mutualSecret, "mutual_chap"},
		{"mutual with chap stored", fabricsSample, "mutual_chap", "vtl", chapSecret, "chap"},
		{"mutual added to chap", chapOnly, "mutual_chap", "vtl", chapSecret, "chap"},
	} {
		for _, dry := range []bool{true, false} {
			e := iscsiEnv(t, c.content, "reload", nil)
			answer := func(secret string) *httptest.ResponseRecorder {
				t.Helper()
				return e.call(http.MethodPost, "/api/v1/transports/iscsi/bind", map[string]any{"library": 10,
					c.field: map[string]string{"name": c.secretName, "secret": secret}, "dry_run": dry})
			}
			rightRec, wrongRec := answer(c.stored), answer("WrongGuess-0123")
			right, wrong := rightRec.Body.String(), wrongRec.Body.String()
			if rightRec.Code != http.StatusUnprocessableEntity || rightRec.Code != wrongRec.Code || right != wrong {
				t.Errorf("%s dry run %v: answers differ or not 422:\nright %d %s\nwrong %d %s", c.name, dry, rightRec.Code, right, wrongRec.Code, wrong)
			}
			if got := decodeBody[fieldErrBody](t, rightRec).Error; got.Field != c.missing || !strings.Contains(got.Message, "send both CHAP secrets") {
				t.Errorf("%s: error %+v", c.name, got)
			}
			if n := len(e.fabricsReqs()); n != 0 {
				t.Errorf("%s: %d helper requests", c.name, n)
			}
			noSecrets(t, c.name, right, "WrongGuess-0123")
		}
	}

	e := iscsiEnv(t, fabricsSample, "reload", nil)
	rec := e.call(http.MethodPost, "/api/v1/transports/iscsi/bind", map[string]any{"library": 10,
		"chap": map[string]string{"name": "pbs", "secret": "New-chap-0123"}, "clear_mutual_chap": true, "dry_run": true})
	if d := decodeBody[iscsiWriteDTO](t, rec).Diff; rec.Code != http.StatusOK || !strings.Contains(d, "-  Mutual CHAP name: vtl") && !strings.Contains(d, "- Mutual CHAP name: vtl") {
		t.Errorf("chap with clear_mutual_chap: %d %s", rec.Code, rec.Body)
	}
	rec = e.call(http.MethodPost, "/api/v1/transports/iscsi/bind", map[string]any{"library": 10, "clear_mutual_chap": true, "dry_run": true})
	if rec.Code != http.StatusOK {
		t.Errorf("clear_mutual_chap alone: %d %s", rec.Code, rec.Body)
	}
	e = iscsiEnv(t, chapOnly, "reload", nil)
	rec = e.call(http.MethodPost, "/api/v1/transports/iscsi/bind", map[string]any{"library": 10,
		"chap": map[string]string{"name": "pbs", "secret": "New-chap-0123"}, "dry_run": true})
	if rec.Code != http.StatusOK {
		t.Errorf("chap alone without mutual stored: %d %s", rec.Code, rec.Body)
	}
}

// TestISCSIBindTargetStateEdited: the target state is read by the name the
// applied file gives: a new target's derived name, a renamed target's new
// name.
func TestISCSIBindTargetStateEdited(t *testing.T) {
	const renamed = "iqn.2026-11.lan.example:renamed10"
	root := filepath.Join(t.TempDir(), "target")
	for _, wwn := range []string{baseName + ".lib30", renamed} {
		if err := os.MkdirAll(filepath.Join(root, "iscsi", wwn, "tpgt_1", "lun", "lun_0"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	no30 := fabricsSample[:strings.Index(fabricsSample, "\niSCSI target: 30")+1]
	e := iscsiEnv(t, no30, "reload", func(c *Config) { c.TargetRoot = root })
	rec := e.call(http.MethodPost, "/api/v1/transports/iscsi/bind", map[string]any{"library": 30, "initiators": []string{pbsIQN}})
	if tr := decodeBody[iscsiWriteDTO](t, rec).Target; rec.Code != http.StatusOK || tr == nil || tr.WWN != baseName+".lib30" || !tr.Present || tr.LUNs != 1 {
		t.Errorf("new target: %d %s", rec.Code, rec.Body)
	}
	rec = e.call(http.MethodPost, "/api/v1/transports/iscsi/bind", map[string]any{"library": 10, "target_name": renamed})
	if tr := decodeBody[iscsiWriteDTO](t, rec).Target; rec.Code != http.StatusOK || tr == nil || tr.WWN != renamed || !tr.Present {
		t.Errorf("renamed target: %d %s", rec.Code, rec.Body)
	}
}

// TestISCSIPreviewLeadingSpaceSecret: a secret line indented " \v" is
// masked in the unbind preview (Codex re-review 2026-11-13).
func TestISCSIPreviewLeadingSpaceSecret(t *testing.T) {
	odd := strings.Replace(fabricsSample, " CHAP secret: ", " \vCHAP secret: ", 1)
	e := iscsiEnv(t, odd, "reload", nil)
	rec := e.call(http.MethodPost, "/api/v1/transports/iscsi/unbind", map[string]any{"library": 10, "dry_run": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	noSecrets(t, "unbind preview", rec.Body.String())
}

// TestISCSIPreviewStoredCHAPNameNoOracle: a CHAP name an earlier apply set
// to a guess shows in a later preview's removed line as it is, whether or
// not the guess is a stored secret (review 2026-11-13: masking it would
// answer the guess).
func TestISCSIPreviewStoredCHAPNameNoOracle(t *testing.T) {
	diff := func(guess string) string {
		t.Helper()
		applied := strings.Replace(fabricsSample, " Target name: iqn.2026-11.lan.example:custom30\n",
			" Target name: iqn.2026-11.lan.example:custom30\n CHAP name: x"+guess+"y\n CHAP secret: Other-secret-0123\n", 1)
		e := iscsiEnv(t, applied, "reload", nil)
		rec := e.call(http.MethodPost, "/api/v1/transports/iscsi/bind", map[string]any{"library": 30, "clear_chap": true, "dry_run": true})
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
		d := decodeBody[iscsiWriteDTO](t, rec).Diff
		if !strings.Contains(d, "\n- CHAP name: x") {
			t.Fatalf("CHAP name line not removed, test does not cover the case: %q", d)
		}
		return d
	}
	wrong, right := diff("password1"), diff(chapSecret)
	if right != strings.ReplaceAll(wrong, "password1", chapSecret) {
		t.Errorf("preview depends on the stored CHAP name holding a secret:\n%q\n%q", wrong, right)
	}
	if strings.Contains(wrong, "Other-secret-0123") {
		t.Errorf("secret shown: %q", wrong)
	}
}

// TestISCSIPreviewTrailingCommentMasked: a line rewriteValue changes keeps
// its trailing comment from the file; a stored secret there is masked on
// the added line as on the removed one.
func TestISCSIPreviewTrailingCommentMasked(t *testing.T) {
	content := strings.Replace(fabricsSample, " Target name: iqn.2026-11.lan.example:custom30\n",
		" Target name: iqn.2026-11.lan.example:custom30 # was "+chapSecret+"\n", 1)
	e := iscsiEnv(t, content, "reload", nil)
	rec := e.call(http.MethodPost, "/api/v1/transports/iscsi/bind", map[string]any{"library": 30,
		"target_name": "iqn.2026-11.lan.example:renamed30", "dry_run": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	d := decodeBody[iscsiWriteDTO](t, rec).Diff
	if !strings.Contains(d, "\n+ Target name: iqn.2026-11.lan.example:renamed30 # was "+conffile.SecretMask+"\n") {
		t.Errorf("added line not masked as expected: %q", d)
	}
	noSecrets(t, "bind preview", rec.Body.String())
}

// TestISCSIProblemSecretName: a malformed secret line that runs the secret
// into the field name shows in GET config under the canonical name.
func TestISCSIProblemSecretName(t *testing.T) {
	for _, line := range []string{" CHAP secret hunter2-Pw:", " CHAP\vsecret hunter2-Pw:"} {
		bad := strings.Replace(fabricsSample, " CHAP secret: "+chapSecret, line, 1)
		e := iscsiEnv(t, bad, "reload", nil)
		rec := e.call(http.MethodGet, "/api/v1/transports/iscsi/config", nil)
		got := decodeBody[iscsiConfigDTO](t, rec)
		if len(got.Problems) == 0 || strings.Contains(rec.Body.String(), "hunter2") ||
			!strings.Contains(got.Problems[0].Message, "chap secret has no value") {
			t.Errorf("%q: %s", line, rec.Body)
		}
	}
}

// TestISCSIMessageCHAPNameNoOracle: the helper's journal tail quotes the
// CHAP name the request sets and the stored secret. The tail answered is
// byte-identical whether the name holds the stored secret, equals it, or
// neither: the new name is masked whole, the stored secret everywhere. The
// helper's message, which quotes outside output only masked that way,
// shows a new name as sent.
func TestISCSIMessageCHAPNameNoOracle(t *testing.T) {
	answer := func(name string) iscsiWriteDTO {
		t.Helper()
		e := iscsiEnv(t, fabricsSample, "reload", nil)
		e.srv.fabricsKey = []byte("one key for both answers")
		e.h.answer = func(apply.Request) apply.Response {
			return apply.Response{Result: apply.ResultRolledBack, Error: apply.ErrFabricsApply, FabricsAction: "reload",
				Message:     "reload failed for CHAP name " + name,
				JournalTail: "login of " + name + " refused; stored pbs secret " + chapSecret}
		}
		rec := e.call(http.MethodPost, "/api/v1/transports/iscsi/bind", map[string]any{"library": 30,
			"chap": map[string]string{"name": name, "secret": "Fresh-secret-0123"}})
		noSecrets(t, name, strings.ReplaceAll(rec.Body.String(), name, ""), "Fresh-secret-0123")
		return decodeBody[iscsiWriteDTO](t, rec)
	}
	for _, shape := range []string{"x%sy", "%s"} {
		wrongName, rightName := strings.Replace(shape, "%s", "password1", 1), strings.Replace(shape, "%s", chapSecret, 1)
		wrong, right := answer(wrongName), answer(rightName)
		if wrong.JournalTail != right.JournalTail || wrong.JournalTail != "login of "+conffile.SecretMask+" refused; stored pbs secret "+conffile.SecretMask {
			t.Errorf("%q: journal tails %q and %q", shape, wrong.JournalTail, right.JournalTail)
		}
		if right.Message != strings.ReplaceAll(wrong.Message, wrongName, rightName) {
			t.Errorf("%q: messages %q and %q", shape, wrong.Message, right.Message)
		}
	}
}

// TestISCSIVersionNotFileHash: the sha256 the routes answer is not the
// SHA-256 of fabrics.conf, which holds CHAP secrets and would let a client
// test guesses at them offline, and a daemon restart (a new key) changes
// it, so an apply against the old one is a conflict.
func TestISCSIVersionNotFileHash(t *testing.T) {
	e := iscsiEnv(t, fabricsSample, "reload", nil)
	sha := conffile.Hash([]byte(fabricsSample))
	got := decodeBody[iscsiWriteDTO](t, e.call(http.MethodPost, "/api/v1/transports/iscsi/unbind", map[string]any{"library": 30, "dry_run": true}))
	cfg := decodeBody[iscsiConfigDTO](t, e.call(http.MethodGet, "/api/v1/transports/iscsi/config", nil))
	if cfg.SHA256 == nil || *cfg.SHA256 == "" || *cfg.SHA256 != str(got.SHA256) {
		t.Fatalf("GET sha256 %v, write sha256 %q: want the same non-empty version", cfg.SHA256, str(got.SHA256))
	}
	if *cfg.SHA256 == sha || str(got.SHA256) == sha {
		t.Errorf("sha256(fabrics.conf) answered: GET %s, write %s", *cfg.SHA256, str(got.SHA256))
	}
	restarted := NewServer(e.srv.cfg)
	if v := versionNow(restarted, []byte(fabricsSample)); v == str(got.SHA256) {
		t.Error("the version did not change with a new daemon")
	}
}

// TestISCSIProblemCHAPNameNoToken: a CHAP name run into the field name
// ("CHAP name pbs-x: ..." is an unknown field) shows in the problem's
// message; without an API token, when GET config leaves CHAP names out, so
// does the message.
func TestISCSIProblemCHAPNameNoToken(t *testing.T) {
	bad := strings.Replace(fabricsSample, " CHAP name: pbs\n", " CHAP name pbsuser7: x\n", 1)
	for _, token := range []string{"s3cret", ""} {
		e := iscsiEnv(t, bad, "reload", func(c *Config) { c.Token = token })
		rec := e.call(http.MethodGet, "/api/v1/transports/iscsi/config", nil)
		got := decodeBody[iscsiConfigDTO](t, rec)
		if len(got.Problems) == 0 {
			t.Fatalf("token %q: no problem for the line: %s", token, rec.Body)
		}
		if shown := strings.Contains(rec.Body.String(), "pbsuser7"); shown != (token != "") {
			t.Errorf("token %q: CHAP name shown %v: %s", token, shown, rec.Body)
		}
	}
}

// TestISCSIHelperHashNotPassedOn: a helper conflict, built as the helper's
// responseOf builds it, quotes the real SHA-256 of fabrics.conf in
// current_sha256 and in its message; neither reaches the client, nor does
// a hash in any other helper text the routes pass on.
func TestISCSIHelperHashNotPassedOn(t *testing.T) {
	sha := conffile.Hash([]byte(fabricsSample))
	path := "/etc/mhvtl/fabrics.conf"
	conflict := func() apply.Response {
		cur := sha
		return apply.Response{Result: apply.ResultConflict, File: path, CurrentSHA256: &cur,
			Message: (&conffile.ConflictError{Path: path, Current: sha}).Error()}
	}
	for name, resp := range map[string]apply.Response{
		"conflict": conflict(),
		"rolled back": {Result: apply.ResultRolledBack, Error: apply.ErrFabricsApply, FabricsAction: "reload",
			Message: "restoring: " + path + " now sha256 " + sha, JournalTail: "apply read " + strings.ToUpper(sha)},
		"error": {Result: apply.ResultError, Error: apply.ErrInternal, Message: "recovery: " + sha},
	} {
		for _, dry := range []bool{true, false} {
			e := iscsiEnv(t, fabricsSample, "reload", nil)
			e.h.answer = func(apply.Request) apply.Response { return resp }
			rec := e.call(http.MethodPost, "/api/v1/transports/iscsi/unbind", map[string]any{"library": 30, "dry_run": dry})
			if body := strings.ToLower(rec.Body.String()); strings.Contains(body, sha) {
				t.Errorf("%s (dry run %v): the file's SHA-256 is in the answer: %s", name, dry, rec.Body)
			}
			if name == "conflict" {
				got := decodeBody[iscsiWriteDTO](t, rec)
				if rec.Code != http.StatusConflict || got.Message != fabricsChanged || got.CurrentSHA256 == nil || *got.CurrentSHA256 != e.version(fabricsSample) {
					t.Errorf("conflict answer = %d %+v", rec.Code, got)
				}
			}
		}
	}
}

// TestISCSIDerivedNameNoOracle: a stored secret that spans a Base name the
// request brings and the ".lib<ID>" a derived target name adds is masked,
// or not, in a helper message naming that target whether or not the Base
// name is the guess.
func TestISCSIDerivedNameNoOracle(t *testing.T) {
	const secret = "le:pick.lib10"
	stored := strings.Replace(fabricsSample, chapSecret, secret, 1)
	answer := func(base string) string {
		t.Helper()
		e := iscsiEnv(t, stored, "reload", nil)
		e.srv.fabricsKey = []byte("one key for both answers")
		e.h.answer = func(apply.Request) apply.Response {
			return apply.Response{Result: apply.ResultRefused, Error: apply.ErrInvalid,
				Message: "iSCSI target " + base + ".lib10 already exists in configfs and mhvtl-fabrics does not own it"}
		}
		rec := e.call(http.MethodPatch, "/api/v1/transports/iscsi", map[string]any{"base_name": base, "dry_run": true})
		got := decodeBody[iscsiWriteDTO](t, rec)
		if got.Error == nil {
			t.Fatalf("%s: %s", base, rec.Body)
		}
		return got.Error.Message
	}
	wrong, right := answer("iqn.2026-11.lan.example:pock"), answer("iqn.2026-11.lan.example:pick")
	if right != strings.ReplaceAll(wrong, "pock", "pick") {
		t.Errorf("messages %q and %q", wrong, right)
	}
}

// TestISCSIVersionHiddenWhileApplying: the helper writes fabrics.conf and
// then reloads mhvtl-fabrics. Until it answers, no route answers a version
// of the file: one of the new file under the old revision would equal the
// version before the write exactly when a CHAP secret guess left the file
// as it was. GET config, a dry run, a write with the old base and a
// conflict's current version answer the same for a right and a wrong
// guess, with no version; after the write the version has changed.
func TestISCSIVersionHiddenWhileApplying(t *testing.T) {
	type seen struct{ get, dry, based, write, cur string }
	run := func(guess string) (seen, string, string) {
		t.Helper()
		e := iscsiEnv(t, fabricsSample, "reload", nil)
		e.srv.fabricsKey = []byte("one key for both answers")
		pre := e.version(fabricsSample)
		raw := func(method, path string, body any) string {
			b, _ := json.Marshal(body)
			r := httptest.NewRequest(method, path, strings.NewReader(string(b)))
			if body == nil {
				r = httptest.NewRequest(method, path, nil)
			}
			r.Header.Set("Authorization", "Bearer "+e.srv.cfg.Token)
			rec := httptest.NewRecorder()
			e.handler.ServeHTTP(rec, r)
			return fmt.Sprintf("%d %s", rec.Code, rec.Body)
		}
		var during seen
		var werr error
		e.answer(func(req apply.Request) apply.Response {
			if req.Op != apply.OpFabrics {
				return apply.Response{Result: apply.ResultValidated, FabricsAction: "reload"}
			}
			if werr = os.WriteFile(filepath.Join(e.dir, "fabrics.conf"), req.Files[0].Content, 0o600); werr != nil {
				return apply.Response{Result: apply.ResultApplied, FabricsAction: "reload"}
			}
			during.get = raw(http.MethodGet, "/api/v1/transports/iscsi/config", nil)
			during.dry = raw(http.MethodPost, "/api/v1/transports/iscsi/unbind", map[string]any{"library": 30, "dry_run": true})
			during.based = raw(http.MethodPost, "/api/v1/transports/iscsi/unbind", map[string]any{"library": 30, "dry_run": true, "base_sha256": pre})
			during.write = raw(http.MethodPost, "/api/v1/transports/iscsi/unbind", map[string]any{"library": 30})
			during.cur = str(e.srv.currentFabricsVersion())
			return apply.Response{Result: apply.ResultApplied, FabricsAction: "reload"}
		})
		rec := e.call(http.MethodPost, "/api/v1/transports/iscsi/bind", map[string]any{"library": 10, "initiators": []string{pbsIQN},
			"chap": map[string]string{"name": "pbs", "secret": guess}, "mutual_chap": map[string]string{"name": "vtl", "secret": mutualSecret}})
		if werr != nil || rec.Code != http.StatusOK {
			t.Fatalf("guess %q: %v %d %s", guess, werr, rec.Code, rec.Body)
		}
		after := decodeBody[iscsiConfigDTO](t, e.call(http.MethodGet, "/api/v1/transports/iscsi/config", nil))
		return during, pre, str(after.SHA256)
	}
	right, pre, afterRight := run(chapSecret)
	wrong, _, afterWrong := run("Wrong-guess-0123")
	if right != wrong {
		t.Errorf("answers during the write differ:\nright %+v\nwrong %+v", right, wrong)
	}
	for _, s := range []string{right.get, right.dry, right.based, right.write} {
		if strings.Contains(s, `"sha256"`) || strings.Contains(s, pre) {
			t.Errorf("a version was answered during the write: %s", s)
		}
	}
	if !strings.HasPrefix(right.get, "200 ") {
		t.Errorf("GET config during the write: %s", right.get)
	}
	for _, s := range []string{right.dry, right.based, right.write} {
		if !strings.HasPrefix(s, "409 ") || !strings.Contains(s, `"result":"`+apply.ResultInProgress+`"`) {
			t.Errorf("write request during the write: %s, want 409 %s", s, apply.ResultInProgress)
		}
	}
	if right.cur != "<absent>" {
		t.Errorf("current version during the write = %s, want none", right.cur)
	}
	if afterRight == "<absent>" || afterRight == pre || afterWrong == "<absent>" || afterWrong == pre {
		t.Errorf("version after the write: right %s, wrong %s, before %s", afterRight, afterWrong, pre)
	}
}

// failingHelper is a helper whose Do of a write fails with err.
type failingHelper struct {
	HelperService
	err error
}

func (h failingHelper) Do(ctx context.Context, req apply.Request) (apply.Response, error) {
	if req.Op == apply.OpFabrics {
		return apply.Response{}, h.err
	}
	return h.HelperService.Do(ctx, req)
}

// failDo makes e's helper fail a write with err.
func failDo(err error) func(*writeEnv) {
	return func(e *writeEnv) { e.srv.cfg.Helper = failingHelper{e.srv.cfg.Helper, err} }
}

// TestISCSIVersionAfterUnreachedWrite: a write that never reached the
// helper leaves fabrics.conf's version as it was; one that reached it
// advances it, whatever the answer.
func TestISCSIVersionAfterUnreachedWrite(t *testing.T) {
	bind := map[string]any{"library": 30, "initiators": []string{pbsIQN}}
	for _, c := range []struct {
		name    string
		helper  func(*writeEnv)
		status  int
		advance bool
	}{
		{"absent", failDo(fmt.Errorf("%w: gone", applyclient.ErrAbsent)), http.StatusConflict, false},
		{"connect failed", failDo(&net.OpError{Op: "dial", Net: "unix", Err: syscall.EACCES}), http.StatusBadGateway, false},
		{"connection lost", failDo(&net.OpError{Op: "read", Net: "unix", Err: syscall.ECONNRESET}), http.StatusBadGateway, true},
		{"refused", func(e *writeEnv) {
			e.answer(func(apply.Request) apply.Response {
				return apply.Response{Result: apply.ResultRefused, Error: apply.ErrInvalid, Message: "no"}
			})
		}, http.StatusUnprocessableEntity, true},
		{"not json", func(e *writeEnv) {
			e.answer(func(apply.Request) apply.Response { return apply.Response{Result: notJSON} })
		}, http.StatusBadGateway, true},
	} {
		e := iscsiEnv(t, fabricsSample, "reload", nil)
		before := e.version(fabricsSample)
		c.helper(e)
		rec := e.call(http.MethodPost, "/api/v1/transports/iscsi/bind", bind)
		if rec.Code != c.status {
			t.Errorf("%s: %d %s, want %d", c.name, rec.Code, rec.Body, c.status)
		}
		after := decodeBody[iscsiConfigDTO](t, e.call(http.MethodGet, "/api/v1/transports/iscsi/config", nil))
		if changed := str(after.SHA256) != before; changed != c.advance || after.SHA256 == nil {
			t.Errorf("%s: version %s after, %s before: want changed %v", c.name, str(after.SHA256), before, c.advance)
		}
	}
}

// helperLikeFabrics makes e's helper check a fabrics request's base
// against the file as the real one does (apply.Helper.fabrics): conflict
// unless it is the file's SHA-256 now; a plan is then validated and an
// apply writes the file.
func helperLikeFabrics(e *writeEnv) {
	e.answer(func(req apply.Request) apply.Response {
		if req.Op != apply.OpFabrics && req.Op != apply.OpFabricsPlan {
			return apply.Response{Result: apply.ResultValidated}
		}
		p := filepath.Join(e.dir, "fabrics.conf")
		cur, err := os.ReadFile(p)
		if err != nil || conffile.Hash(cur) != req.Files[0].BaseSHA256 {
			return apply.Response{Result: apply.ResultConflict, File: "fabrics.conf", Message: "conflict"}
		}
		if req.Op == apply.OpFabricsPlan {
			return apply.Response{Result: apply.ResultValidated, FabricsAction: "reload"}
		}
		if err := os.WriteFile(p, req.Files[0].Content, 0o600); err != nil {
			return apply.Response{Result: apply.ResultError, Error: "io", Message: err.Error()}
		}
		return apply.Response{Result: apply.ResultApplied, FabricsAction: "reload"}
	})
}

// chapOnlySample is fabricsSample without mutual CHAP, so a bind can set
// the CHAP secret alone.
var chapOnlySample = strings.Replace(fabricsSample, " Mutual CHAP name: vtl\n Mutual CHAP secret: "+mutualSecret+"\n", "", 1)

// guessBind is a bind that sets library 10's CHAP secret to guess.
func guessBind(guess string) map[string]any {
	return map[string]any{"library": 10, "chap": map[string]string{"name": "pbs", "secret": guess}}
}

func recText(rec *httptest.ResponseRecorder) string {
	return fmt.Sprintf("%d %s", rec.Code, rec.Body)
}

// TestISCSIDryRunRacingWriteNoOracle: a write that sets a CHAP secret to
// a guess and runs between a dry run's read and its helper check leaves
// the file byte-identical exactly when the guess is right. The helper
// would then validate the dry run's base for a right guess and find a
// conflict for a wrong one; the dry run is answered the same either way.
func TestISCSIDryRunRacingWriteNoOracle(t *testing.T) {
	run := func(guess string) string {
		t.Helper()
		e := iscsiEnv(t, chapOnlySample, "reload", nil)
		e.srv.fabricsKey = []byte("one key for both answers")
		helperLikeFabrics(e)
		var wrote string
		e.srv.beforeFabricsHelper = func() {
			e.srv.beforeFabricsHelper = nil
			wrote = recText(e.call(http.MethodPost, "/api/v1/transports/iscsi/bind", guessBind(guess)))
		}
		got := recText(e.call(http.MethodPost, "/api/v1/transports/iscsi/unbind", map[string]any{"library": 30, "dry_run": true}))
		if !strings.HasPrefix(wrote, "200 ") || !strings.Contains(wrote, `"result":"applied"`) {
			t.Fatalf("guess %q: the racing write was not applied: %s", guess, wrote)
		}
		return got
	}
	right, wrong := run(chapSecret), run("WrongGuess-0123")
	if right != wrong {
		t.Errorf("dry run answers differ:\nright %s\nwrong %s", right, wrong)
	}
	if !strings.HasPrefix(right, "409 ") || !strings.Contains(right, `"result":"`+apply.ResultInProgress+`"`) ||
		strings.Contains(right, `"sha256"`) || !strings.Contains(right, `"diff":""`) {
		t.Errorf("dry run answer = %s, want a 409 %s without version or diff", right, apply.ResultInProgress)
	}
}

// TestISCSIDryRunOverlappedAnswersBusy: a dry run whose helper call
// another one overlapped, a second dry run or a stage's plan, is answered
// apply_in_progress ("preview again"), not conflict ("changed since it
// was read"): nothing changed the file. The call that overlapped it, alone
// in the helper, is answered as usual.
func TestISCSIDryRunOverlappedAnswersBusy(t *testing.T) {
	for _, other := range []string{"iscsi", "config"} {
		e := iscsiEnv(t, fabricsSample, "reload", nil)
		helperLikeFabrics(e)
		path, body := "/api/v1/transports/iscsi/unbind", map[string]any{"library": 10, "dry_run": true}
		if other == "config" {
			path, body = "/api/v1/config/apply", map[string]any{"stage": e.staged().Stage, "dry_run": true}
		}
		var second string
		e.srv.beforeFabricsHelper = func() {
			e.srv.beforeFabricsHelper = nil
			second = e.rawCall(http.MethodPost, path, body)
		}
		first := recText(e.call(http.MethodPost, "/api/v1/transports/iscsi/unbind", map[string]any{"library": 30, "dry_run": true}))
		if !strings.HasPrefix(first, "409 ") || !strings.Contains(first, `"result":"`+apply.ResultInProgress+`"`) || strings.Contains(first, `"sha256"`) {
			t.Errorf("%s: overlapped dry run = %s, want 409 %s without version", other, first, apply.ResultInProgress)
		}
		if !strings.HasPrefix(second, "200 ") || !strings.Contains(second, `"result":"validated"`) {
			t.Errorf("%s: the other call = %s, want validated", other, second)
		}
	}
}

// TestISCSIWriteRacingWriteNoOracle: a write W2 that sets a CHAP secret to
// a guess and comes after write W1 read the file, before W1 reached the
// helper, is answered apply_in_progress at once and never reaches it:
// otherwise W1's stale base would be validated exactly when W2 left the
// file byte-identical. Both answers are the same for a right and a wrong
// guess.
func TestISCSIWriteRacingWriteNoOracle(t *testing.T) {
	run := func(guess string) (string, string) {
		t.Helper()
		e := iscsiEnv(t, chapOnlySample, "reload", nil)
		e.srv.fabricsKey = []byte("one key for both answers")
		helperLikeFabrics(e)
		var w2 string
		e.srv.beforeFabricsHelper = func() {
			e.srv.beforeFabricsHelper = nil
			w2 = recText(e.call(http.MethodPost, "/api/v1/transports/iscsi/bind", guessBind(guess)))
		}
		w1 := recText(e.call(http.MethodPost, "/api/v1/transports/iscsi/unbind", map[string]any{"library": 30}))
		writes := 0
		for _, r := range e.fabricsReqs() {
			if r.Op == apply.OpFabrics {
				writes++
			}
		}
		if writes != 1 {
			t.Errorf("guess %q: %d writes reached the helper, want W1's only", guess, writes)
		}
		return w1, w2
	}
	w1Right, w2Right := run(chapSecret)
	w1Wrong, w2Wrong := run("WrongGuess-0123")
	if w1Right != w1Wrong || w2Right != w2Wrong {
		t.Errorf("answers differ:\nW1 right %s\nW1 wrong %s\nW2 right %s\nW2 wrong %s", w1Right, w1Wrong, w2Right, w2Wrong)
	}
	if !strings.HasPrefix(w2Right, "409 ") || !strings.Contains(w2Right, `"result":"`+apply.ResultInProgress+`"`) {
		t.Errorf("W2 = %s, want 409 %s", w2Right, apply.ResultInProgress)
	}
	if !strings.HasPrefix(w1Right, "200 ") {
		t.Errorf("W1 = %s, want applied", w1Right)
	}
}

// TestISCSIWriteAnswersVersionAfter: a write's answer holds the version
// of the file after the helper answered, the one GET config answers next
// and a following apply takes as base, not the one it was made against.
// A dry run's answer holds the same version as GET config. When another
// call that may change the file reached the helper meanwhile, no version
// is answered.
func TestISCSIWriteAnswersVersionAfter(t *testing.T) {
	e := iscsiEnv(t, fabricsSample, "reload", nil)
	helperLikeFabrics(e)
	get := func() string {
		t.Helper()
		return str(decodeBody[iscsiConfigDTO](t, e.call(http.MethodGet, "/api/v1/transports/iscsi/config", nil)).SHA256)
	}
	pre := get()
	rec := e.call(http.MethodPost, "/api/v1/transports/iscsi/unbind", map[string]any{"library": 30, "dry_run": true, "base_sha256": pre})
	if dry := str(decodeBody[iscsiWriteDTO](t, rec).SHA256); rec.Code != http.StatusOK || dry != get() {
		t.Fatalf("dry run: %d %s, GET answers %s", rec.Code, rec.Body, get())
	}
	rec = e.call(http.MethodPost, "/api/v1/transports/iscsi/unbind", map[string]any{"library": 30, "base_sha256": get()})
	applied := str(decodeBody[iscsiWriteDTO](t, rec).SHA256)
	if rec.Code != http.StatusOK || applied == "<absent>" || applied == pre || applied != get() {
		t.Fatalf("apply: %d %s; before %s, GET after %s", rec.Code, rec.Body, pre, get())
	}
	rec = e.call(http.MethodPatch, "/api/v1/transports/iscsi", map[string]any{"base_name": baseName + "x", "base_sha256": applied})
	if rec.Code != http.StatusOK {
		t.Fatalf("apply on the answered version: %d %s", rec.Code, rec.Body)
	}

	// Another call that may change the file reaches the helper while the
	// write is in it.
	inner := e.h.answer
	e.answer(func(req apply.Request) apply.Response {
		e.srv.fabricsMu.Lock()
		e.srv.fabricsSeq += 2
		e.srv.fabricsRev++
		e.srv.fabricsMu.Unlock()
		return inner(req)
	})
	rec = e.call(http.MethodPost, "/api/v1/transports/iscsi/bind", map[string]any{"library": 30, "initiators": []string{pbsIQN}})
	if got := decodeBody[iscsiWriteDTO](t, rec); got.SHA256 != nil || got.CurrentSHA256 != nil {
		t.Errorf("apply that another call overlapped: %d %s, want no version", rec.Code, rec.Body)
	}
}

// rawCall is e.call for a goroutine other than the test's, such as a
// helper answer: it reports nothing through t.
func (e *writeEnv) rawCall(method, path string, body any) string {
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, path, nil)
	} else {
		b, _ := json.Marshal(body)
		r = httptest.NewRequest(method, path, bytes.NewReader(b))
	}
	r.Header.Set("Authorization", "Bearer "+e.srv.cfg.Token)
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, r)
	return recText(rec)
}

// TestISCSIRecoveryChangesVersion: any helper request but units may
// recover a dead apply and put fabrics.conf back. A service plan or a
// stage's dry run whose helper does so, and answers recovered, advances
// the version whether or not the file put back is byte-identical to the
// one before: otherwise comparing the versions would tell whether a dead
// write of a CHAP guess left the file as it was. While the request is in
// the helper, GET config answers no version. Units, which never recovers,
// leaves it.
func TestISCSIRecoveryChangesVersion(t *testing.T) {
	for _, c := range []struct {
		route string
		same  bool
	}{{"service", true}, {"service", false}, {"config", true}, {"config", false}} {
		same := c.same
		e := iscsiEnv(t, fabricsSample, "reload", nil)
		path, body := "/api/v1/service/libraries/10/restart", map[string]any{"dry_run": true}
		if c.route == "config" {
			path, body = "/api/v1/config/apply", map[string]any{"stage": e.staged().Stage, "dry_run": true}
		}
		get := func() *string {
			t.Helper()
			return decodeBody[iscsiConfigDTO](t, e.call(http.MethodGet, "/api/v1/transports/iscsi/config", nil)).SHA256
		}
		restored := fabricsSample
		if !same {
			restored = strings.Replace(fabricsSample, chapSecret, "Restored-0123", 1)
		}
		var during string
		e.answer(func(req apply.Request) apply.Response {
			if err := os.WriteFile(filepath.Join(e.dir, "fabrics.conf"), []byte(restored), 0o600); err != nil {
				return apply.Response{Result: apply.ResultError, Error: "io", Message: err.Error()}
			}
			during = e.rawCall(http.MethodGet, "/api/v1/transports/iscsi/config", nil)
			return apply.Response{Result: apply.ResultRecovered, Message: "recovered"}
		})
		before := get()
		if rec := e.call(http.MethodGet, "/api/v1/service", nil); rec.Code != http.StatusOK {
			t.Fatalf("GET service: %d %s", rec.Code, rec.Body)
		}
		if units := get(); str(units) != str(before) {
			t.Errorf("%s same %v: units changed the version: %s, before %s", c.route, same, str(units), str(before))
		}
		rec := e.call(http.MethodPost, path, body)
		if rec.Code != http.StatusConflict {
			t.Fatalf("%s same %v: %d %s", c.route, same, rec.Code, rec.Body)
		}
		after := get()
		if before == nil || after == nil || *after == *before {
			t.Errorf("%s same %v: version %s after the recovery, %s before: want both, different", c.route, same, str(after), str(before))
		}
		if !strings.HasPrefix(during, "200 ") || strings.Contains(during, `"sha256"`) {
			t.Errorf("%s same %v: GET config during the request: %s, want no version", c.route, same, during)
		}
	}
}

// TestISCSIWriteRacingRecoveryNoOracle: a write W1 reads fabrics.conf as
// a dead write of a CHAP guess left it; a service plan sent between W1's
// read and its helper call would recover and put the original file back
// before W1's base is checked. The helper would then apply W1 exactly
// when the guess was right (the file put back is byte-identical) and find
// a conflict otherwise, which a later GET would show even with W1's own
// answer uniform. The plan is answered in_progress without reaching the
// helper, so W1's answer, the configuration GET shows after it and the
// file are the same for a right and a wrong guess.
func TestISCSIWriteRacingRecoveryNoOracle(t *testing.T) {
	run := func(guess string) (string, string, string) {
		t.Helper()
		dead := strings.Replace(chapOnlySample, chapSecret, guess, 1)
		e := iscsiEnv(t, dead, "reload", nil)
		e.srv.fabricsKey = []byte("one key for both answers")
		helperLikeFabrics(e)
		check := e.h.answer
		e.answer(func(req apply.Request) apply.Response {
			if req.Op == apply.OpServicePlan {
				if err := os.WriteFile(filepath.Join(e.dir, "fabrics.conf"), []byte(chapOnlySample), 0o600); err != nil {
					return apply.Response{Result: apply.ResultError, Error: "io", Message: err.Error()}
				}
				return apply.Response{Result: apply.ResultRecovered, Message: "recovered"}
			}
			return check(req)
		})
		stage := e.staged().Stage
		var plans []string
		e.srv.beforeFabricsHelper = func() {
			e.srv.beforeFabricsHelper = nil
			plans = append(plans,
				e.rawCall(http.MethodPost, "/api/v1/service/libraries/10/restart", map[string]any{"dry_run": true}),
				e.rawCall(http.MethodPost, "/api/v1/config/apply", map[string]any{"stage": stage, "dry_run": true}))
		}
		got := recText(e.call(http.MethodPost, "/api/v1/transports/iscsi/unbind", map[string]any{"library": 30}))
		for _, plan := range plans {
			if !strings.HasPrefix(plan, "409 ") || !strings.Contains(plan, `"result":"`+apply.ResultInProgress+`"`) {
				t.Errorf("guess %q: plan during the write = %s, want 409 %s", guess, plan, apply.ResultInProgress)
			}
		}
		for _, r := range e.requests() {
			if r.Op == apply.OpServicePlan || r.Op == apply.OpPlan {
				t.Errorf("guess %q: a %s reached the helper", guess, r.Op)
			}
		}
		cfg := decodeBody[iscsiConfigDTO](t, e.call(http.MethodGet, "/api/v1/transports/iscsi/config", nil))
		if cfg.SHA256 == nil {
			t.Errorf("guess %q: GET config after the write has no version", guess)
		}
		cfg.SHA256 = nil
		b, _ := json.Marshal(cfg)
		// The files differ by the guess, so do their versions.
		got = regexp.MustCompile(`"sha256":"[0-9a-f]+"`).ReplaceAllString(got, `"sha256":"<version>"`)
		return got, string(b), strings.ReplaceAll(e.disk("fabrics.conf"), guess, "<guess>")
	}
	right, rightCfg, rightFile := run(chapSecret)
	wrong, wrongCfg, wrongFile := run("WrongGuess-0123")
	if right != wrong {
		t.Errorf("W1 answers differ:\nright %s\nwrong %s", right, wrong)
	}
	if rightCfg != wrongCfg || rightFile != wrongFile {
		t.Errorf("state after W1 differs:\nright %s\n%s\nwrong %s\n%s", rightCfg, rightFile, wrongCfg, wrongFile)
	}
	if !strings.HasPrefix(right, "200 ") || !strings.Contains(right, `"result":"applied"`) {
		t.Errorf("W1 = %s, want applied", right)
	}
}

// TestHelperCallOutlivesClient: a dry run whose client goes away while
// the helper works on it stays in flight until the helper answers: the
// helper goes on, and may recover and put fabrics.conf back, so GET config
// answers no version meanwhile. Once the helper answers, the call's
// sequence and revision steps are made.
func TestHelperCallOutlivesClient(t *testing.T) {
	e := iscsiEnv(t, fabricsSample, "reload", nil)
	release, entered := make(chan struct{}), make(chan struct{})
	e.answer(func(req apply.Request) apply.Response {
		close(entered)
		<-release
		return apply.Response{Result: apply.ResultValidated}
	})
	get := func() *string {
		t.Helper()
		return decodeBody[iscsiConfigDTO](t, e.call(http.MethodGet, "/api/v1/transports/iscsi/config", nil)).SHA256
	}
	before := get()
	e.srv.fabricsMu.Lock()
	seq, rev := e.srv.fabricsSeq, e.srv.fabricsRev
	e.srv.fabricsMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest(http.MethodPost, "/api/v1/service/libraries/10/restart", strings.NewReader(`{"dry_run":true}`)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+e.srv.cfg.Token)
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.handler.ServeHTTP(httptest.NewRecorder(), r)
	}()
	<-entered
	cancel()
	select {
	case <-done:
	case <-time.After(300 * time.Millisecond):
	}
	if v := get(); v != nil {
		t.Errorf("GET config while the helper still works = version %s, want none", *v)
	}
	close(release)
	<-done
	after := get()
	e.srv.fabricsMu.Lock()
	gotSeq, gotRev, writing := e.srv.fabricsSeq, e.srv.fabricsRev, e.srv.fabricsWriting
	e.srv.fabricsMu.Unlock()
	if gotSeq != seq+2 || gotRev != rev+1 || writing != 0 {
		t.Errorf("after the answer: seq %d rev %d writing %d, want %d %d 0", gotSeq, gotRev, writing, seq+2, rev+1)
	}
	if before == nil || after == nil || *after == *before {
		t.Errorf("version %s after, %s before: want both, different", str(after), str(before))
	}
}
