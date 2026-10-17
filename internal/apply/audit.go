package apply

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"mhvtl-control/internal/conffile"
)

const (
	auditFile = "audit.jsonl"
	// maxDiff caps the diff kept in one audit record.
	maxDiff = 16 << 10
)

// AuditRecord is one line of StateDir/audit.jsonl (plan 3.2, write.go; AC
// 4): who asked, when, from which process, what changed and how it ended.
type AuditRecord struct {
	TS    time.Time   `json:"ts"`
	Actor string      `json:"actor"`
	Peer  Peer        `json:"peer"`
	Op    string      `json:"op"`
	Files []AuditFile `json:"files"`
	// Diff is a line diff of the changed files, cut at maxDiff bytes
	// (DiffTruncated).
	Diff          string   `json:"diff,omitempty"`
	DiffTruncated bool     `json:"diff_truncated,omitempty"`
	Units         []string `json:"units"`
	Result        string   `json:"result"`
	Error         string   `json:"error,omitempty"`
	Message       string   `json:"message,omitempty"`
	// OrphanedMedia are cartridges an apply left out of every library;
	// their media is kept. PR 3b's merge fills it.
	OrphanedMedia []string   `json:"orphaned_media"`
	Conflicts     []Conflict `json:"conflicts,omitempty"`
}

// AuditFile is a file's hash before and after ("" when absent).
type AuditFile struct {
	Name   string `json:"name"`
	Before string `json:"before"`
	After  string `json:"after"`
}

// audit appends rec to audit.jsonl, fsyncs it, and sends it to the
// journal. A failure to audit is logged, never hidden.
func (h *Helper) audit(rec AuditRecord) {
	if rec.Units == nil {
		rec.Units = []string{}
	}
	if rec.Files == nil {
		rec.Files = []AuditFile{}
	}
	if rec.OrphanedMedia == nil {
		rec.OrphanedMedia = []string{}
	}
	b, err := json.Marshal(rec)
	if err == nil {
		err = appendSync(filepath.Join(h.cfg.StateDir, auditFile), append(b, '\n'))
	}
	if err != nil {
		h.cfg.Logf("audit: %v", err)
	}
	var names []string
	for _, f := range rec.Files {
		names = append(names, f.Name)
	}
	msg := fmt.Sprintf("%s by %q (pid %d): %s", rec.Op, rec.Actor, rec.Peer.PID, rec.Result)
	if rec.Error != "" {
		msg += " (" + rec.Error + ": " + rec.Message + ")"
	}
	if len(names) > 0 {
		msg += "; files " + strings.Join(names, ", ")
	}
	h.cfg.Journal(map[string]string{
		"MESSAGE":                 msg,
		"MESSAGE_ID":              messageID(rec.Op, rec.Result),
		"PRIORITY":                priority(rec.Result),
		"MHVTL_APPLY_OP":          rec.Op,
		"MHVTL_APPLY_RESULT":      rec.Result,
		"MHVTL_APPLY_ACTOR":       rec.Actor,
		"MHVTL_APPLY_PEER_PID":    fmt.Sprint(rec.Peer.PID),
		"MHVTL_APPLY_PEER_CGROUP": rec.Peer.Cgroup,
		"MHVTL_APPLY_FILES":       strings.Join(names, " "),
	})
}

// maxAudit is the size at which audit.jsonl is rotated to audit.jsonl.1,
// the one older file kept. A variable for the tests.
var maxAudit int64 = 16 << 20

