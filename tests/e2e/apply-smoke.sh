#!/bin/bash
# apply-smoke.sh: the Phase 3.5 VM smoke test of mhvtl-control-apply, the
# privileged configuration helper (plan Phase 3.5 and Phase 7 steps 1-7,
# through the helper's socket; the Phase 4 write API does not exist yet).
#
# Run it as root on a disposable VM with mhVTL running (libraries 10 and 30
# of mhVTL's sample device.conf), mhvtl-control installed and
# mhvtl-control.service active, mtx, lsscsi and jq installed, and the `ch`
# module blacklisted. It creates, changes and deletes library 40 and
# installs drop-ins under /run/systemd/system: never run it on a box that
# matters.
#
#   apply-smoke.sh [-o OUTDIR] [-c APPLYSMOKE] [SCENARIO...]
#
# Scenarios (default: all but the reboot pair, in this order):
#   enable           enable the helper socket and boot-recovery unit, hello
#   create           create library 40 (IBM 03584 unless LIB_VENDOR and
#                    LIB_PRODUCT say otherwise, 2x ULT3580-TD8, 20 slots,
#                    4 MAP slots, 16 pre-filled, global home)
#   change           add a drive and 10 slots to library 40
#   persist          the change with PERSIST: True, then toggled off
#   conflict         a hand edit between plan and apply
#   rollback         a start failure rolls back
#   rollback-persist the rollback with PERSIST: True and run-time moves
#   crash            the helper killed mid-apply, then runtime recovery
#   concurrency      hello during an apply; SIGTERM mid-apply
#   placement        mtx moves and a MAP export survive an apply
#   placement-force  a loaded drive under a force token
#   delete           delete library 40
#   reboot-prep      the helper killed mid-apply, then a reboot
#   reboot-check     after reboot-prep's reboot: boot recovery ran
#
# Each scenario prints ok/FAIL per check and PASS/FAIL at its end; the run
# ends with a summary and exits non-zero if any scenario failed. Requests
# and responses go to OUTDIR/<scenario>/; a failed scenario also gets
# OUTDIR/<scenario>/logs/ with the helper's journal, audit.jsonl, the
# configuration, unit states, lsscsi and mtx status.
#
# Test-only technique: the helper serves only processes in
# mhvtl-control.service's cgroup (internal/apply/peer.go), so the client
# runs from a shell that first writes its own PID to that cgroup's
# cgroup.procs, then execs the client (see client below).
set -euo pipefail
export LC_ALL=C

CONF=/etc/mhvtl
STATE_DIR=/var/lib/mhvtl-control-apply
CGROUP_PROCS=/sys/fs/cgroup/system.slice/mhvtl-control.service/cgroup.procs
SMOKE_STATE=/var/tmp/apply-smoke/state
APPLYSMOKE=${APPLYSMOKE:-/usr/local/libexec/applysmoke}
OUT=/var/tmp/apply-smoke/run-$(date +%Y%m%d-%H%M%S)
LIB=40
# Library 40's changer; LIB_VENDOR=STK LIB_PRODUCT=L700 selects another
# personality.
LIB_VENDOR=${LIB_VENDOR:-IBM}
LIB_PRODUCT=${LIB_PRODUCT:-03584L32}
MARKER="# apply-smoke: library $LIB"

ALL=(enable create change persist conflict rollback rollback-persist crash concurrency placement placement-force delete)

usage() { sed -n '2,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//'; exit 2; }

while getopts o:c:h opt; do
	case $opt in
	o) OUT=$OPTARG ;;
	c) APPLYSMOKE=$OPTARG ;;
	*) usage ;;
	esac
