package scsi

import (
	"sync"
	"time"
)

// FakeDevice is an in-memory Device for tests. Respond decides each command's
// outcome: it fills dataIn (returning the byte count) or returns an error such
// as a *SenseError. Every CDB is recorded in order. A data-out command
// (DoOut) is passed to Respond with the data it sends, which is also
// recorded in DataOut.
type FakeDevice struct {
	mu      sync.Mutex
	CDBs    [][]byte
	DataOut [][]byte
	Respond func(cdb, data []byte) (int, error)
	Closed  bool
}

var (
	_ Device        = (*FakeDevice)(nil)
	_ DataOutDevice = (*FakeDevice)(nil)
)

// Do records cdb and delegates to Respond.
func (f *FakeDevice) Do(cdb, dataIn []byte, _ time.Duration) (int, []byte, error) {
	f.mu.Lock()
	f.CDBs = append(f.CDBs, append([]byte(nil), cdb...))
	f.mu.Unlock()
	if f.Respond == nil {
		return 0, nil, nil
	}
	n, err := f.Respond(cdb, dataIn)
	if se, ok := err.(*SenseError); ok {
		return n, se.Sense, err
	}
	return n, nil, err
}

// DoOut records cdb and dataOut and delegates to Respond.
func (f *FakeDevice) DoOut(cdb, dataOut []byte, _ time.Duration) ([]byte, error) {
	f.mu.Lock()
	f.CDBs = append(f.CDBs, append([]byte(nil), cdb...))
	f.DataOut = append(f.DataOut, append([]byte(nil), dataOut...))
	f.mu.Unlock()
	if f.Respond == nil {
		return nil, nil
	}
	_, err := f.Respond(cdb, append([]byte(nil), dataOut...))
	if se, ok := err.(*SenseError); ok {
		return se.Sense, err
	}
	return nil, err
}

// Close marks the fake closed.
func (f *FakeDevice) Close() error {
	f.mu.Lock()
	f.Closed = true
	f.mu.Unlock()
	return nil
}

// Recorded returns a copy of the CDBs issued so far.
func (f *FakeDevice) Recorded() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.CDBs...)
}

// FixedSense builds 18 bytes of fixed-format sense data for key/ASC/ASCQ.
func FixedSense(key, asc, ascq uint8) []byte {
	b := make([]byte, 18)
	b[0] = 0x70
	b[2] = key & 0x0f
	b[7] = 10
	b[12], b[13] = asc, ascq
	return b
}
