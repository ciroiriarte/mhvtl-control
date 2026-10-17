package personality

import (
	"regexp"
	"strings"
)

// Load is how a drive loads a medium (LOAD_RW or LOAD_RO in mhVTL).
type Load bool

// Load capabilities.
const (
	RO Load = false
	RW Load = true
)

// Media is one add_drive_media_list() entry of a drive personality.
type Media struct {
	Name string // a media_handling name, e.g. "LTO6", "LTO6 WORM", "03592 JC"
	Load Load
}

// DriveProduct is a row of vtltape's tape_drives table: the product
// identification that selects a drive personality.
type DriveProduct struct {
	Product string // without the table's blank padding to 16 characters
	InitFn  string
	Source  string
}

// DriveModel is the media a drive personality's init function registers.
type DriveModel struct {
	InitFn string
	Source string // the init function's definition
	// WORM is whether the personality sets ssc_pm.set_WORM. vtltape refuses
	// to load a WORM cartridge in a drive whose personality does not
	// ("drive doesn't support WORM", usr/cmd/vtltape.c:1401-1405, upstream
	// master 852b78a), whatever its media list says.
	WORM  bool
	Media []Media
}

// DefaultDriveInit is the personality of a product missing from DriveProducts
// (vtltape.c drive_init).
const DefaultDriveInit = "init_default_ssc"