done
shift $((OPTIND - 1))
SCENARIOS=("$@")
[ ${#SCENARIOS[@]} -gt 0 ] || SCENARIOS=("${ALL[@]}")

HOME_PATH=$(sed -n 's/^MHVTL_HOME_PATH=//p' "$CONF/mhvtl.conf" | tail -1)
HOME_PATH=${HOME_PATH:-/var/lib/mhvtl}

log() { echo "$*" >&2; }

# --- the client --------------------------------------------------------------

# client runs applysmoke from mhvtl-control.service's cgroup: the shell
# moves itself there, then execs the client, so the helper's peer check sees
# the client's PID in that cgroup.
client() {
	sh -c 'echo $$ >"$1" && shift && exec "$@"' sh "$CGROUP_PROCS" "$APPLYSMOKE" "$@"
}

# send OP REQUEST NAME [send flags...]: sends REQUEST (or nothing for
# "-") with OP and saves the response as $STEP_DIR/NAME.json.
send() {
	local op=$1 req=$2 name=$3
	shift 3
	local args=(send -op "$op" "$@")
	[ "$req" = - ] || args+=("$req")
	client "${args[@]}" >"$STEP_DIR/$name.json" 2>"$STEP_DIR/$name.err"
}

# mkreq NAME [request flags...] FILE...: builds a request against $CONF.
mkreq() {
	local name=$1
	shift
	"$APPLYSMOKE" request -config-dir "$CONF" -actor "apply-smoke $STEP" "$@" >"$STEP_DIR/$name.req.json"
	echo "$STEP_DIR/$name.req.json"
}

# --- checks ------------------------------------------------------------------

STEP='' STEP_DIR='' FAILS=0

check() {
	local desc=$1
	shift
	echo "--- $desc: $*" >>"$STEP_DIR/checks.log"
	if "$@" >>"$STEP_DIR/checks.log" 2>&1; then
		log "  ok   $desc"
	else
		log "  FAIL $desc"
		FAILS=$((FAILS + 1))
	fi
}

# jqis FILE FILTER VALUE: jq -r FILTER of FILE is VALUE.
jqis() {
	local got
	got=$(jq -r "$2" "$1")
	[ "$got" = "$3" ] || { echo "$1: $2 is '$got', want '$3'"; return 1; }
}

# same A B: files A and B are equal (diff shown in checks.log).
same() { diff -u "$1" "$2"; }

eq() { [ "$1" = "$2" ] || { echo "'$1' != '$2'"; return 1; }; }
differs() { [ "$1" != "$2" ] || { echo "both '$1'"; return 1; }; }

sha() { sha256sum "$1" | cut -d' ' -f1; }

# wait_for SECONDS CMD...: polls CMD every 0.2 s.
wait_for() {
	local deadline=$((SECONDS + $1))
	shift
	until "$@"; do
		[ $SECONDS -lt $deadline ] || return 1
		sleep 0.2
	done
}

# --- mhVTL -------------------------------------------------------------------

# lib_sg N: the sg node of library N, the medium changer at the C:T:L
# device.conf gives (the node changes on every restart, so it is never
# cached). The type matters: library 10's 0:0:0 is also the address of the
# VM's disk and CD-ROM on their own hosts.
lib_sg() {
	local h
	h=$(grep -E "^Library: $1 " "$CONF/device.conf") || return 1
	set -- $h
	lsscsi -g | awk -v a=":$((10#$4)):$((10#$6)):$((10#$8))]" '$2 == "mediumx" && index($1, a) { print $NF }'
}
lib_present() { [ -n "$(lib_sg "$1" 2>/dev/null)" ]; }

# mtx_status N FILE: mtx status of library N, retried for the unit
# attention after a restart and for a library still coming online.
mtx_status() {
	local sg
	for _ in $(seq 1 20); do
		sg=$(lib_sg "$1" 2>/dev/null || true)
		if [ -n "$sg" ] && mtx -f "$sg" status >"$2" 2>&1; then
			return 0
		fi
		sleep 1
	done
	cat "$2" >&2
	return 1
}
online() { mtx_status "$1" "$STEP_DIR/online.$1.mtx"; }

mtx_do() {
	local sg
	sg=$(lib_sg "$1")
	shift
	mtx -f "$sg" "$@"
}

# Element numbers mtx uses, from a status file.
mtx_slots() { # FILE Full|Empty
	awk -v s="$2" '/^ *Storage Element [0-9]+:/ { split($3, a, ":"); if ($0 ~ ":" s) print a[1] }' "$1"
}
mtx_maps() { # FILE Full|Empty
	awk -v s="$2" '/IMPORT\/EXPORT:/ { if ($0 ~ "EXPORT:" s) print $3 }' "$1"
}
mtx_counts() { # FILE -> "<drives> <slots> <maps>"
	echo "$(grep -c '^Data Transfer Element' "$1") $(grep -Ec '^ *Storage Element [0-9]+:' "$1") $(grep -c 'IMPORT/EXPORT' "$1")"
}
# barcode of mtx storage element E.
mtx_barcode() { awk -v e="$2" '$1 == "Storage" && $3 ~ "^" e "[: ]" { sub(/.*VolumeTag=/, ""); print $1 }' "$1"; }

placement() { "$APPLYSMOKE" placement "$@"; }

# unload_all N: puts every loaded drive's cartridge back in its slot.
unload_all() {
	local f=$STEP_DIR/unload.$1.mtx d
	mtx_status "$1" "$f"
	local src
	for d in $(awk -F'[ :]' '/^Data Transfer Element [0-9]+:Full/ { print $4 }' "$f"); do
		src=$(sed -n "s/^Data Transfer Element $d:Full (Storage Element \([0-9]*\) Loaded).*/\1/p" "$f")
		[ -n "$src" ] || src=$(mtx_slots "$f" Empty | head -1)
		mtx_do "$1" unload "$src" "$d"
	done
}

# Units and their PIDs.
unit_pids() {
	local u
	for u in $(systemctl list-units --type=service --state=active --no-legend --plain 'vtllibrary@*' 'vtltape@*' | awk '{ print $1 }' | sort); do
		echo "$u $(systemctl show -p MainPID --value "$u")"
	done
}
pid_in() { awk -v u="$1" '$1 == u { print $2 }' "$2"; }
# pids_kept BEFORE AFTER UNIT...: every unit of BEFORE but the ones named
# has the same PID in AFTER.
pids_kept() {
	local before=$1 after=$2
	shift 2
	local skip=" $* " u p bad=0
	while read -r u p; do
		case $skip in *" $u "*) continue ;; esac
		if ! grep -qx "$u $p" "$after"; then
			echo "$u: PID $p, now $(grep "^$u " "$after" || echo gone)"
			bad=1
		fi
	done <"$before"
	return $bad
}
# others_kept: the units of every library but 40 (and of standalone
# drives) still have the PIDs the enable scenario recorded.
others_kept() {
	local f=$SMOKE_STATE/pids.enable
	[ -s "$f" ] || { log "  (no PIDs from the enable scenario; skipping)"; return 0; }
	unit_pids >"$STEP_DIR/pids.now"
	check "PIDs of units outside library $LIB unchanged since enable" pids_kept \
		<(grep -Ev "@$((LIB / 10))[0-9]\.service " "$f") "$STEP_DIR/pids.now"
}
active() { systemctl is-active --quiet "$@"; }
lib_units() { # N -> vtllibrary@N.service and its drives' units
	echo "vtllibrary@$1.service"
	awk -v lib="$1" '/^Drive: / { d = $2 } /^ Library ID: / && $3 == lib { print "vtltape@" d ".service" }' "$CONF/device.conf"
}
lib_drives() { lib_units "$1" | grep -c vtltape; }

helper_instance() {
	systemctl list-units --no-legend --plain --state=active 'mhvtl-control-apply@*' | awk '{ print $1 }'
}

# --- library 40 --------------------------------------------------------------

drive_stanza() { # ID SLOT
	local t=$((16 + $1 - LIB))
	cat <<EOF

Drive: $1 CHANNEL: 00 TARGET: $t LUN: 00
 Library ID: $LIB Slot: $(printf %02d "$2")
 Vendor identification: IBM
 Product identification: ULT3580-TD8
 Product revision level: HB81
 Unit serial number: XYZZY_$1
 NAA: 40:22:33:44:ab:00:$t:00
 Compression: factor 1 enabled 1
 Compression type: lzo
 Backoff: 400
EOF
}

lib_stanza() {
	cat <<EOF
$MARKER, removed by the delete scenario

Library: $LIB CHANNEL: 00 TARGET: 16 LUN: 00
 Vendor identification: $LIB_VENDOR
 Product identification: $LIB_PRODUCT
 Unit serial number: XYZZY_$LIB
 NAA: 40:22:33:44:ab:00:16:00
 PERSIST: False
 Backoff: 400
EOF
	drive_stanza $((LIB + 1)) 1
	drive_stanza $((LIB + 2)) 2
}

BARCODES=$(seq -f "F400%02gL8" 1 16)

lib_contents() {
	local n
	printf 'VERSION: 2\n\nDrive 1:\nDrive 2:\n\nPicker 1:\n\nMAP 1:\nMAP 2:\nMAP 3:\nMAP 4:\n\n'
	echo "# apply-smoke: slots 1-16 pre-filled, 17-20 empty"
	for n in $(seq 1 20); do
		if [ "$n" -le 16 ]; then
			printf 'Slot %d: F400%02dL8\n' "$n" "$n"
		else
			printf 'Slot %d:\n' "$n"
		fi
	done
}

# plan_apply NAME REQUEST: plan, check it validated, apply with the plan's
# units; the apply response is $STEP_DIR/NAME.apply.json.
plan_apply() {
	send plan "$2" "$1.plan"
	check "$1: plan validated" jqis "$STEP_DIR/$1.plan.json" .result validated
	send apply "$2" "$1.apply" -units-from "$STEP_DIR/$1.plan.json"
}

create_lib() {
	mkdir -p "$STEP_DIR/new"
	{ cat "$CONF/device.conf"; lib_stanza; } >"$STEP_DIR/new/device.conf"
	lib_contents >"$STEP_DIR/new/library_contents.$LIB"
	plan_apply create "$(mkreq create "$STEP_DIR/new/device.conf" "$STEP_DIR/new/library_contents.$LIB")"
	jqis "$STEP_DIR/create.apply.json" .result applied
}

ensure_lib() {
	if ! grep -q "^Library: $LIB " "$CONF/device.conf"; then
		log "  (creating library $LIB)"
		create_lib
	fi
	unload_all "$LIB"
}

# grow NAME SLOTS [DRIVES]: a request for library 40 with SLOTS more slots
# (and DRIVES more drives), next built from the live view.
grow() {
	local name=$1 add=$2 adddrives=${3:-0} d n slots drives
	mkdir -p "$STEP_DIR/$name"
	mtx_status "$LIB" "$STEP_DIR/$name/live.mtx"
	read -r drives slots _ <<<"$(mtx_counts "$STEP_DIR/$name/live.mtx")"
	cp "$CONF/device.conf" "$STEP_DIR/$name/device.conf"
	n=$(lib_drives "$LIB")
	for d in $(seq 1 "$adddrives"); do
		drive_stanza $((LIB + n + d)) $((n + d)) >>"$STEP_DIR/$name/device.conf"
	done
	"$APPLYSMOKE" contents -from "$CONF/library_contents.$LIB" -mtx "$STEP_DIR/$name/live.mtx" \
		-slots $((slots + add)) -drives $((drives + adddrives)) >"$STEP_DIR/$name/library_contents.$LIB"
	mkreq "$name" "$STEP_DIR/$name/device.conf" "$STEP_DIR/$name/library_contents.$LIB"
}

# Drop-ins on vtllibrary@40 that delay or fail its start.
DROPIN_DIR=/run/systemd/system/vtllibrary@$LIB.service.d
dropin() { # NAME EXECSTARTPRE
	mkdir -p "$DROPIN_DIR"
	printf '[Service]\nExecStartPre=%s\n' "$2" >"$DROPIN_DIR/$1.conf"
	systemctl daemon-reload
}
undropin() {
	rm -f "$DROPIN_DIR/$1.conf"
	systemctl daemon-reload
}
lib_activating() { [ "$(systemctl show -p ActiveState --value "vtllibrary@$LIB.service")" = activating ]; }
lib_settled() { ! lib_activating; }

# the placement of a library_contents, and of mtx status without drives.
file_placement() { placement -contents "$1"; }
mtx_placement() { placement -mtx "$1" | grep -v '^Drive ' || true; }
# other_lines FILE: library_contents without its Slot and MAP lines.
other_lines() { grep -Ev '^(Slot|MAP) [0-9]+:' "$1" || true; }

# --- scenarios ---------------------------------------------------------------

s_enable() {
	systemctl enable --now mhvtl-control-apply.socket
	systemctl enable mhvtl-control-apply-recover.service
	check "socket active" active mhvtl-control-apply.socket
	check "recover unit enabled" systemctl is-enabled --quiet mhvtl-control-apply-recover.service
	check "mhvtl-control.service active" active mhvtl-control.service
	send hello - hello
	check "hello: ready" jqis "$STEP_DIR/hello.json" .state ready
	unit_pids | tee "$STEP_DIR/pids" >"$SMOKE_STATE/pids.enable"
	check "units recorded" test -s "$STEP_DIR/pids"
}

s_create() {
	if grep -q "^Library: $LIB " "$CONF/device.conf"; then
		log "  library $LIB exists; deleting it first"
		s_delete_quiet
	fi
	unit_pids >"$STEP_DIR/pids.before"
	mkdir -p "$STEP_DIR/new"
	{ cat "$CONF/device.conf"; lib_stanza; } >"$STEP_DIR/new/device.conf"
	lib_contents >"$STEP_DIR/new/library_contents.$LIB"
	local req
	req=$(mkreq create "$STEP_DIR/new/device.conf" "$STEP_DIR/new/library_contents.$LIB")
	plan_apply create "$req"
	check "plan units" jqis "$STEP_DIR/create.plan.json" '.units | sort | join(" ")' \
		"vtllibrary@$LIB.service vtltape@$((LIB + 1)).service vtltape@$((LIB + 2)).service"
	check "apply: applied" jqis "$STEP_DIR/create.apply.json" .result applied
	local b missing=0
	for b in $BARCODES; do
		[ -e "$HOME_PATH/$b" ] || { echo "missing $HOME_PATH/$b" >>"$STEP_DIR/checks.log"; missing=1; }
	done
	check "pre-filled cartridges exist in $HOME_PATH" eq "$missing" 0
	check "mtx status works" mtx_status "$LIB" "$STEP_DIR/after.mtx"
	check "mtx: 2 drives, 20 slots, 4 MAP" eq "$(mtx_counts "$STEP_DIR/after.mtx")" "2 20 4"
	check "mtx load 1 0" mtx_do "$LIB" load 1 0
	check "mtx unload 1 0" mtx_do "$LIB" unload 1 0
	lsscsi -g >"$STEP_DIR/lsscsi"
	local t
	for t in 16 17 18; do
		check "lsscsi lists 0:$t:0" grep -q ":0:$t:0]" "$STEP_DIR/lsscsi"
	done
	unit_pids >"$STEP_DIR/pids.after"
	check "other units' PIDs unchanged" pids_kept "$STEP_DIR/pids.before" "$STEP_DIR/pids.after"
	check "library $LIB units active" active $(lib_units "$LIB")
}

s_change() {
	ensure_lib
	unit_pids >"$STEP_DIR/pids.before"
	local n new req
	n=$(lib_drives "$LIB")
	new=vtltape@$((LIB + n + 1)).service
	req=$(grow change 10 1)
	read -r _ slots0 _ <<<"$(mtx_counts "$STEP_DIR/change/live.mtx")"
	plan_apply change "$req"
	check "plan units: library and new drive only" jqis "$STEP_DIR/change.plan.json" '.units | sort | join(" ")' \
		"vtllibrary@$LIB.service $new"
	check "apply: applied" jqis "$STEP_DIR/change.apply.json" .result applied
	unit_pids >"$STEP_DIR/pids.after"
	check "vtllibrary@$LIB PID changed" differs "$(pid_in "vtllibrary@$LIB.service" "$STEP_DIR/pids.before")" \
		"$(pid_in "vtllibrary@$LIB.service" "$STEP_DIR/pids.after")"
	check "$new active" active "$new"
	check "every other PID unchanged" pids_kept "$STEP_DIR/pids.before" "$STEP_DIR/pids.after" "vtllibrary@$LIB.service"
	check "mtx status works" mtx_status "$LIB" "$STEP_DIR/after.mtx"
	check "mtx: one more drive, 10 more slots" eq "$(mtx_counts "$STEP_DIR/after.mtx")" "$((n + 1)) $((slots0 + 10)) 4"
	check "placement unchanged" same <(mtx_placement "$STEP_DIR/change/live.mtx") <(mtx_placement "$STEP_DIR/after.mtx")
	others_kept
}

# persist_round NAME VALUE: sets PERSIST to VALUE on library 40, moves two
# cartridges, adds 5 slots, and checks that both the moves and the new slot
# count are live and in library_contents.
# set_persist NAME VALUE: applies PERSIST: VALUE to library 40.
set_persist() {
	local name=$1 value=$2 req
	mkdir -p "$STEP_DIR/$name.set"
	sed "/^Library: $LIB /,/^\$/ s/^ PERSIST: .*/ PERSIST: $value/" "$CONF/device.conf" >"$STEP_DIR/$name.set/device.conf"
	req=$(mkreq "$name.set" "$STEP_DIR/$name.set/device.conf")
	plan_apply "$name.set" "$req"
	check "$name: PERSIST: $value applied" jqis "$STEP_DIR/$name.set.apply.json" .result applied
}

# move_two NAME: moves the first two full slots of library 40 to the first
# two empty ones.
move_two() {
	local full empty
	mtx_status "$LIB" "$STEP_DIR/$1.pre-move.mtx"
	mapfile -t full < <(mtx_slots "$STEP_DIR/$1.pre-move.mtx" Full | head -2)
	mapfile -t empty < <(mtx_slots "$STEP_DIR/$1.pre-move.mtx" Empty | head -2)
	mtx_do "$LIB" transfer "${full[0]}" "${empty[0]}"
	mtx_do "$LIB" transfer "${full[1]}" "${empty[1]}"
}

persist_round() {
	local name=$1 value=$2 req f slots0
	set_persist "$name" "$value"
	move_two "$name"
	req=$(grow "$name" 5)
	read -r _ slots0 _ <<<"$(mtx_counts "$STEP_DIR/$name/live.mtx")"
	plan_apply "$name" "$req"
	check "$name: slots applied" jqis "$STEP_DIR/$name.apply.json" .result applied
	f=$CONF/library_contents.$LIB.persist
	check "$name: no .persist left to win at the start" test ! -e "$f"
	check "$name: mtx status works" mtx_status "$LIB" "$STEP_DIR/$name.after.mtx"
	check "$name: 5 more slots live" eq "$(mtx_counts "$STEP_DIR/$name.after.mtx" | cut -d' ' -f2)" "$((slots0 + 5))"
	check "$name: moved placements live" same <(mtx_placement "$STEP_DIR/$name/live.mtx") <(mtx_placement "$STEP_DIR/$name.after.mtx")
	check "$name: moved placements in library_contents" same <(mtx_placement "$STEP_DIR/$name/live.mtx") <(file_placement "$CONF/library_contents.$LIB")
}

s_persist() {
	ensure_lib
	persist_round persist-on True
	persist_round persist-off False
	others_kept
}

s_conflict() {
	ensure_lib
	unit_pids >"$STEP_DIR/pids.before"
	cp -p "$CONF/device.conf" "$STEP_DIR/device.conf.orig"
	mkdir -p "$STEP_DIR/new"
	{ cat "$CONF/device.conf"; echo "# apply-smoke: conflict probe"; } >"$STEP_DIR/new/device.conf"
	local req hand
	req=$(mkreq conflict "$STEP_DIR/new/device.conf")
	send plan "$req" conflict.plan
	check "plan validated" jqis "$STEP_DIR/conflict.plan.json" .result validated
	echo "# apply-smoke: hand edit" >>"$CONF/device.conf"
	hand=$(sha "$CONF/device.conf")
	send apply "$req" conflict.apply -units-from "$STEP_DIR/conflict.plan.json"
	check "apply: conflict (409)" jqis "$STEP_DIR/conflict.apply.json" .result conflict
	check "conflict names device.conf" jqis "$STEP_DIR/conflict.apply.json" '.file | endswith("device.conf")' true
	check "conflict reports the current hash" jqis "$STEP_DIR/conflict.apply.json" .current_sha256 "$hand"
	check "device.conf left as hand-edited" eq "$(sha "$CONF/device.conf")" "$hand"
	unit_pids >"$STEP_DIR/pids.after"
	check "no unit restarted" same "$STEP_DIR/pids.before" "$STEP_DIR/pids.after"
	cp -p "$STEP_DIR/device.conf.orig" "$CONF/device.conf"
}

# snapshot NAME: device.conf hash, library_contents.40 and its live
# placement, before an apply that should end where it started.
snapshot() {
	sha "$CONF/device.conf" >"$1.devconf.sha"
	cp "$CONF/library_contents.$LIB" "$1.contents"
	mtx_status "$LIB" "$1.mtx"
}
# restored NAME: checks the configuration and placement are SNAPSHOT's.
restored() {
	local what=$1 snap=$2
	check "$what: device.conf sha256 unchanged" eq "$(sha "$CONF/device.conf")" "$(cat "$snap.devconf.sha")"
	check "$what: library_contents.$LIB placement is the original's" \
		same <(mtx_placement "$snap.mtx") <(file_placement "$CONF/library_contents.$LIB")
	check "$what: library_contents.$LIB other lines unchanged" \
		same <(other_lines "$snap.contents") <(other_lines "$CONF/library_contents.$LIB")
	check "$what: units active" active $(lib_units "$LIB")
	check "$what: library $LIB online" mtx_status "$LIB" "$snap.after.mtx"
	check "$what: live placement unchanged" same <(mtx_placement "$snap.mtx") <(mtx_placement "$snap.after.mtx")
}

# rollback_round NAME: an apply of 2 more slots whose first start of
# vtllibrary@40 fails rolls back to the snapshot taken just before.
rollback_round() {
	local name=$1 req
	snapshot "$STEP_DIR/$name.pre"
	# Fails the first start only: the rollback's own start of the library
	# must succeed for the rollback to be checked.
	rm -f /run/apply-smoke-fail-once
	dropin fail "/bin/sh -c 'test -e /run/apply-smoke-fail-once || { touch /run/apply-smoke-fail-once; exit 1; }'"
	req=$(grow "$name" 2)
	plan_apply "$name" "$req"
	undropin fail
	check "$name: apply rolled_back" jqis "$STEP_DIR/$name.apply.json" .result rolled_back
	check "$name: failed unit is vtllibrary@$LIB" jqis "$STEP_DIR/$name.apply.json" .failed_unit "vtllibrary@$LIB.service"
	restored "$name" "$STEP_DIR/$name.pre"
	check "$name: no intent left" test ! -e "$STATE_DIR/intent.json"
}

s_rollback() {
	ensure_lib
	rollback_round rollback
	others_kept
}

# The rollback with PERSIST: True and cartridges moved at run time: the
# stops write .persist files, so the restored library_contents.40 must be
# what the library loads, with no .persist left to win at its start.
s_rollback_persist() {
	ensure_lib
	set_persist persist-on True
	move_two moved
	rollback_round rollback
	check "no .persist left after the rollback" test ! -e "$CONF/library_contents.$LIB.persist"
	set_persist persist-off False
	others_kept
}

# kill_mid_apply NAME REQUEST: starts an apply, waits until the library's
# start hangs in the sleep drop-in, and SIGKILLs the helper instance.
kill_mid_apply() {
	local name=$1 req=$2 bg unit
	send plan "$req" "$name.plan"
	check "$name: plan validated" jqis "$STEP_DIR/$name.plan.json" .result validated
	send apply "$req" "$name.apply" -units-from "$STEP_DIR/$name.plan.json" &
	bg=$!
	check "$name: apply reached the start of vtllibrary@$LIB" wait_for 300 lib_activating
	unit=$(helper_instance)
	echo "$unit" >"$STEP_DIR/$name.instance"
	check "$name: one helper instance" eq "$(echo "$unit" | wc -w)" 1
	systemctl kill -s KILL "$unit"
	wait "$bg" || true
	check "$name: intent.json left" test -e "$STATE_DIR/intent.json"
}

s_crash() {
	ensure_lib
	snapshot "$STEP_DIR/pre"
	dropin sleep "/bin/sleep 30"
	kill_mid_apply crash "$(grow crash 2)"
	undropin sleep
	check "start job settled" wait_for 120 lib_settled
	local before after
	before=$(sha256sum "$CONF/device.conf" "$CONF/library_contents.$LIB" "$STATE_DIR/intent.json")
	send hello - hello
	check "hello: incomplete_apply" jqis "$STEP_DIR/hello.json" .state incomplete_apply
	after=$(sha256sum "$CONF/device.conf" "$CONF/library_contents.$LIB" "$STATE_DIR/intent.json")
	check "hello changed nothing" eq "$before" "$after"
	send plan "$STEP_DIR/crash.req.json" recover.plan
	check "next plan: recovered" jqis "$STEP_DIR/recover.plan.json" .result recovered
	check "intent removed" test ! -e "$STATE_DIR/intent.json"
	restored recovery "$STEP_DIR/pre"
	local req
	req=$(grow again 2)
	plan_apply again "$req"
	check "a second apply succeeds" jqis "$STEP_DIR/again.apply.json" .result applied
	others_kept
}

s_concurrency() {
	ensure_lib
	local recover0 req bg unit
	recover0=$(systemctl show -p InactiveExitTimestampMonotonic --value mhvtl-control-apply-recover.service)
	dropin sleep "/bin/sleep 10"
	req=$(grow live 1)
	send plan "$req" live.plan
	send apply "$req" live.apply -units-from "$STEP_DIR/live.plan.json" &
	bg=$!
	check "apply reached the start of vtllibrary@$LIB" wait_for 300 lib_activating
	send hello - hello
	check "hello during the apply: apply_in_progress" jqis "$STEP_DIR/hello.json" .state apply_in_progress
	wait "$bg" || true
	check "the apply completes" jqis "$STEP_DIR/live.apply.json" .result applied
	check "boot recovery did not run on the apply's starts" \
		eq "$(systemctl show -p InactiveExitTimestampMonotonic --value mhvtl-control-apply-recover.service)" "$recover0"

	req=$(grow term 1)
	send plan "$req" term.plan
	send apply "$req" term.apply -units-from "$STEP_DIR/term.plan.json" &
	bg=$!
	check "second apply reached the start of vtllibrary@$LIB" wait_for 300 lib_activating
	unit=$(helper_instance)
	systemctl kill -s TERM "$unit"
	wait "$bg" || true
	check "helper instance exited" wait_for 120 bash -c "! systemctl is-active --quiet '$unit'"
	undropin sleep
	jq -r '.result + " " + (.error // "")' "$STEP_DIR/term.apply.json" >"$STEP_DIR/term.result" 2>&1 || true
	log "  (SIGTERM apply answered: $(cat "$STEP_DIR/term.result"))"
	local l
	for l in $(awk '/^Library: / { print $2 }' "$CONF/device.conf"); do
		check "library $l online after SIGTERM" online "$l"
	done
	send hello - term.hello
	if jqis "$STEP_DIR/term.hello.json" .state incomplete_apply >/dev/null; then
		send recover - term.recover
		check "recover after SIGTERM" jqis "$STEP_DIR/term.recover.json" .result recovered
	fi
	send hello - final.hello
	check "helper ready again" jqis "$STEP_DIR/final.hello.json" .state ready
	others_kept
}

s_placement() {
	ensure_lib
	mtx_status "$LIB" "$STEP_DIR/start.mtx"
	local full empty maps
	mapfile -t full < <(mtx_slots "$STEP_DIR/start.mtx" Full | head -3)
	mapfile -t empty < <(mtx_slots "$STEP_DIR/start.mtx" Empty | head -2)
	mapfile -t maps < <(mtx_maps "$STEP_DIR/start.mtx" Empty | head -1)
	mtx_do "$LIB" transfer "${full[0]}" "${empty[0]}"
	mtx_do "$LIB" transfer "${full[1]}" "${empty[1]}"
	mtx_do "$LIB" transfer "${full[2]}" "${maps[0]}"
	local req
	req=$(grow placement 2)
	check "a cartridge is in the MAP" grep -q 'IMPORT/EXPORT:Full' "$STEP_DIR/placement/live.mtx"
	plan_apply placement "$req"
	check "apply: applied" jqis "$STEP_DIR/placement.apply.json" .result applied
	check "mtx status works" mtx_status "$LIB" "$STEP_DIR/after.mtx"
	check "slots and MAP as before the apply (AC 13)" same <(placement -mtx "$STEP_DIR/placement/live.mtx") <(placement -mtx "$STEP_DIR/after.mtx")
	check "library_contents holds the moves" same <(mtx_placement "$STEP_DIR/placement/live.mtx") <(file_placement "$CONF/library_contents.$LIB")
	others_kept
}

s_placement_force() {
	ensure_lib
	mtx_status "$LIB" "$STEP_DIR/start.mtx"
	local slot bc req
	slot=$(mtx_slots "$STEP_DIR/start.mtx" Full | head -1)
	bc=$(mtx_barcode "$STEP_DIR/start.mtx" "$slot")
	mtx_do "$LIB" load "$slot" 0
	req=$(grow force 1)
	send plan "$req" force.plan
	check "plan refused: drive_loaded" jqis "$STEP_DIR/force.plan.json" .error drive_loaded
	check "plan carries a force token" jqis "$STEP_DIR/force.plan.json" '.force_token != null' true
	send apply "$req" force.apply -units-from "$STEP_DIR/force.plan.json" -force-from "$STEP_DIR/force.plan.json"
	check "forced apply: applied" jqis "$STEP_DIR/force.apply.json" .result applied
	check "forced_unloads lists $bc to slot $slot" jqis "$STEP_DIR/force.apply.json" \
		"[.forced_unloads[]? | select(.barcode == \"$bc\" and .source_slot == $slot)] | length" 1
	check "mtx status works" mtx_status "$LIB" "$STEP_DIR/after.mtx"
	check "drive 0 empty" grep -q '^Data Transfer Element 0:Empty' "$STEP_DIR/after.mtx"
	check "$bc back in slot $slot" eq "$(mtx_barcode "$STEP_DIR/after.mtx" "$slot")" "$bc"
	check "every other cartridge where it was" same \
		<(placement -mtx "$STEP_DIR/force/live.mtx" | sed "s/^Drive 1 $bc from Slot $slot\$/Slot $slot $bc/" | sort -k1,1 -k2,2n) \
		<(placement -mtx "$STEP_DIR/after.mtx" | sort -k1,1 -k2,2n)
	others_kept
}

delete_req() {
	mkdir -p "$STEP_DIR/new"
	sed "/^$MARKER/,\$d" "$CONF/device.conf" >"$STEP_DIR/new/device.conf"
	mkreq delete -delete "library_contents.$LIB" "$STEP_DIR/new/device.conf"
}

s_delete_quiet() {
	unload_all "$LIB"
	plan_apply delete "$(delete_req)"
}

s_delete() {
	ensure_lib
	unit_pids >"$STEP_DIR/pids.before"
	local units u
	units=$(lib_units "$LIB")
	check "device.conf ends with library $LIB's block" grep -q "^$MARKER" "$CONF/device.conf"
	s_delete_quiet
	check "plan units: the library's" jqis "$STEP_DIR/delete.plan.json" '.units | sort | join(" ")' "$(echo $units | tr ' ' '\n' | sort | xargs)"
	check "apply: applied" jqis "$STEP_DIR/delete.apply.json" .result applied
	for u in $units; do
		check "$u stopped" bash -c "! systemctl is-active --quiet $u"
		check "$u not enabled" bash -c "! systemctl is-enabled --quiet $u"
	done
	check "no wants links left" bash -c "! find /etc/systemd/system /run/systemd/generator /run/systemd/generator.late \
		\( $(printf -- "-name %s -o " $units) -false \) 2>/dev/null | grep ."
	check "library_contents.$LIB removed" test ! -e "$CONF/library_contents.$LIB"
	check "home kept: cartridges still in $HOME_PATH" test -e "$HOME_PATH/F40001L8"
	lsscsi -g >"$STEP_DIR/lsscsi"
	check "lsscsi no longer lists 0:16:0" bash -c "! grep -q ':0:16:0]' '$STEP_DIR/lsscsi'"
	unit_pids >"$STEP_DIR/pids.after"
	check "other units' PIDs unchanged" pids_kept "$STEP_DIR/pids.before" "$STEP_DIR/pids.after" $units
	others_kept
}

s_reboot_prep() {
	ensure_lib
	snapshot "$SMOKE_STATE/reboot"
	dropin sleep "/bin/sleep 30"
	kill_mid_apply reboot "$(grow reboot 2)"
	cat /proc/sys/kernel/random/boot_id >"$SMOKE_STATE/reboot.boot_id"
	log "  rebooting in 5 s; run '$0 -o $OUT reboot-check' once the VM is back"
	systemd-run --on-active=5 --unit=apply-smoke-reboot systemctl reboot
}

s_reboot_check() {
	systemctl is-system-running --wait >/dev/null || true
	check "a reboot happened" bash -c "[ \"\$(cat /proc/sys/kernel/random/boot_id)\" != \"\$(cat '$SMOKE_STATE/reboot.boot_id')\" ]"
	check "intent.json removed" test ! -e "$STATE_DIR/intent.json"
	local u=mhvtl-control-apply-recover.service
	journalctl -b -u "$u" --no-pager >"$STEP_DIR/recover.journal" || true
	local rec media lib
	rec=$(systemctl show -p ExecMainExitTimestampMonotonic --value "$u")
	check "boot recovery ran this boot" differs "$rec" 0
	check "boot recovery exited 0" eq "$(systemctl show -p ExecMainStatus --value "$u")" 0
	check "boot recovery restored something" bash -c "! grep -q 'boot recovery: nothing' '$STEP_DIR/recover.journal'"
	media=$(systemctl show -p ExecMainStartTimestampMonotonic --value mhvtl-media.service)
	lib=$(systemctl show -p ExecMainStartTimestampMonotonic --value "vtllibrary@$LIB.service")
	echo "recover exit $rec, mhvtl-media start $media, vtllibrary@$LIB start $lib" >>"$STEP_DIR/checks.log"
	check "boot recovery finished before mhvtl-media started" test "$rec" -le "$media"
	check "boot recovery finished before vtllibrary@$LIB started" test "$rec" -le "$lib"
	restored "after reboot" "$SMOKE_STATE/reboot"
}

# --- driver ------------------------------------------------------------------

collect() {
	local d=$OUT/$1/logs
	mkdir -p "$d"
	journalctl --no-pager --since "$2" -u 'mhvtl-control-apply@*' -u mhvtl-control-apply.socket \
		-u mhvtl-control-apply-recover.service -u mhvtl-control.service -u mhvtl-media.service \
		-u 'vtllibrary@*' -u 'vtltape@*' >"$d/journal.txt" 2>&1 || true
	cp -p "$STATE_DIR/audit.jsonl" "$d/" 2>/dev/null || true
	cp -p "$STATE_DIR/intent.json" "$d/" 2>/dev/null || true
	cp -a "$CONF" "$d/etc-mhvtl" 2>/dev/null || true
	systemctl list-units --all --no-pager 'vtl*' 'mhvtl*' >"$d/units.txt" 2>&1 || true
	lsscsi -g >"$d/lsscsi.txt" 2>&1 || true
	ls -la "$HOME_PATH" >"$d/home.txt" 2>&1 || true
	local l sg
	for l in $(awk '/^Library: / { print $2 }' "$CONF/device.conf" 2>/dev/null); do
		sg=$(lib_sg "$l" 2>/dev/null || true)
		[ -z "$sg" ] || mtx -f "$sg" status >"$d/mtx.$l.txt" 2>&1 || true
	done
}

main() {
	[ "$(id -u)" = 0 ] || { log "run as root"; exit 2; }
	local tool
	for tool in jq mtx lsscsi "$APPLYSMOKE"; do
		command -v "$tool" >/dev/null || { log "missing $tool"; exit 2; }
	done
	[ -w "$CGROUP_PROCS" ] || { log "no $CGROUP_PROCS: is mhvtl-control.service running?"; exit 2; }
	mkdir -p "$OUT" "$SMOKE_STATE"
	log "output: $OUT"
	local s results=() rc since fn
	for s in "${SCENARIOS[@]}"; do
		fn=s_${s//-/_}
		declare -F "$fn" >/dev/null || { log "unknown scenario $s"; exit 2; }
		STEP=$s STEP_DIR=$OUT/$s
		mkdir -p "$STEP_DIR"
		since=$(date '+%Y-%m-%d %H:%M:%S')
		log "=== $s"
		# A subshell with errexit on: an unexpected failure aborts this
		# scenario only, and the failed checks are its exit status.
		set +e
		(
			set -eE
			trap 'log "  ABORT at line $LINENO: $BASH_COMMAND"' ERR
			"$fn"
			exit $((FAILS > 0))
		)
		rc=$?
		set -e
		if [ $rc = 0 ]; then
			results+=("PASS $s")
		else
			results+=("FAIL $s")
			collect "$s" "$since"
		fi
		log "${results[-1]}"
	done
	cp -p "$STATE_DIR/audit.jsonl" "$OUT/" 2>/dev/null || true
	log ""
	log "summary ($OUT):"
	printf '  %s\n' "${results[@]}" >&2
	! printf '%s\n' "${results[@]}" | grep -q '^FAIL'
}

main
