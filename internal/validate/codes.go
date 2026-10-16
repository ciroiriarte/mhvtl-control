package validate

// Finding codes. A code is "<family>.<check>" and stays the same across
// releases, so a client can act on it without parsing the message.
const (
	CodeAddrNoDriveLine = "address.no_drive_line"
	CodeAddrOrder       = "address.order"
	CodeAddrNumber      = "address.invalid_number"
	CodeAddrDuplicate   = "address.duplicate"
	CodeAddrOverflow    = "address.overflow"

	CodeIdentIDRange            = "identity.id_range"
	CodeIdentAddressRange       = "identity.address_range"
	CodeIdentIDDup              = "identity.id_duplicate"
	CodeIdentAddressDup         = "identity.address_duplicate"
	CodeIdentSerialMissing      = "identity.serial_missing"
	CodeIdentSerialLength       = "identity.serial_length"
	CodeIdentVendorLength       = "identity.vendor_length"
	CodeIdentProductLength      = "identity.product_length"
	CodeIdentSerialDup          = "identity.serial_duplicate"
	CodeIdentNAAInvalid         = "identity.naa_invalid"
	CodeIdentNAADerived         = "identity.naa_derived"
	CodeIdentNAADup             = "identity.naa_duplicate"
	CodeIdentSerialCut          = "identity.serial_cut"
	CodeIdentHostSerial         = "identity.host_serial"
	CodeIdentHostNAA            = "identity.host_naa"
	CodeIdentDriveLineSerial    = "identity.drive_line_serial"
	CodeIdentDriveLineIgnored   = "identity.drive_line_ignored"
	CodeIdentDriveLineNoDrive   = "identity.drive_line_no_drive"
	CodeIdentDriveLineSerialDup = "identity.drive_line_serial_duplicate"

	CodePresetUnmet      = "preset.unmet"
	CodePresetUnknownApp = "preset.unknown_app"

	CodeMediaNoHome      = "media.no_home"
	CodeMediaNotInHome   = "media.not_in_home"
	CodeMediaUnknownType = "media.unknown_type"
	CodeMediaDensity     = "media.density"
	CodeMediaNoDrives    = "media.no_drives"
	CodeMediaUnreadable  = "media.unreadable"

	CodeBarcodeLength      = "barcode.length"
	CodeBarcodeChars       = "barcode.chars"
	CodeBarcodeDup         = "barcode.duplicate"
	CodeBarcodePersistRead = "barcode.persist_unreadable"
	CodeBarcodeInPersist   = "barcode.in_persist"
	CodeBarcodeHeldLive    = "barcode.held_live"
	CodeBarcodeInOtherHome = "barcode.in_other_home"
	CodeBarcodeLoaded      = "barcode.loaded"

	CodeRemovalUnknownLibrary = "removal.unknown_library"
	CodeRemovalDispositionDup = "removal.disposition_duplicate"
	CodeRemovalNotLeaving     = "removal.not_leaving"
	CodeRemovalLeaves         = "removal.leaves"
	CodeRemovalSlotMissing    = "removal.slot_missing"
	CodeRemovalSlotTaken      = "removal.slot_taken"
	CodeRemovalSlotShared     = "removal.slot_shared"
	CodeRemovalDestination    = "removal.destination"
	CodeRemovalMAPFull        = "removal.map_full"
	CodeRemovalMoveBack       = "removal.move_back"

	CodeFileProblem = "file.problem"
)
