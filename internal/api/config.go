package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"mhvtl-control/internal/applyclient"
	"mhvtl-control/internal/conffile"
	"mhvtl-control/internal/config"
	"mhvtl-control/internal/hostids"
	"mhvtl-control/internal/media"
	"mhvtl-control/internal/personality"
	"mhvtl-control/internal/validate"
)

// The configuration editor's read-only routes (plan 2.4, PR 2b): the
// conffile model of device.conf and library_contents.<id>, the personality
// table, validation of a proposed configuration, and what the host offers for
// writes. Nothing here writes: changing the configuration is the privileged
// apply helper's job (plan Phase 3), behind write routes that come with it.

// CapabilityPaths are the host paths GET /config/capabilities inspects. A
// zero field takes the default DefaultCapabilityPaths gives it.
type CapabilityPaths struct {
	// HelperSocket is the apply helper's socket (plan 3.1:
	// mhvtl-control-apply.socket ListenStream). It exists only while the
	// socket unit is enabled and listening.
	HelperSocket string
	// LegacyEnv is the daemon's options file before it moved to
	// /etc/mhvtl-control (plan 2.1). The helper refuses to run while it
	// exists (plan 3.2 guard.go): it would sit in the directory the helper
	// writes.
	LegacyEnv string
	// Generators are where mhVTL installs its systemd generator,
	// mhvtl-device-conf-generator: SYSTEMD_GENERATOR_DIR, by default
	// /usr/lib/systemd/system-generators (mhvtl config.mk:29, upstream
	// master 852b78a), or systemd.pc's systemdsystemgeneratordir on Debian
	// (debian/rules:33), under /lib on releases before the /usr merge.
	Generators []string
	// MakeVTLMedia is mhVTL's make_vtl_media, a path or a name looked up in
	// PATH.
	MakeVTLMedia string
}

// DefaultCapabilityPaths are the installed locations.
var DefaultCapabilityPaths = CapabilityPaths{
	HelperSocket: "/run/mhvtl-control-apply.sock",
	LegacyEnv:    "/etc/mhvtl/mhvtl-control.env",
	Generators: []string{
		"/usr/lib/systemd/system-generators/mhvtl-device-conf-generator",
		"/lib/systemd/system-generators/mhvtl-device-conf-generator",
	},
	MakeVTLMedia: "make_vtl_media",
}

func (p CapabilityPaths) withDefaults() CapabilityPaths {
	d := DefaultCapabilityPaths
	if p.HelperSocket == "" {
		p.HelperSocket = d.HelperSocket
	}
	if p.LegacyEnv == "" {
		p.LegacyEnv = d.LegacyEnv
	}
	if p.Generators == nil {
		p.Generators = d.Generators
	}
	if p.MakeVTLMedia == "" {
		p.MakeVTLMedia = d.MakeVTLMedia
	}
	return p
}

// Values of capabilitiesDTO.Helper. With a helper client configured they
// come from the helper's hello (plan 3.2 intent.go): the daemon cannot see
// the helper's state directory, which its unit masks (plan 2.1).
const (
	helperAbsent      = "absent"
	helperPresent     = "present"
	helperLegacy      = "legacy_env_blocks"
	helperInProgress  = "in_progress"
	helperIncomplete  = "incomplete_apply"
	helperUnreadable  = "intent_unreadable"
	helperUnreachable = "unreachable"
)

// helloStates maps the helper's hello states to capabilitiesDTO.Helper.
var helloStates = map[string]string{
	applyclient.Absent:           helperAbsent,
	applyclient.Present:          helperPresent,
	applyclient.LegacyEnv:        helperLegacy,
	applyclient.InProgress:       helperInProgress,
	applyclient.Incomplete:       helperIncomplete,
	applyclient.IntentUnreadable: helperUnreadable,
}

// helloTTL is how long a hello answers GET /config/capabilities (plan 4.1):
// each hello starts a helper instance, as the socket unit runs one per
// connection.
const helloTTL = 2 * time.Second

// helloTimeout bounds one hello.
const helloTimeout = 5 * time.Second

// helloCache holds the last hello, from when it ended, and the hello in
// flight, which every caller that asks meanwhile shares.
type helloCache struct {
	mu     sync.Mutex
	at     time.Time
	state  string
	err    error
	flight *helloFlight
}