// DriveProducts is tape_drives[] in mhvtl usr/cmd/vtltape.c.
var DriveProducts = []DriveProduct{
	{Product: "ULT3580-TD1", InitFn: "init_ult3580_td1", Source: "usr/cmd/vtltape.c:139"},
	{Product: "ULT3580-TD2", InitFn: "init_ult3580_td2", Source: "usr/cmd/vtltape.c:140"},
	{Product: "ULT3580-TD3", InitFn: "init_ult3580_td3", Source: "usr/cmd/vtltape.c:141"},
	{Product: "ULT3580-TD4", InitFn: "init_ult3580_td4", Source: "usr/cmd/vtltape.c:142"},
	{Product: "ULT3580-TD5", InitFn: "init_ult3580_td5", Source: "usr/cmd/vtltape.c:143"},
	{Product: "ULT3580-TD6", InitFn: "init_ult3580_td6", Source: "usr/cmd/vtltape.c:144"},
	{Product: "ULT3580-TD7", InitFn: "init_ult3580_td7", Source: "usr/cmd/vtltape.c:145"},
	{Product: "ULT3580-HH7", InitFn: "init_ult3580_td7", Source: "usr/cmd/vtltape.c:146"},
	{Product: "ULT3580-TD8", InitFn: "init_ult3580_td8", Source: "usr/cmd/vtltape.c:147"},
	{Product: "ULT3580-HH8", InitFn: "init_ult3580_td8", Source: "usr/cmd/vtltape.c:148"},
	{Product: "ULT3580-TD9", InitFn: "init_ult3580_td9", Source: "usr/cmd/vtltape.c:149"},
	{Product: "ULT3580-HH9", InitFn: "init_ult3580_td9", Source: "usr/cmd/vtltape.c:150"},
	{Product: "ULT3580-TDA", InitFn: "init_ult3580_tda", Source: "usr/cmd/vtltape.c:151"},
	{Product: "ULT3580-HHA", InitFn: "init_ult3580_tda", Source: "usr/cmd/vtltape.c:152"},
	{Product: "ULTRIUM-TD1", InitFn: "init_ult3580_td1", Source: "usr/cmd/vtltape.c:153"},
	{Product: "ULTRIUM-TD2", InitFn: "init_ult3580_td2", Source: "usr/cmd/vtltape.c:154"},
	{Product: "ULTRIUM-HH2", InitFn: "init_ult3580_td2", Source: "usr/cmd/vtltape.c:155"},
	{Product: "ULTRIUM-TD3", InitFn: "init_ult3580_td3", Source: "usr/cmd/vtltape.c:156"},
	{Product: "ULTRIUM-HH3", InitFn: "init_ult3580_td3", Source: "usr/cmd/vtltape.c:157"},
	{Product: "ULTRIUM-TD4", InitFn: "init_ult3580_td4", Source: "usr/cmd/vtltape.c:158"},
	{Product: "ULTRIUM-HH4", InitFn: "init_ult3580_td4", Source: "usr/cmd/vtltape.c:159"},
	{Product: "ULTRIUM-TD5", InitFn: "init_ult3580_td5", Source: "usr/cmd/vtltape.c:160"},
	{Product: "ULTRIUM-HH5", InitFn: "init_ult3580_td5", Source: "usr/cmd/vtltape.c:161"},
	{Product: "ULTRIUM-TD6", InitFn: "init_ult3580_td6", Source: "usr/cmd/vtltape.c:162"},
	{Product: "ULTRIUM-HH6", InitFn: "init_ult3580_td6", Source: "usr/cmd/vtltape.c:163"},
	{Product: "ULTRIUM-TD7", InitFn: "init_ult3580_td7", Source: "usr/cmd/vtltape.c:164"},
	{Product: "ULTRIUM-HH7", InitFn: "init_ult3580_td7", Source: "usr/cmd/vtltape.c:165"},
	{Product: "ULTRIUM-TD8", InitFn: "init_ult3580_td8", Source: "usr/cmd/vtltape.c:166"},
	{Product: "ULTRIUM-HH8", InitFn: "init_ult3580_td8", Source: "usr/cmd/vtltape.c:167"},
	{Product: "ULTRIUM-TD9", InitFn: "init_ult3580_td9", Source: "usr/cmd/vtltape.c:168"},
	{Product: "ULTRIUM-HH9", InitFn: "init_ult3580_td9", Source: "usr/cmd/vtltape.c:169"},
	{Product: "ULTRIUM-TDA", InitFn: "init_ult3580_tda", Source: "usr/cmd/vtltape.c:170"},
	{Product: "ULTRIUM-HHA", InitFn: "init_ult3580_tda", Source: "usr/cmd/vtltape.c:171"},
	{Product: "Ultrium 1-SCSI", InitFn: "init_hp_ult_1", Source: "usr/cmd/vtltape.c:172"},
	{Product: "Ultrium 2-SCSI", InitFn: "init_hp_ult_2", Source: "usr/cmd/vtltape.c:173"},
	{Product: "Ultrium 3-SCSI", InitFn: "init_hp_ult_3", Source: "usr/cmd/vtltape.c:174"},
	{Product: "Ultrium 4-SCSI", InitFn: "init_hp_ult_4", Source: "usr/cmd/vtltape.c:175"},
	{Product: "Ultrium 5-SCSI", InitFn: "init_hp_ult_5", Source: "usr/cmd/vtltape.c:176"},
	{Product: "Ultrium 6-SCSI", InitFn: "init_hp_ult_6", Source: "usr/cmd/vtltape.c:177"},
	{Product: "Ultrium 7-SCSI", InitFn: "init_hp_ult_7", Source: "usr/cmd/vtltape.c:178"},
	{Product: "Ultrium 8-SCSI", InitFn: "init_hp_ult_8", Source: "usr/cmd/vtltape.c:179"},
	{Product: "SDX-300C", InitFn: "init_ait1_ssc", Source: "usr/cmd/vtltape.c:180"},
	{Product: "SDX-500C", InitFn: "init_ait2_ssc", Source: "usr/cmd/vtltape.c:181"},
	{Product: "SDX-500V", InitFn: "init_ait2_ssc", Source: "usr/cmd/vtltape.c:182"},
	{Product: "SDX-700C", InitFn: "init_ait3_ssc", Source: "usr/cmd/vtltape.c:183"},
	{Product: "SDX-700V", InitFn: "init_ait3_ssc", Source: "usr/cmd/vtltape.c:184"},
	{Product: "SDX-900V", InitFn: "init_ait4_ssc", Source: "usr/cmd/vtltape.c:185"},
	{Product: "03592J1A", InitFn: "init_3592_j1a", Source: "usr/cmd/vtltape.c:186"},
	{Product: "03592E05", InitFn: "init_3592_E05", Source: "usr/cmd/vtltape.c:187"},
	{Product: "03592E06", InitFn: "init_3592_E06", Source: "usr/cmd/vtltape.c:188"},
	{Product: "03592E07", InitFn: "init_3592_E07", Source: "usr/cmd/vtltape.c:189"},
	{Product: "T10000C", InitFn: "init_t10kC_ssc", Source: "usr/cmd/vtltape.c:190"},
	{Product: "T10000B", InitFn: "init_t10kB_ssc", Source: "usr/cmd/vtltape.c:191"},
	{Product: "T10000A", InitFn: "init_t10kA_ssc", Source: "usr/cmd/vtltape.c:192"},
	{Product: "T9840D", InitFn: "init_9840D_ssc", Source: "usr/cmd/vtltape.c:193"},
	{Product: "T9840C", InitFn: "init_9840C_ssc", Source: "usr/cmd/vtltape.c:194"},
	{Product: "T9840B", InitFn: "init_9840B_ssc", Source: "usr/cmd/vtltape.c:195"},
	{Product: "T9840A", InitFn: "init_9840A_ssc", Source: "usr/cmd/vtltape.c:196"},
	{Product: "T9940B", InitFn: "init_9940B_ssc", Source: "usr/cmd/vtltape.c:197"},
	{Product: "T9940A", InitFn: "init_9940A_ssc", Source: "usr/cmd/vtltape.c:198"},
	{Product: "DLT7000", InitFn: "init_dlt7000_ssc", Source: "usr/cmd/vtltape.c:199"},
	{Product: "DLT8000", InitFn: "init_dlt8000_ssc", Source: "usr/cmd/vtltape.c:200"},
	{Product: "SDLT 320", InitFn: "init_sdlt320_ssc", Source: "usr/cmd/vtltape.c:201"},
	{Product: "SDLT600", InitFn: "init_sdlt600_ssc", Source: "usr/cmd/vtltape.c:202"},
}

