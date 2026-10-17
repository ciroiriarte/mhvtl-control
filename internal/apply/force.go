package apply

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mhvtl-control/internal/conffile"
	"mhvtl-control/internal/validate"
)

// Force tokens (plan 3.4, AC 9). A plan refused only for drive_loaded
// returns a token that lets the same change be applied with those drives
// unloaded into their slots. The token binds the change, the files it was
// made against, the units and the live placement: any of them differing
// at apply time voids it. It overrides drive_loaded and nothing else.
const (
	forceKeyFile = "force.key"
	nonceDir     = "nonces"
	forceTTL     = 120 * time.Second
	nonceLen     = 16
	tokenLen     = nonceLen + 8 + sha256.Size
)

// forceBinding are the values a force token is bound to, each recomputed
// from fresh reads when the token is checked.
type forceBinding struct {
	// Diff hashes the change: each file's name and new content (or its
	// deletion), and the dispositions.
	Diff string
	// Bases are the hashes the change was made against.
	Bases []string
	// Units are the derived units.
	Units []string
	// R1 hashes the live placement validation read.
	R1 string
	// Loaded is the sorted set of loaded drives the token overrides, as
	// READ ELEMENT STATUS and TEST UNIT READY found them (idle and
	// standalone drives included): a drive loaded since voids it.
	Loaded []string
}

// bindingOf computes the binding of req with units, live and the loaded
// drives.
func bindingOf(req *Request, units []string, live *validate.Live, loaded []string) forceBinding {
	d := sha256.New()
	var b forceBinding
	for _, c := range changesOf(req) {
		h := ""
		if !c.del {
			h = conffile.Hash(c.data)
		}
		writeField(d, c.name, strconv.FormatBool(c.del), h)
		b.Bases = append(b.Bases, c.base)
	}
	disp, _ := json.Marshal(req.Dispositions)
	writeField(d, string(disp))
	b.Diff = hex.EncodeToString(d.Sum(nil))
	b.Units = units
	r1, _ := json.Marshal(live) // map keys marshal sorted
	b.R1 = conffile.Hash(r1)
	b.Loaded = append([]string{}, loaded...)
	sort.Strings(b.Loaded)
	return b
}

// writeField writes each value length-prefixed, so no two different
// lists hash alike.
func writeField(w io.Writer, vals ...string) {
	for _, v := range vals {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(v)))
		w.Write(n[:])
		io.WriteString(w, v)
	}
}

func (b forceBinding) mac(key, nonce []byte, expiry uint64) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(nonce)
	binary.Write(m, binary.BigEndian, expiry)
	writeField(m, b.Diff)
	writeField(m, b.Bases...)
	writeField(m, strings.Join(b.Units, " "))
	writeField(m, b.R1)
	writeField(m, b.Loaded...)
	return m.Sum(nil)
}

// forceKey returns the HMAC key, RuntimeDir/force.key: 32 random bytes,
// created 0600 on first use. RuntimeDir is the helper's alone; the daemon
// cannot see it (plan 3.1), so it cannot mint tokens.
func (h *Helper) forceKey() ([]byte, error) {
	path := filepath.Join(h.cfg.RuntimeDir, forceKeyFile)
	for i := 0; i < 2; i++ {
		f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err == nil {
			key := make([]byte, 33)
			n, err := io.ReadFull(f, key)
			f.Close()
			if err != io.ErrUnexpectedEOF || n != 32 {
				return nil, errors.New(path + ": not a 32-byte key")
			}
			return key[:32], nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		// Written to a temporary file and linked into place, so a
		// concurrent reader never sees a short key; losing the race to
		// another helper reads its key instead.
		tmp := path + ".new." + hex.EncodeToString(key[:4])
		if err := os.WriteFile(tmp, key, 0o600); err != nil {
			return nil, err
		}
		err = os.Link(tmp, path)
		os.Remove(tmp)
		if err == nil {
			return key, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
	}
	return nil, errors.New(path + ": cannot create")
}

// mintForce returns a token for b, valid for forceTTL:
// base64(nonce ‖ expiry ‖ HMAC-SHA256(key, nonce ‖ expiry ‖ b)).
func (h *Helper) mintForce(b forceBinding) (string, time.Time, error) {
	key, err := h.forceKey()
	if err != nil {
		return "", time.Time{}, err
	}
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return "", time.Time{}, err
	}
	exp := h.cfg.Now().Add(forceTTL).Truncate(time.Second)
	tok := make([]byte, 0, tokenLen)
	tok = append(tok, nonce...)
	tok = binary.BigEndian.AppendUint64(tok, uint64(exp.Unix()))
	tok = append(tok, b.mac(key, nonce, uint64(exp.Unix()))...)
	return base64.RawURLEncoding.EncodeToString(tok), exp, nil
}

// checkForce accepts token for b once: it must decode, be unexpired, carry
// b's MAC, and its nonce must not have been used. The nonce is recorded
// with O_EXCL under RuntimeDir/nonces, named by its expiry so that expired
// ones are pruned here.
func (h *Helper) checkForce(token string, b forceBinding) error {
	nonce, expiry, err := h.verifyForce(token, b)
	if err != nil {
		return err
	}
	return h.useForce(nonce, expiry)
}

// verifyForce is checkForce short of recording the nonce: the token
// decodes, carries b's MAC and is unexpired.
func (h *Helper) verifyForce(token string, b forceBinding) (nonce []byte, expiry uint64, err error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != tokenLen {
		return nil, 0, refuse(ErrForce, "malformed force token")
	}
	nonce, expiry, mac := raw[:nonceLen], binary.BigEndian.Uint64(raw[nonceLen:nonceLen+8]), raw[nonceLen+8:]
	key, err := h.forceKey()
	if err != nil {
		return nil, 0, err
	}
	if !hmac.Equal(mac, b.mac(key, nonce, expiry)) {
		return nil, 0, refuse(ErrForce, "the force token does not match this change, its files, units or live placement; plan again")
	}
	if int64(expiry) <= h.cfg.Now().Unix() {
		return nil, 0, refuse(ErrForce, "the force token expired; plan again")
	}
	return nonce, expiry, nil
}

// useForce records a verified token's nonce, once.
func (h *Helper) useForce(nonce []byte, expiry uint64) error {
	now := h.cfg.Now().Unix()
	dir := filepath.Join(h.cfg.RuntimeDir, nonceDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	h.pruneNonces(dir, now)
	f, err := os.OpenFile(filepath.Join(dir, strconv.FormatUint(expiry, 10)+"-"+hex.EncodeToString(nonce)), os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return refuse(ErrForce, "the force token was already used; plan again")
	}
	if err != nil {
		return err
	}
	return f.Close()
}

// pruneNonces removes the nonces whose tokens have expired.
func (h *Helper) pruneNonces(dir string, now int64) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		exp, _, ok := strings.Cut(e.Name(), "-")
		if t, err := strconv.ParseInt(exp, 10, 64); ok && err == nil && t <= now {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