// helloFlight is a hello in progress; state and err are set before done is
// closed.
type helloFlight struct {
	done  chan struct{}
	state string
	err   error
}

// helperState returns the helper's hello state, asking it at most every
// helloTTL. The hello runs apart from the request that started it, so a
// client that goes away neither cancels it for the others nor gets its
// cancellation cached; a caller whose own request ends stops waiting.
func (s *Server) helperState(ctx context.Context) (string, error) {
	c := &s.hello
	c.mu.Lock()
	if !c.at.IsZero() && s.now().Sub(c.at) < helloTTL {
		defer c.mu.Unlock()
		return c.state, c.err
	}
	f := c.flight
	if f == nil {
		f = &helloFlight{done: make(chan struct{})}
		c.flight = f
		go s.hello1(context.WithoutCancel(ctx), f)
	}
	c.mu.Unlock()
	select {
	case <-f.done:
		return f.state, f.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// hello1 asks the helper once and caches the answer. A Hello that panics
// is its callers' error, and not cached: it runs on its own goroutine, out
// of reach of the HTTP recover middleware, and the flight must end so the
// next caller asks again rather than wait on it.
func (s *Server) hello1(ctx context.Context, f *helloFlight) {
	ctx, cancel := context.WithTimeout(ctx, helloTimeout)
	defer cancel()
	c := &s.hello
	defer func() {
		if r := recover(); r != nil {
			log.Printf("apply helper hello panicked: %v\n%s", r, debug.Stack())
			f.state, f.err = "", errors.New("the apply helper check failed; the daemon log has the cause")
			c.mu.Lock()
			c.flight = nil
			c.mu.Unlock()
			close(f.done)
		}
	}()
	f.state, f.err = s.cfg.Helper.Hello(ctx)
	c.mu.Lock()
	c.state, c.err, c.at, c.flight = f.state, f.err, s.now(), nil
	c.mu.Unlock()
	close(f.done)
}

// Values of capabilitiesDTO.Writes.
const (
	writesOK          = "ok"
	writesTLSRequired = "tls_required"
	// writesNoToken: no API token is configured. Config writes make root
	// rewrite /etc/mhvtl and restart services, so they are not open to
	// every local user, on loopback either.
	writesNoToken  = "token_required"
	writesNoHelper = "helper_absent"
	writesBlocked  = "helper_blocked"
)

// capabilitiesDTO says whether configuration writes could be accepted, and
// what the host offers them.
type capabilitiesDTO struct {
	// Helper is the apply helper's state: absent, present, present but
	// refusing while the legacy options file exists (legacy_env_blocks),
	// running an apply (in_progress), holding an apply that died and that
	// it recovers first (incomplete_apply), or one whose intent it cannot
	// read and that needs recovering by hand (intent_unreadable); with the
	// socket there but no usable answer, unreachable.
	Helper string `json:"helper"`
	// HelperError says why the helper is unreachable.
	HelperError string `json:"helper_error,omitempty"`
	// Writes says whether the UI should offer edits (plan 4.1): no helper
	// socket (helper_absent), else a non-loopback listen without TLS, which
	// the write routes refuse (tls_required), else no API token configured
	// (token_required), else a helper that refuses
	// every change until the operator acts, intent_unreadable or
	// legacy_env_blocks (helper_blocked), else ok. An apply running or one
	// the helper recovers first is not a lasting refusal.
	Writes string `json:"writes"`
	// Generator is whether mhVTL's systemd generator is installed, so an
	// apply needs only daemon-reload to add or remove instances (AC 11).
	Generator bool `json:"generator"`
	// PerLibraryHomeMedia is whether make_vtl_media creates each library's
	// media in that library's home (markh794/mhvtl#276). Without it, an apply
	// that needs media in a library's own home is refused (plan 1b.2).
	PerLibraryHomeMedia bool `json:"per_library_home_media"`
}

func (s *Server) capabilities(ctx context.Context) capabilitiesDTO {
	p := s.cfg.Capabilities.withDefaults()
	c := capabilitiesDTO{Helper: helperAbsent}
	if s.cfg.Helper != nil {
		state, err := s.helperState(ctx)
		switch h, ok := helloStates[state]; {
		case err != nil:
			c.Helper, c.HelperError = helperUnreachable, err.Error()
		case ok:
			c.Helper = h
		default:
			c.Helper, c.HelperError = helperUnreachable, fmt.Sprintf("unknown state %q", state)
		}
	} else if fi, err := os.Lstat(p.HelperSocket); err == nil && fi.Mode()&fs.ModeSocket != 0 {
		c.Helper = helperPresent
		if _, err := os.Lstat(p.LegacyEnv); err == nil {
			c.Helper = helperLegacy
		}
	}
	switch {
	case c.Helper == helperAbsent:
		c.Writes = writesNoHelper
	case s.cfg.InsecureListen:
		c.Writes = writesTLSRequired
	case s.cfg.Token == "":
		c.Writes = writesNoToken
	case c.Helper == helperUnreadable || c.Helper == helperLegacy:
		c.Writes = writesBlocked
	default:
		c.Writes = writesOK
	}
	for _, g := range p.Generators {
		if fi, err := os.Stat(g); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			c.Generator = true
			break
		}
	}
	c.PerLibraryHomeMedia = media.PerLibraryHome(p.MakeVTLMedia)
	return c
}

func (s *Server) handleConfigCapabilities(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.capabilities(r.Context()))
}