// DriveModels holds, per drive init function in usr/pm/*_pm.c, the media it
// adds in order. The LTO-7 and later IBM entries expand the ult_gen_media
// tables of ult3580_pm.c the way init_ult3580_lbp does.
var DriveModels = []DriveModel{
	{InitFn: "init_ait1_ssc", Source: "usr/pm/ait_pm.c:312", Media: []Media{
		{"AIT1", RW}, {"AIT1 Clean", RO},
	}},
	{InitFn: "init_ait2_ssc", Source: "usr/pm/ait_pm.c:350", Media: []Media{
		{"AIT1", RW}, {"AIT1 Clean", RO},
		{"AIT2", RW}, {"AIT2 Clean", RO},
	}},
	{InitFn: "init_ait3_ssc", Source: "usr/pm/ait_pm.c:392", Media: []Media{
		{"AIT1", RO}, {"AIT1 Clean", RO},
		{"AIT2", RW}, {"AIT2 Clean", RO},
		{"AIT3", RW}, {"AIT3 Clean", RO},
	}},
	{InitFn: "init_ait4_ssc", Source: "usr/pm/ait_pm.c:437", WORM: true, Media: []Media{
		{"AIT2", RO}, {"AIT2 Clean", RO},
		{"AIT3", RW}, {"AIT3 Clean", RO},
		{"AIT4", RW}, {"AIT4 Clean", RO}, {"AIT4 WORM", RW},
	}},
	{InitFn: "init_default_ssc", Source: "usr/pm/default_ssc_pm.c:387", WORM: true, Media: []Media{
		{"LTO1", RW},
		{"LTO2", RW},
		{"LTO3", RW},
		{"LTO4", RW},
		{"LTO5", RW},
		{"LTO6", RW},
		{"LTO7", RW},
		{"LTO8", RW},
		{"LTO9", RW},
		{"LTO10", RW},
		{"LTO10P", RW},
		{"DDS1", RW},
		{"DDS2", RW},
		{"DDS3", RW},
		{"DDS4", RW},
		{"DDS5", RW},
		{"DLT2", RW},
		{"DLT3", RW},
		{"DLT4", RW},
		{"SDLT", RW},
		{"SDLT 220", RW},
		{"SDLT 320", RW},
		{"SDLT 600", RW},
		{"T10KA", RW},
		{"T10KB", RW},
		{"T10KC", RW},
		{"T10KD", RW},
		{"9840A", RW},
		{"9840B", RW},
		{"9840C", RW},
		{"9840D", RW},
		{"9940A", RW},
		{"9940B", RW},
		{"AIT1", RW},
		{"AIT2", RW},
		{"AIT3", RW},
		{"AIT4", RW},
		{"03592 JA", RW},
		{"03592 JB", RW},
		{"03592 JC", RW},
	}},
	{InitFn: "init_hp_ult_1", Source: "usr/pm/hp_ultrium_pm.c:416", Media: []Media{
		{"LTO1", RW}, {"LTO1 Clean", RO},
	}},
	{InitFn: "init_hp_ult_2", Source: "usr/pm/hp_ultrium_pm.c:463", Media: []Media{
		{"LTO1", RW}, {"LTO1 Clean", RO},
		{"LTO2", RW}, {"LTO2 Clean", RO},
	}},
	{InitFn: "init_hp_ult_3", Source: "usr/pm/hp_ultrium_pm.c:514", WORM: true, Media: []Media{
		{"LTO1", RO}, {"LTO1 Clean", RO},
		{"LTO2", RW}, {"LTO2 Clean", RO},
		{"LTO3", RW}, {"LTO3 Clean", RO}, {"LTO3 WORM", RW},
	}},
	{InitFn: "init_hp_ult_4", Source: "usr/pm/hp_ultrium_pm.c:573", WORM: true, Media: []Media{
		{"LTO2", RO}, {"LTO2 Clean", RO},
		{"LTO3", RW}, {"LTO3 Clean", RO}, {"LTO3 WORM", RW},
		{"LTO4", RW}, {"LTO4 Clean", RO}, {"LTO4 WORM", RW},
	}},
	{InitFn: "init_hp_ult_5", Source: "usr/pm/hp_ultrium_pm.c:642", WORM: true, Media: []Media{
		{"LTO3", RO}, {"LTO3 Clean", RO},
		{"LTO4", RW}, {"LTO4 Clean", RO}, {"LTO4 WORM", RW}, {"LTO4 ENCR", RW},
		{"LTO5", RW}, {"LTO5 Clean", RO}, {"LTO5 WORM", RW}, {"LTO5 ENCR", RW},
	}},
	{InitFn: "init_hp_ult_6", Source: "usr/pm/hp_ultrium_pm.c:716", WORM: true, Media: []Media{
		{"LTO4", RO}, {"LTO4 Clean", RO}, {"LTO4 WORM", RO},
		{"LTO5", RW}, {"LTO5 Clean", RO}, {"LTO5 WORM", RW}, {"LTO5 ENCR", RW},
		{"LTO6", RW}, {"LTO6 Clean", RO}, {"LTO6 WORM", RW}, {"LTO6 ENCR", RW},
	}},
	{InitFn: "init_hp_ult_7", Source: "usr/pm/hp_ultrium_pm.c:791", WORM: true, Media: []Media{
		{"LTO5", RO}, {"LTO5 Clean", RO}, {"LTO5 WORM", RO},
		{"LTO6", RW}, {"LTO6 Clean", RO}, {"LTO6 WORM", RW}, {"LTO6 ENCR", RW},
		{"LTO7", RW}, {"LTO7 Clean", RO}, {"LTO7 WORM", RW}, {"LTO7 ENCR", RW},
	}},
	{InitFn: "init_hp_ult_8", Source: "usr/pm/hp_ultrium_pm.c:865", WORM: true, Media: []Media{
		{"LTO7", RW}, {"LTO7 Clean", RO}, {"LTO7 WORM", RW}, {"LTO7 ENCR", RW},
		{"LTO8", RW}, {"LTO8 Clean", RO}, {"LTO8 WORM", RW}, {"LTO8 ENCR", RW},
	}},
	{InitFn: "init_3592_j1a", Source: "usr/pm/ibm_03592_pm.c:364", WORM: true, Media: []Media{
		{"03592 JA", RW}, {"03592 JA Clean", RO}, {"03592 JA WORM", RW},
	}},
	{InitFn: "init_3592_E05", Source: "usr/pm/ibm_03592_pm.c:402", WORM: true, Media: []Media{
		{"03592 JA", RW}, {"03592 JA Clean", RO}, {"03592 JA WORM", RW},
		{"03592 JB", RW}, {"03592 JB Clean", RO}, {"03592 JB WORM", RW},
	}},
	{InitFn: "init_3592_E06", Source: "usr/pm/ibm_03592_pm.c:443", WORM: true, Media: []Media{
		{"03592 JA", RW}, {"03592 JA Clean", RO}, {"03592 JA WORM", RW},
		{"03592 JB", RW}, {"03592 JB Clean", RO}, {"03592 JB WORM", RW},
		{"03592 JC", RW}, {"03592 JC Clean", RO}, {"03592 JC WORM", RW},
	}},
	{InitFn: "init_3592_E07", Source: "usr/pm/ibm_03592_pm.c:493", WORM: true, Media: []Media{
		{"03592 JB", RW}, {"03592 JB Clean", RO}, {"03592 JB WORM", RW},
		{"03592 JC", RW}, {"03592 JC Clean", RO}, {"03592 JC WORM", RW},
		{"03592 JK", RW}, {"03592 JK Clean", RO}, {"03592 JK WORM", RW},
	}},
	{InitFn: "init_dlt7000_ssc", Source: "usr/pm/quantum_dlt_pm.c:329", Media: []Media{
		{"DLT3", RO},
		{"DLT4", RW}, {"DLT4 Clean", RO},
	}},
	{InitFn: "init_dlt8000_ssc", Source: "usr/pm/quantum_dlt_pm.c:380", Media: []Media{
		{"DLT3", RO},
		{"DLT4", RW}, {"DLT4 Clean", RO},
	}},
	{InitFn: "init_sdlt320_ssc", Source: "usr/pm/quantum_dlt_pm.c:432", Media: []Media{
		{"SDLT", RO},
		{"SDLT 220", RW},
		{"SDLT 320", RW}, {"SDLT 320 Clean", RO},
	}},
	{InitFn: "init_sdlt600_ssc", Source: "usr/pm/quantum_dlt_pm.c:485", WORM: true, Media: []Media{
		{"SDLT 220", RO},
		{"SDLT 320", RW}, {"SDLT 320 Clean", RO},
		{"SDLT 600", RW}, {"SDLT 600 Clean", RO}, {"SDLT 600 WORM", RW},
	}},
	{InitFn: "init_9840A_ssc", Source: "usr/pm/stk9x40_pm.c:343", WORM: true, Media: []Media{
		{"9840A", RW}, {"9840A Clean", RO},
	}},
	{InitFn: "init_9840B_ssc", Source: "usr/pm/stk9x40_pm.c:382", WORM: true, Media: []Media{
		{"9840A", RW}, {"9840A Clean", RO},
		{"9840B", RW}, {"9840B Clean", RO},
	}},
	{InitFn: "init_9840C_ssc", Source: "usr/pm/stk9x40_pm.c:424", WORM: true, Media: []Media{
		{"9840A", RO}, {"9840A Clean", RO},
		{"9840B", RW}, {"9840B Clean", RO},
		{"9840C", RW}, {"9840C Clean", RO},
	}},
	{InitFn: "init_9840D_ssc", Source: "usr/pm/stk9x40_pm.c:470", WORM: true, Media: []Media{
		{"9840B", RO}, {"9840B Clean", RO},
		{"9840C", RW}, {"9840C Clean", RO},
		{"9840D", RW}, {"9840D Clean", RO},
	}},
	{InitFn: "init_9940A_ssc", Source: "usr/pm/stk9x40_pm.c:513", WORM: true, Media: []Media{
		{"9940A", RW}, {"9940A Clean", RO},
	}},
	{InitFn: "init_9940B_ssc", Source: "usr/pm/stk9x40_pm.c:549", WORM: true, Media: []Media{
		{"9940A", RW}, {"9940A Clean", RO},
		{"9940B", RW}, {"9940B Clean", RO},
	}},
	{InitFn: "init_t10kA_ssc", Source: "usr/pm/t10000_pm.c:425", WORM: true, Media: []Media{
		{"T10KA", RW}, {"T10KA Clean", RO},
	}},
	{InitFn: "init_t10kB_ssc", Source: "usr/pm/t10000_pm.c:465", WORM: true, Media: []Media{
		{"T10KA", RW}, {"T10KA Clean", RO},
		{"T10KB", RW}, {"T10KB Clean", RO},
	}},
	{InitFn: "init_t10kC_ssc", Source: "usr/pm/t10000_pm.c:508", WORM: true, Media: []Media{
		{"T10KA", RW}, {"T10KA Clean", RO},
		{"T10KB", RW}, {"T10KB Clean", RO},
		{"T10KC", RW}, {"T10KC Clean", RO},
	}},
	{InitFn: "init_ult3580_td1", Source: "usr/pm/ult3580_pm.c:539", Media: []Media{
		{"LTO1", RW}, {"LTO1 Clean", RO},
	}},
	{InitFn: "init_ult3580_td2", Source: "usr/pm/ult3580_pm.c:588", Media: []Media{
		{"LTO1", RW}, {"LTO1 Clean", RO},
		{"LTO2", RW}, {"LTO2 Clean", RO},
	}},
	{InitFn: "init_ult3580_td3", Source: "usr/pm/ult3580_pm.c:639", WORM: true, Media: []Media{
		{"LTO1", RO}, {"LTO1 Clean", RO},
		{"LTO2", RW}, {"LTO2 Clean", RO},
		{"LTO3", RW}, {"LTO3 Clean", RO}, {"LTO3 WORM", RW},
	}},
	{InitFn: "init_ult3580_td4", Source: "usr/pm/ult3580_pm.c:699", WORM: true, Media: []Media{
		{"LTO2", RO}, {"LTO2 Clean", RO},
		{"LTO3", RW}, {"LTO3 Clean", RO}, {"LTO3 WORM", RW},
		{"LTO4", RW}, {"LTO4 Clean", RO}, {"LTO4 WORM", RW},
	}},
	{InitFn: "init_ult3580_td5", Source: "usr/pm/ult3580_pm.c:767", WORM: true, Media: []Media{
		{"LTO3", RO}, {"LTO3 Clean", RO},
		{"LTO4", RW}, {"LTO4 Clean", RO}, {"LTO4 WORM", RW}, {"LTO4 ENCR", RW},
		{"LTO5", RW}, {"LTO5 Clean", RO}, {"LTO5 WORM", RW}, {"LTO5 ENCR", RW},
	}},
	{InitFn: "init_ult3580_td6", Source: "usr/pm/ult3580_pm.c:843", WORM: true, Media: []Media{
		{"LTO4", RO}, {"LTO4 Clean", RO}, {"LTO4 WORM", RW},
		{"LTO5", RW}, {"LTO5 Clean", RO}, {"LTO5 WORM", RW}, {"LTO5 ENCR", RW},
		{"LTO6", RW}, {"LTO6 Clean", RO}, {"LTO6 WORM", RW}, {"LTO6 ENCR", RW},
	}},
	{InitFn: "init_ult3580_td7", Source: "usr/pm/ult3580_pm.c:1024", WORM: true, Media: []Media{
		{"LTO5", RO}, {"LTO5 Clean", RO}, {"LTO5 WORM", RW}, {"LTO5 ENCR", RW},
		{"LTO6", RW}, {"LTO6 Clean", RO}, {"LTO6 WORM", RW}, {"LTO6 ENCR", RW},
		{"LTO7", RW}, {"LTO7 Clean", RO}, {"LTO7 WORM", RW}, {"LTO7 ENCR", RW},
	}},
	{InitFn: "init_ult3580_td8", Source: "usr/pm/ult3580_pm.c:1034", WORM: true, Media: []Media{
		{"LTO7", RW}, {"LTO7 Clean", RO}, {"LTO7 WORM", RW}, {"LTO7 ENCR", RW},
		{"LTO8", RW}, {"LTO8 Clean", RO}, {"LTO8 WORM", RW}, {"LTO8 ENCR", RW}, {"LTO8 M8", RW},
	}},
	{InitFn: "init_ult3580_td9", Source: "usr/pm/ult3580_pm.c:1049", WORM: true, Media: []Media{
		{"LTO8", RW}, {"LTO8 Clean", RO}, {"LTO8 WORM", RW}, {"LTO8 ENCR", RW},
		{"LTO9", RW}, {"LTO9 Clean", RO}, {"LTO9 WORM", RW}, {"LTO9 ENCR", RW},
	}},
	{InitFn: "init_ult3580_tda", Source: "usr/pm/ult3580_pm.c:1062", WORM: true, Media: []Media{
		{"LTO10", RW}, {"LTO10 Clean", RO}, {"LTO10 WORM", RW}, {"LTO10 ENCR", RW},
		{"LTO10P", RW},
	}},
}