// appendSync appends b to path and fsyncs it, first rotating path to
// path.1 when b would take it past maxAudit. An flock on the file
// serializes helpers appending at once (peer refusals are audited outside
// exclusive.lock); one that finds the path renamed under it reopens.
func appendSync(path string, b []byte) error {
	for {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
		if err != nil {
			return err
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
			f.Close()
			return err
		}
		fi, err := f.Stat()
		if err != nil {
			f.Close()
			return err
		}
		if pi, err := os.Lstat(path); err != nil || !os.SameFile(fi, pi) {
			f.Close()
			continue
		}
		if fi.Size() > 0 && fi.Size()+int64(len(b)) > maxAudit {
			err := os.Rename(path, path+".1")
			if err == nil {
				err = syncDir(filepath.Dir(path))
			}
			f.Close()
			if err != nil {
				return err
			}
			continue
		}
		if _, err := f.Write(b); err != nil {
			f.Close()
			return err
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	}
}

// Journal MESSAGE_IDs, for journalctl MESSAGE_ID=... and catalog entries.
const (
	msgApplied   = "3f3c62b593644e98a0567aefd6f95245"
	msgRefused   = "19402d28d2e945bd8825910af1355ac1"
	msgRecovered = "c8fe29ec70b54af786cc0860777c4969"
	msgFailed    = "dd5aae8285324411b47f8e1343fb7e41"
)

func messageID(op, result string) string {
	switch {
	case result == ResultRefused || result == ResultConflict || result == ResultInProgress:
		return msgRefused
	case result == ResultRecovered || op == OpRecover && result == ResultOK:
		return msgRecovered
	case result == ResultError:
		return msgFailed
	}
	return msgApplied
}

// priority is the syslog priority: 3 err, 4 warning, 6 info.
func priority(result string) string {
	switch result {
	case ResultError:
		return "3"
	case ResultRefused, ResultConflict, ResultRecovered:
		return "4"
	}
	return "6"
}

// JournalSocket is journald's native protocol socket.
const JournalSocket = "/run/systemd/journal/socket"

// SendJournal sends fields to journald over its native protocol
// (https://systemd.io/JOURNAL_NATIVE_PROTOCOL/): one datagram, KEY=value
// lines, and the binary form for a value with a newline.
func SendJournal(socket string, fields map[string]string) error {
	var b bytes.Buffer
	b.WriteString("SYSLOG_IDENTIFIER=mhvtl-control-apply\n")
	for k, v := range fields {
		if !strings.Contains(v, "\n") {
			b.WriteString(k + "=" + v + "\n")
			continue
		}
		b.WriteString(k + "\n")
		binary.Write(&b, binary.LittleEndian, uint64(len(v)))
		b.WriteString(v + "\n")
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write(b.Bytes())
	return err
}

// lineDiff is a minimal diff of a and b: the lines between their common
// head and tail, as removed ("-") and added ("+") lines, under a header
// naming the file.
func lineDiff(name string, a, b []byte) string {
	al, bl := strings.SplitAfter(string(a), "\n"), strings.SplitAfter(string(b), "\n")
	i := 0
	for i < len(al) && i < len(bl) && al[i] == bl[i] {
		i++
	}
	j := 0
	for j < len(al)-i && j < len(bl)-i && al[len(al)-1-j] == bl[len(bl)-1-j] {
		j++
	}
	var s strings.Builder
	fmt.Fprintf(&s, "--- %s\n+++ %s\n@@ line %d @@\n", name, name, i+1)
	for _, l := range al[i : len(al)-j] {
		if l != "" {
			s.WriteString("-" + strings.TrimSuffix(l, "\n") + "\n")
		}
	}
	for _, l := range bl[i : len(bl)-j] {
		if l != "" {
			s.WriteString("+" + strings.TrimSuffix(l, "\n") + "\n")
		}
	}
	return s.String()
}

// auditFiles describes a change set for the audit: hashes and the capped
// diff.
func auditFiles(changes []change, files *fileSet) (out []AuditFile, diff string, truncated bool) {
	var d strings.Builder
	for _, c := range changes {
		cur, _ := files.current(c.name)
		af := AuditFile{Name: c.name, Before: cur.sha}
		if !c.del {
			af.After = conffile.Hash(c.data)
		}
		out = append(out, af)
		d.WriteString(lineDiff(c.name, cur.data, c.data))
	}
	diff = d.String()
	if len(diff) > maxDiff {
		diff, truncated = diff[:maxDiff], true
	}
	return out, diff, truncated
}