// configSnapshot is the configuration in force: the parsed model and the
// bytes it was parsed from, by file name.
type configSnapshot struct {
	inv   config.Inventory
	files map[string][]byte
}

// readConfig reads device.conf and the library_contents files. A missing
// device.conf gives an empty configuration, as GET /libraries treats it.
func (s *Server) readConfig() (configSnapshot, error) {
	snap := configSnapshot{files: map[string][]byte{}}
	inv, err := config.ReadInventory(s.cfg.ConfigDir, func(name string) ([]byte, error) {
		b, err := os.ReadFile(filepath.Join(s.cfg.ConfigDir, name))
		if err == nil {
			snap.files[name] = b
		}
		return b, err
	})
	var dce *config.DeviceConfError
	if errors.As(err, &dce) && errors.Is(err, fs.ErrNotExist) {
		return snap, nil
	}
	snap.inv = inv
	return snap, err
}

// readConfigOrFail is readConfig, writing a 500 when it fails.
func (s *Server) readConfigOrFail(w http.ResponseWriter) (configSnapshot, bool) {
	snap, err := s.readConfig()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "config_error", err.Error())
		return snap, false
	}
	return snap, true
}

// liveContents returns lib's library_contents (data) with its live placement
// written in, the base a client edits to build a proposed library_contents
// (plan, amendment "Phase 4 client contract for next"): the live cartridge on
// each Slot and MAP line, a Slot line for each slot the Slot lines skip that
// holds a cartridge live, and no barcode on a Picker line whose cartridge the
// live view places elsewhere: the live view has no picker to say where it
// is, as changer.Inventory keeps no medium transport elements (mhVTL's READ
// ELEMENT STATUS does report them).
func liveContents(data []byte, lib config.Library, live validate.Placement) ([]byte, error) {
	c := conffile.ParseLibContents(data)
	base := validate.Baseline(lib, &live, nil)
	file, highest := map[int]string{}, 0
	for _, sl := range lib.Slots {
		file[sl.Number] = sl.Barcode
		highest = max(highest, sl.Number)
	}
	for n := 1; n <= highest; n++ {
		want := base.Slots[n]
		have, listed := file[n]
		var err error
		switch {
		case listed && have != want:
			err = c.SetBarcode("Slot", n, want)
		case !listed && want != "":
			err = c.AddEntry("Slot", n, want)
		}
		if err != nil {
			return nil, err
		}
	}
	for _, m := range lib.MAPs {
		if want := base.MAPs[m.Number]; m.Barcode != want {
			if err := c.SetBarcode("MAP", m.Number, want); err != nil {
				return nil, err
			}
		}
	}
	elsewhere := map[string]bool{}
	for _, els := range []map[int]string{base.Slots, base.MAPs, live.Drives} {
		for _, b := range els {
			elsewhere[b] = true
		}
	}
	for _, pm := range lib.PickerMedia {
		if elsewhere[pm.Barcode] {
			if err := c.SetBarcode("Picker", pm.Number, ""); err != nil {
				return nil, err
			}
		}
	}
	return c.Render(), nil
}

type configProblemDTO struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Message string `json:"message"`
}

func toProblems(ps []config.Problem) []configProblemDTO {
	out := make([]configProblemDTO, 0, len(ps))
	for _, p := range ps {
		out = append(out, configProblemDTO{File: filepath.Base(p.File), Line: p.Line, Message: p.Msg})
	}
	return out
}