// DriveFor returns the drive personality vtltape gives a drive with this
// product identification. vtltape blank-pads the product to 16 characters and
// compares it whole with each tape_drives name (vtltape.c config_lu).
func DriveFor(product string) DriveModel {
	fn := DefaultDriveInit
	p := pad16(product)
	for _, d := range DriveProducts {
		if pad16(d.Product) == p {
			fn = d.InitFn
			break
		}
	}
	for _, m := range DriveModels {
		if m.InitFn == fn {
			return m
		}
	}
	return DriveModel{}
}

// pad16 is snprintf("%-16s") into a 16-byte product_id.
func pad16(s string) string {
	if len(s) >= 16 {
		return s[:16]
	}
	return s + strings.Repeat(" ", 16-len(s))
}

// Reads reports whether the drive loads cartridges of the media family name
// (a DensityMedia value) in any form: data, WORM, cleaning or encrypted.
func (m DriveModel) Reads(name string) bool {
	for _, md := range m.Media {
		if mediaFamily(md.Name) == name {
			return true
		}
	}
	return false
}

// mediaFamily strips the cartridge form from a media_handling name.
func mediaFamily(name string) string {
	for _, s := range []string{" Clean", " WORM", " ENCR", " M8"} {
		if f, ok := strings.CutSuffix(name, s); ok {
			return f
		}
	}
	return name
}

