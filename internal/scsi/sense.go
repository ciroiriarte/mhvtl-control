package scsi

import "fmt"

// Sense keys (SPC-4 table 49) that callers branch on.
const (
	KeyNoSense        = 0x0
	KeyNotReady       = 0x2
	KeyMediumError    = 0x3
	KeyHardwareError  = 0x4
	KeyIllegalRequest = 0x5
	KeyUnitAttention  = 0x6
)

// ASC/ASCQ pairs a medium changer reports for MOVE MEDIUM, as (ASC<<8)|ASCQ.
// Values match mhvtl include/common/mhvtl_scsi.h.
const (
	ASCOffline             = 0x0412 // logical unit offline
	ASCManualIntervention  = 0x0403 // manual intervention required
	ASCInvalidElement      = 0x2101 // invalid element address
	ASCMediumNotPresent    = 0x3a00
	ASCMAPOpen             = 0x3a02 // medium not present - tray open (mhVTL: MAP open)
	ASCMediumDestFull      = 0x3b0d // medium destination element full
	ASCMediumSrcEmpty      = 0x3b0e // medium source element empty
	ASCMediumRemovalPrevnt = 0x5302 // medium removal prevented
)

var senseKeyNames = map[uint8]string{
	KeyNoSense:        "NO SENSE",
	0x1:               "RECOVERED ERROR",
	KeyNotReady:       "NOT READY",
	KeyMediumError:    "MEDIUM ERROR",
	KeyHardwareError:  "HARDWARE ERROR",
	KeyIllegalRequest: "ILLEGAL REQUEST",
	KeyUnitAttention:  "UNIT ATTENTION",
	0x7:               "DATA PROTECT",
	0xb:               "ABORTED COMMAND",
}

var ascNames = map[uint16]string{
	ASCOffline:             "logical unit offline",
	ASCManualIntervention:  "manual intervention required",
	ASCInvalidElement:      "invalid element address",
	ASCMediumNotPresent:    "medium not present",
	ASCMAPOpen:             "import/export element (MAP) open",
	ASCMediumDestFull:      "destination element full",
	ASCMediumSrcEmpty:      "source element empty",
	ASCMediumRemovalPrevnt: "medium removal prevented",
	0x2900:                 "power on or reset occurred",
	0x2a01:                 "mode parameters changed",
	0x3f03:                 "inquiry data changed",
	0x2000:                 "invalid command operation code",
	0x2400:                 "invalid field in CDB",
}

// SenseError is a CHECK CONDITION with decoded sense data.
type SenseError struct {
	Key  uint8
	ASC  uint8
	ASCQ uint8
	// Sense is the raw sense buffer as returned by the target.
	Sense []byte
}

// Code returns (ASC<<8)|ASCQ for comparison against the ASC* constants.
func (e *SenseError) Code() uint16 { return uint16(e.ASC)<<8 | uint16(e.ASCQ) }

func (e *SenseError) Error() string {
	key := senseKeyNames[e.Key]
	if key == "" {
		key = fmt.Sprintf("sense key 0x%x", e.Key)
	}
	if desc := ascNames[e.Code()]; desc != "" {
		return fmt.Sprintf("scsi: %s: %s (ASC/ASCQ %02x/%02x)", key, desc, e.ASC, e.ASCQ)
	}
	return fmt.Sprintf("scsi: %s (ASC/ASCQ %02x/%02x)", key, e.ASC, e.ASCQ)
}

// DecodeSense decodes fixed-format (70h/71h) sense data, and descriptor
// format (72h/73h) for targets that use it. Sense too short to carry a key
// still yields a *SenseError so a CHECK CONDITION is never mistaken for
// success.
func DecodeSense(b []byte) *SenseError {
	e := &SenseError{Sense: b}
	if len(b) == 0 {
		return e
	}
	switch b[0] & 0x7f {
	case 0x70, 0x71:
		if len(b) > 2 {
			e.Key = b[2] & 0x0f
		}
		if len(b) > 13 {
			e.ASC, e.ASCQ = b[12], b[13]
		}
	case 0x72, 0x73:
		if len(b) > 3 {
			e.Key, e.ASC, e.ASCQ = b[1]&0x0f, b[2], b[3]
		}
	}
	return e
}