type elementStartDTO struct {
	Picker  int `json:"picker"`
	MAP     int `json:"map"`
	Drive   int `json:"drive"`
	Storage int `json:"storage"`
}

// personalityDTO is a library personality: its element windows and the
// element counts they allow, and its INQUIRY identification pages.
type personalityDTO struct {
	InitFn       string             `json:"init_fn"`
	Name         string             `json:"name"`
	Start        elementStartDTO    `json:"start"`
	WindowSource string             `json:"window_source"`
	Limits       personality.Limits `json:"limits"`
	VPD83        bool               `json:"vpd83"`
	SendsNAA     bool               `json:"sends_naa"`
}

func toPersonality(m personality.Model) personalityDTO {
	return personalityDTO{
		InitFn:       m.InitFn,
		Name:         m.Name,
		Start:        elementStartDTO{int(m.StartPicker), int(m.StartMAP), int(m.StartDrive), int(m.StartStorage)},
		WindowSource: m.WindowSource,
		Limits:       m.Limits(),
		VPD83:        m.VPD83,
		SendsNAA:     m.SendsNAA,
	}
}

// selectedPersonalityDTO is the personality a library gets, with the
// dispatch rule that selects it (mhvtl usr/cmd/vtllibrary.c customise_lu).
type selectedPersonalityDTO struct {
	personalityDTO
	RuleSource string `json:"rule_source"`
}

type driveLineDTO struct {
	Number int    `json:"number"`
	Serial string `json:"serial"`
}

// configLibraryDTO is a library as device.conf and library_contents.<id>
// define it. Slots, MAPs and pickers are the file's, not the live state.
type configLibraryDTO struct {
	ID      int    `json:"id"`
	Channel int    `json:"channel"`
	Target  int    `json:"target"`
	LUN     int    `json:"lun"`
	Vendor  string `json:"vendor"`
	Product string `json:"product"`
	Serial  string `json:"serial"`
	NAA     string `json:"naa"`
	HomeDir string `json:"home_dir"`
	// Home is the media home the library resolves to (HomeDir, else
	// MHVTL_HOME_PATH).
	Home        string                 `json:"home"`
	Personality selectedPersonalityDTO `json:"personality"`
	Drives      []driveDTO             `json:"drives"`
	DriveLines  []driveLineDTO         `json:"drive_lines"`
	Slots       []slotDTO              `json:"slots"`
	MAPs        []slotDTO              `json:"maps"`
	Pickers     []int                  `json:"pickers"`
	PickerMedia []slotDTO              `json:"picker_media"`
}

func (s *Server) toConfigLibrary(l config.Library) configLibraryDTO {
	base := toLibraryDTO(l)
	p := personality.Lookup(l.Vendor, l.Product)
	lines := make([]driveLineDTO, 0, len(l.DriveLines))
	for _, d := range l.DriveLines {
		lines = append(lines, driveLineDTO{d.Number, d.Serial})
	}
	pm := make([]slotDTO, 0, len(l.PickerMedia))
	for _, m := range l.PickerMedia {
		pm = append(pm, slotDTO{Number: m.Number, Barcode: m.Barcode})
	}
	return configLibraryDTO{
		ID: l.ID, Channel: l.Channel, Target: l.Target, LUN: l.LUN,
		Vendor: l.Vendor, Product: l.Product, Serial: l.Serial, NAA: l.NAA, HomeDir: l.HomeDir,
		Home:        s.homeFor(l),
		Personality: selectedPersonalityDTO{toPersonality(p.Model), p.Rule.Source},
		Drives:      base.Drives,
		DriveLines:  lines,
		Slots:       base.Slots,
		MAPs:        base.MAPs,
		Pickers:     base.Pickers,
		PickerMedia: pm,
	}
}

func (s *Server) configLibraries(inv config.Inventory) []configLibraryDTO {
	out := make([]configLibraryDTO, 0, len(inv.Libraries))
	for _, l := range inv.Libraries {
		out = append(out, s.toConfigLibrary(l))
	}
	return out
}