// DensityMedia maps a density name that mktape accepts (mhvtl usr/vtllib.c,
// the density branches before "is an invalid density") to the media_handling
// name of the same media type in the drive personalities.
var DensityMedia = map[string]string{
	"LTO1": "LTO1", "LTO2": "LTO2", "LTO3": "LTO3", "LTO4": "LTO4", "LTO5": "LTO5",
	"LTO6": "LTO6", "LTO7": "LTO7", "LTO8": "LTO8", "LTO9": "LTO9", "LTO10": "LTO10",
	"LTO10P": "LTO10P",
	"AIT1":   "AIT1", "AIT2": "AIT2", "AIT3": "AIT3", "AIT4": "AIT4",
	"DLT3": "DLT3", "DLT4": "DLT4",
	"SDLT1": "SDLT", "SDLT220": "SDLT 220", "SDLT320": "SDLT 320", "SDLT600": "SDLT 600",
	"9840A": "9840A", "9840B": "9840B", "9840C": "9840C", "9840D": "9840D",
	"9940A": "9940A", "9940B": "9940B",
	"T10KA": "T10KA", "T10KB": "T10KB", "T10KC": "T10KC",
	"DDS1": "DDS1", "DDS2": "DDS2", "DDS3": "DDS3", "DDS4": "DDS4",
	"J1A": "03592 JA", "E05": "03592 JB", "E06": "03592 JC", "E07": "03592 JK",
}