// configFileDTO is one configuration file as read. LiveContent is the file
// with the live placement written in (liveContents), present when the
// library's changer could be read.
type configFileDTO struct {
	Name        string  `json:"name"`
	SHA256      string  `json:"sha256"`
	Content     string  `json:"content"`
	LiveContent *string `json:"live_content,omitempty"`
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	snap, ok := s.readConfigOrFail(w)
	if !ok {
		return
	}
	live, liveErrs := s.readLive(r.Context(), snap.inv.Libraries)
	names := make([]string, 0, len(snap.files))
	for n := range snap.files {
		names = append(names, n)
	}
	sort.Strings(names)
	files := make([]configFileDTO, 0, len(names))
	for _, n := range names {
		f := configFileDTO{Name: n, SHA256: conffile.Hash(snap.files[n]), Content: string(snap.files[n])}
		if lib, ok := contentsLibrary(snap.inv, n); ok {
			if p, ok := live[lib.ID]; ok {
				if b, err := liveContents(snap.files[n], lib, p); err == nil {
					lc := string(b)
					f.LiveContent = &lc
				} else {
					liveErrs[lib.ID] = "live placement not written into " + n + ": " + err.Error()
				}
			}
		}
		files = append(files, f)
	}
	slots := map[int]slotsViewDTO{}
	for _, l := range snap.inv.Libraries {
		p, isLive := live[l.ID]
		slots[l.ID] = slotsView(l, p, isLive, liveErrs[l.ID])
	}
	standalone := make([]driveDTO, 0, len(snap.inv.Standalone))
	for _, d := range snap.inv.Standalone {
		standalone = append(standalone, toDriveDTO(d))
	}
	sys := s.systemValues()
	var conf []byte
	if sys.File != nil {
		conf = []byte(sys.File.Content)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"config_dir": s.cfg.ConfigDir,
		// default_home is where the dashboard looks for media (--home-dir);
		// default_home_effective is what mhVTL and the apply helper use.
		"default_home":           s.homeFor(config.Library{}),
		"default_home_effective": effectiveDefaultHome(conf),
		"files":                  files,
		"libraries":              s.configLibraries(snap.inv),
		"standalone_drives":      standalone,
		"problems":               toProblems(snap.inv.Problems),
		"live_errors":            liveErrs,
		// The slots of each library, as GET /config/libraries/{id}/slots
		// gives them, so a client reads every changer once.
		"slots":  slots,
		"system": sys,
	})
}

// systemDTO is mhvtl.conf's values of the keys PATCH /config/system sets
// (a key the file lacks gives none), and the file, which a client diffs a
// staged one against; none when it does not exist.
type systemDTO struct {
	Values map[string]string `json:"values"`
	File   *configFileDTO    `json:"file,omitempty"`
}

func (s *Server) systemValues() systemDTO {
	d := systemDTO{Values: map[string]string{}}
	data, err := os.ReadFile(filepath.Join(s.cfg.ConfigDir, mhvtlConf))
	if err != nil {
		return d
	}
	d.File = &configFileDTO{Name: mhvtlConf, SHA256: conffile.Hash(data), Content: string(data)}
	c := conffile.ParseMhvtlConf(data)
	for k := range systemKeys {
		if v, ok := c.Get(k); ok {
			d.Values[k] = v
		}
	}
	return d
}

// contentsLibrary returns the library whose library_contents file name is.
func contentsLibrary(inv config.Inventory, name string) (config.Library, bool) {
	for _, l := range inv.Libraries {
		if name == "library_contents."+strconv.Itoa(l.ID) {
			return l, true
		}
	}
	return config.Library{}, false
}

func (s *Server) handleConfigLibraries(w http.ResponseWriter, _ *http.Request) {
	snap, ok := s.readConfigOrFail(w)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"libraries": s.configLibraries(snap.inv)})
}

// configLibrary resolves the {id} path value, writing the error response
// itself when it fails.
func (s *Server) configLibrary(w http.ResponseWriter, r *http.Request) (config.Library, bool) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "library id must be an integer")
		return config.Library{}, false
	}
	snap, ok := s.readConfigOrFail(w)
	if !ok {
		return config.Library{}, false
	}
	for _, l := range snap.inv.Libraries {
		if l.ID == id {
			return l, true
		}
	}
	writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("library %d not found", id))
	return config.Library{}, false
}

func (s *Server) handleConfigLibrary(w http.ResponseWriter, r *http.Request) {
	if l, ok := s.configLibrary(w, r); ok {
		writeJSON(w, http.StatusOK, s.toConfigLibrary(l))
	}
}

func (s *Server) handleConfigDrives(w http.ResponseWriter, r *http.Request) {
	l, ok := s.configLibrary(w, r)
	if !ok {
		return
	}
	dto := s.toConfigLibrary(l)
	writeJSON(w, http.StatusOK, map[string]any{"library": l.ID, "drives": dto.Drives, "drive_lines": dto.DriveLines})
}

// configSlotDTO is a storage or MAP slot: what library_contents lists and,
// when the library is live, what READ ELEMENT STATUS reports.
type configSlotDTO struct {
	Number  int    `json:"number"`
	Barcode string `json:"barcode"`
	// Listed is false for a storage slot below the highest Slot line that no
	// line names: vtllibrary makes it, empty (usr/cmd/vtllibrary.c
	// :1054-1064, upstream master 852b78a).
	Listed      bool    `json:"listed"`
	LiveBarcode *string `json:"live_barcode,omitempty"`
}

// slotsViewDTO is GET /config/libraries/{id}/slots, and the slots of each
// library in GET /config.
type slotsViewDTO struct {
	Library   int             `json:"library"`
	Live      bool            `json:"live"`
	LiveError string          `json:"live_error,omitempty"`
	Slots     []configSlotDTO `json:"slots"`
	MAPs      []configSlotDTO `json:"maps"`
	// Drives are the drive elements holding a cartridge now, by drive slot
	// number; empty when the library is not live.
	Drives []liveDriveDTO `json:"drives"`
	// Warning says that the running library holds cartridges in storage or
	// MAP slots library_contents does not make: the file changed since the
	// library started.
	Warning string `json:"warning,omitempty"`
}

type liveDriveDTO struct {
	Number  int    `json:"number"`
	Barcode string `json:"barcode"`
}

// slotsView lists l's storage and MAP slots with the file's and, when the
// placement is known (isLive), the live barcode.
func slotsView(l config.Library, p validate.Placement, isLive bool, liveErr string) slotsViewDTO {
	liveOf := func(m map[int]string, n int) *string {
		if !isLive {
			return nil
		}
		b := m[n]
		return &b
	}
	listed, highest := map[int]string{}, 0
	for _, sl := range l.Slots {
		listed[sl.Number] = sl.Barcode
		highest = max(highest, sl.Number)
	}
	slots := []configSlotDTO{}
	for n := 1; n <= highest; n++ {
		b, in := listed[n]
		slots = append(slots, configSlotDTO{Number: n, Barcode: b, Listed: in, LiveBarcode: liveOf(p.Slots, n)})
	}
	maps := []configSlotDTO{}
	for _, m := range l.MAPs {
		maps = append(maps, configSlotDTO{Number: m.Number, Barcode: m.Barcode, Listed: true, LiveBarcode: liveOf(p.MAPs, m.Number)})
	}
	v := slotsViewDTO{Library: l.ID, Live: isLive, LiveError: liveErr, Slots: slots, MAPs: maps, Drives: []liveDriveDTO{}}
	if !isLive {
		return v
	}
	for _, n := range sortedInts(p.Drives) {
		v.Drives = append(v.Drives, liveDriveDTO{n, p.Drives[n]})
	}
	inFile := map[int]bool{}
	for _, m := range l.MAPs {
		inFile[m.Number] = true
	}
	var extra []string
	for _, n := range sortedInts(p.Slots) {
		if n > highest {
			extra = append(extra, fmt.Sprintf("slot %d (%s)", n, p.Slots[n]))
		}
	}
	for _, n := range sortedInts(p.MAPs) {
		if !inFile[n] {
			extra = append(extra, fmt.Sprintf("MAP slot %d (%s)", n, p.MAPs[n]))
		}
	}
	if len(extra) > 0 {
		v.Warning = fmt.Sprintf("the running library holds cartridges in %s, which library_contents.%d does not list: the file changed since the library started, and the library makes other slots when it restarts",
			strings.Join(extra, ", "), l.ID)
	}
	return v
}

func sortedInts(m map[int]string) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}

func (s *Server) handleConfigSlots(w http.ResponseWriter, r *http.Request) {
	l, ok := s.configLibrary(w, r)
	if !ok {
		return
	}
	live, liveErrs := s.readLive(r.Context(), []config.Library{l})
	p, isLive := live[l.ID]
	writeJSON(w, http.StatusOK, slotsView(l, p, isLive, liveErrs[l.ID]))
}