// barcodeRe is set_density's regex in mhvtl usr/cmd/make_vtl_media.in
// (:120, upstream master 852b78a). Like it, it is not anchored at the start:
// a longer barcode takes its media type from its tail, as make_vtl_media
// creates it. vtllibrary's 16-character barcode limit is checked elsewhere.
var barcodeRe = regexp.MustCompile(`[A-Z0-9]{6,8}([SLXTJDP])([123456789ABCHKTUVWXYZ])$`)

// BarcodeDensity is set_density from make_vtl_media: the density
// make_vtl_media passes to mktape for a pre-filled barcode, or "UNKNOWN".
func BarcodeDensity(barcode string) string {
	m := barcodeRe.FindStringSubmatch(barcode)
	if m == nil {
		return "UNKNOWN"
	}
	kind, c := m[1], m[2]
	switch kind {
	case "L":
		switch c {
		case "T":
			return "LTO3"
		case "U":
			return "LTO4"
		case "V":
			return "LTO5"
		case "W":
			return "LTO6"
		case "X":
			return "LTO7"
		case "Y":
			return "LTO8"
		case "Z":
			return "LTO9"
		case "A", "H":
			return "LTO10"
		}
		return "LTO" + c
	case "P":
		if c == "A" {
			return "LTO10P"
		}
	case "D":
		if c == "7" {
			return "DLT4"
		}
	case "S":
		switch c {
		case "3":
			return "SDLT600"
		case "2":
			return "SDLT320"
		case "1":
			return "SDLT220"
		}
		return "SDLT"
	case "J":
		switch c {
		case "A":
			return "J1A"
		case "B", "W", "X":
			return "E05"
		case "C":
			return "E06"
		case "Y", "K":
			return "E07"
		}
	case "X":
		return "AIT" + c
	case "T":
		switch c {
		case "Z":
			return "9840A"
		case "Y":
			return "9840B"
		case "X":
			return "9840C"
		case "W":
			return "9840D"
		case "V":
			return "9940A"
		case "U":
			return "9940B"
		}
		return "T10K" + c
	}
	return "UNKNOWN"
}

var (
	worm3592Re = regexp.MustCompile(`[A-Z0-9]{6,8}([J])([WXY])$`)
	wormLTORe  = regexp.MustCompile(`[A-Z0-9]{6,8}([L])([TUVWXYZH])$`)
)

// BarcodeMediaType is set_media_type from make_vtl_media: "data", "WORM" or
// "clean".
func BarcodeMediaType(barcode string) string {
	switch {
	case strings.HasPrefix(barcode, "W"):
		return "WORM"
	case strings.HasPrefix(barcode, "CLN"):
		return "clean"
	case worm3592Re.MatchString(barcode), wormLTORe.MatchString(barcode):
		return "WORM"
	}
	return "data"
}