type ruleDTO struct {
	Vendor        string `json:"vendor"`
	ProductPrefix string `json:"product_prefix"`
	Source        string `json:"source"`
	// Order is the rule's place in vtllibrary's evaluation, across
	// personalities: the first that matches wins.
	Order int `json:"order"`
}

type mediaDTO struct {
	Name string `json:"name"`
	Load string `json:"load"` // "rw" or "ro"
}

type driveModelDTO struct {
	Product string     `json:"product"`
	InitFn  string     `json:"init_fn"`
	Source  string     `json:"source"`
	WORM    bool       `json:"worm"`
	Media   []mediaDTO `json:"media"`
}

type targetAppDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (s *Server) handleConfigPersonalities(w http.ResponseWriter, _ *http.Request) {
	type libraryPersonality struct {
		personalityDTO
		// Rules are the dispatch branches that select it, in evaluation
		// order: the first rule whose prefixes match a library's vendor and
		// product (compared case-insensitively over the prefix) wins.
		Rules []ruleDTO `json:"rules"`
	}
	var libs []libraryPersonality
	at := map[string]int{}
	for n, r := range personality.Rules {
		i, ok := at[r.InitFn]
		if !ok {
			i = len(libs)
			at[r.InitFn] = i
			libs = append(libs, libraryPersonality{personalityDTO: toPersonality(personality.Models[r.InitFn])})
		}
		libs[i].Rules = append(libs[i].Rules, ruleDTO{r.Vendor, r.ProductPrefix, r.Source, n})
	}
	drives := make([]driveModelDTO, 0, len(personality.DriveProducts))
	for _, d := range personality.DriveProducts {
		m := personality.DriveFor(d.Product)
		media := make([]mediaDTO, 0, len(m.Media))
		for _, md := range m.Media {
			load := "ro"
			if md.Load == personality.RW {
				load = "rw"
			}
			media = append(media, mediaDTO{md.Name, load})
		}
		drives = append(drives, driveModelDTO{d.Product, d.InitFn, d.Source, m.WORM, media})
	}
	apps := make([]targetAppDTO, 0, len(validate.Presets))
	for id, p := range validate.Presets {
		apps = append(apps, targetAppDTO{id, p.Name})
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].ID < apps[j].ID })
	writeJSON(w, http.StatusOK, map[string]any{"libraries": libs, "drives": drives, "target_apps": apps})
}

// Bounds of a validation request, those of the apply helper's protocol
// (plan 3.2 proto.go); the body as a whole is bounded by maxBody (1 MiB).
const (
	maxValidateFiles    = 64
	maxValidateFileSize = 256 << 10
)

var contentsName = regexp.MustCompile(`^library_contents\.([0-9]{1,5})$`)

// contentsID returns the library ID of a library_contents.<id> file name.
// The ID must be written as mhVTL builds the name, "library_contents.%ld"
// (usr/vtllib.c:322, upstream master 852b78a): library_contents.010 is a
// file vtllibrary never reads, so validating it would prove nothing.
func contentsID(name string) (int, bool) {
	m := contentsName.FindStringSubmatch(name)
	if m == nil {
		return 0, false
	}
	id, err := strconv.Atoi(m[1])
	return id, err == nil && name == "library_contents."+strconv.Itoa(id)
}

// validateRequest is a proposed configuration, given as whole files.
//
// Files maps a file name, device.conf or library_contents.<id>, to its
// proposed content; null deletes a library_contents file. A file left out
// is unchanged: device.conf as it is on disk, and a library_contents.<id>
// as the live view has it (configFileDTO.LiveContent, from GET /config),
// or as on disk when the library's changer cannot be read. That is the view
// validation reads a proposal as an edit of (validate.Merge), so a request
// that changes only device.conf proposes no cartridge moves.
type validateRequest struct {
	Files map[string]*string `json:"files"`
	// TargetApps maps a library ID to the backup application it is set up
	// for (GET /config/personalities target_apps).
	TargetApps map[int]string `json:"target_apps"`
	// Dispositions say where the cartridges that would leave a library go.
	Dispositions []validate.Disposition `json:"dispositions"`
}

type validateResponse struct {
	OK       bool               `json:"ok"`
	Errors   []validate.Finding `json:"errors"`
	Warnings []validate.Finding `json:"warnings"`
	// Problems are the lines of the proposed files mhVTL would not read as
	// written; each is also an error finding (file.problem).
	Problems []configProblemDTO `json:"problems"`
	// LiveErrors says, by library ID, why its live placement is unknown;
	// validation then reads library_contents in its place.
	LiveErrors map[int]string `json:"live_errors"`
}

func (s *Server) handleConfigValidate(w http.ResponseWriter, r *http.Request) {
	var req validateRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	err := dec.Decode(&req)
	if err == nil {
		// One JSON object, and nothing after it.
		if err = dec.Decode(&struct{}{}); err == io.EOF {
			err = nil
		} else if err == nil {
			err = errors.New("unexpected data after the JSON object")
		}
	}
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("request body over %d bytes", maxBody))
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if len(req.Files) > maxValidateFiles {
		writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("%d files; at most %d", len(req.Files), maxValidateFiles))
		return
	}
	for name, c := range req.Files {
		switch {
		case name == "device.conf" && c == nil:
			writeError(w, http.StatusBadRequest, "bad_request", "device.conf cannot be deleted")
			return
		case name != "device.conf" && !validContentsName(name):
			writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("file %q: only device.conf and library_contents.<id> can be proposed", name))
			return
		case c != nil && len(*c) > maxValidateFileSize:
			writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("file %q is over %d bytes", name, maxValidateFileSize))
			return
		}
	}

	snap, ok := s.readConfigOrFail(w)
	if !ok {
		return
	}
	cur := snap.inv
	liveP, liveErrs := s.readLive(r.Context(), cur.Libraries)
	base := map[string][]byte{}
	for name, b := range snap.files {
		base[name] = b
		if lib, ok := contentsLibrary(cur, name); ok {
			if p, ok := liveP[lib.ID]; ok {
				lc, err := liveContents(b, lib, p)
				if err != nil {
					// Without the live view written in, a proposal built
					// from it would read as moving cartridges.
					delete(liveP, lib.ID)
					liveErrs[lib.ID] = "live placement not written into " + name + ": " + err.Error()
					continue
				}
				base[name] = lc
			}
		}
	}
	next, err := config.ReadInventory(s.cfg.ConfigDir, func(name string) ([]byte, error) {
		if c, ok := req.Files[name]; ok {
			if c == nil {
				return nil, fs.ErrNotExist
			}
			return []byte(*c), nil
		}
		if b, ok := base[name]; ok {
			return b, nil
		}
		// A library new in the proposal: mhVTL would read whatever file of
		// that name is on disk.
		return os.ReadFile(filepath.Join(s.cfg.ConfigDir, name))
	})
	var dce *config.DeviceConfError
	if errors.As(err, &dce) && errors.Is(err, fs.ErrNotExist) {
		// No device.conf, in force or proposed: no libraries.
		next, err = config.Inventory{}, nil
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "config_error", err.Error())
		return
	}
	for name := range req.Files {
		if name == "device.conf" {
			continue
		}
		id, _ := contentsID(name)
		_, inCur := findConfigLibrary(cur, id)
		_, inNext := findConfigLibrary(next, id)
		if !inCur && !inNext {
			writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("file %q: library %d is in neither the current nor the proposed device.conf", name, id))
			return
		}
	}

	var live *validate.Live
	if len(liveP) > 0 {
		live = &validate.Live{Libraries: liveP}
	}
	sysRoot := s.cfg.SysRoot
	if sysRoot == "" {
		sysRoot = "/sys"
	}
	host, hostErr := hostids.Read(sysRoot)
	res := validate.Validate(cur, next, live, host, validate.Options{
		TargetApps:   req.TargetApps,
		Dispositions: req.Dispositions,
		ConfigDir:    s.cfg.ConfigDir,
		DefaultHome:  s.homeFor(config.Library{}),
		HostErr:      hostErr,
	})
	writeJSON(w, http.StatusOK, validateResponse{
		OK:         res.OK(),
		Errors:     res.Errors,
		Warnings:   res.Warnings,
		Problems:   toProblems(next.Problems),
		LiveErrors: liveErrs,
	})
}

func validContentsName(name string) bool {
	_, ok := contentsID(name)
	return ok
}

func findConfigLibrary(inv config.Inventory, id int) (config.Library, bool) {
	for _, l := range inv.Libraries {
		if l.ID == id {
			return l, true
		}
	}
	return config.Library{}, false
}
