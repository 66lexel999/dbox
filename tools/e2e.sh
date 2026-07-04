#!/usr/bin/env bash
# End-to-end suite for MyIDM. Run from repo root in git-bash:
#   bash tools/e2e.sh
# Covers: segmented integrity, pause/resume, no-range fallback, chunked
# (unknown size), and crash recovery after a hard kill.
set -u

ROOT="A:/PersonalApps/MyIDM"
T="$ROOT/.e2e"
SRV="http://127.0.0.1:18081"
TS="http://127.0.0.1:19090"
PASS=0; FAIL=0
APPID=""; TSPID=""

say()  { printf '%s\n' "$*"; }
ok()   { PASS=$((PASS+1)); say "  PASS: $1"; }
bad()  { FAIL=$((FAIL+1)); say "  FAIL: $1"; }
check(){ if eval "$2"; then ok "$1"; else bad "$1"; fi; }

jstr(){ grep -o "\"$2\":\"[^\"]*\"" <<<"$1" | head -1 | cut -d'"' -f4; }
jnum(){ grep -o "\"$2\":-\?[0-9][0-9.]*" <<<"$1" | head -1 | cut -d: -f2; }

add(){ # url segments -> task id
  local resp; resp=$(curl -s -X POST "$SRV/api/tasks" -H "Content-Type: application/json" -d "{\"url\":\"$1\",\"segments\":${2:-0}}")
  jstr "$resp" id
}
view(){ curl -s "$SRV/api/tasks/$1"; }

wait_status(){ # id want timeout_s
  local i=0 s
  while [ $i -lt "$3" ]; do
    s=$(jstr "$(view "$1")" status)
    [ "$s" = "$2" ] && return 0
    if [ "$s" = "failed" ] && [ "$2" != "failed" ]; then
      say "    (task $1 failed: $(jstr "$(view "$1")" error))"; return 1
    fi
    sleep 1; i=$((i+1))
  done
  say "    (timeout waiting for $2, last status: $s)"; return 1
}

hash_match(){ # taskid sourcefile
  local name path
  name=$(jstr "$(view "$1")" fileName)
  path="$T/dl/$name"
  [ -f "$path" ] || { say "    (missing $path)"; return 1; }
  local a b
  a=$(sha256sum "$path" | cut -d' ' -f1)
  b=$(sha256sum "$2" | cut -d' ' -f1)
  [ "$a" = "$b" ]
}

cleanup(){
  [ -n "$APPID" ] && kill -9 "$APPID" 2>/dev/null
  [ -n "$TSPID" ] && kill -9 "$TSPID" 2>/dev/null
  sleep 1
}
trap cleanup EXIT

start_app(){
  "$ROOT/bin/myidm.exe" -listen 127.0.0.1:18081 -dir "$T/dl" -data "$T/data" -open=false >>"$T/app.log" 2>&1 &
  APPID=$!
  local i=0
  while [ $i -lt 20 ]; do
    curl -s -o /dev/null "$SRV/api/tasks" && return 0
    sleep 0.5; i=$((i+1))
  done
  say "FATAL: app did not come up"; exit 1
}

# ---------- setup ----------
say "== setup =="
rm -rf "$T"; mkdir -p "$T/files" "$T/dl" "$T/data"
head -c $((32*1024*1024)) /dev/urandom > "$T/files/big.bin"
head -c $((8*1024*1024))  /dev/urandom > "$T/files/nr.bin"
head -c $((6*1024*1024))  /dev/urandom > "$T/files/ch.bin"
head -c $((16*1024*1024)) /dev/urandom > "$T/files/slow.bin"

"$ROOT/bin/testserver.exe" -listen 127.0.0.1:19090 -dir "$T/files" >>"$T/ts.log" 2>&1 &
TSPID=$!
sleep 1
start_app

# ---------- test 1: segmented download integrity ----------
say "== test 1: ranged 32MB, 6 segments =="
ID=$(add "$TS/ranged/big.bin" 6)
check "task created" '[ -n "$ID" ]'
wait_status "$ID" completed 120; check "completed" '[ $? -eq 0 ]'
V=$(view "$ID")
SEGS=$(grep -o '"start"' <<<"$V" | wc -l)
check "used multiple segments (got $SEGS)" '[ "$SEGS" -ge 2 ]'
check "sha256 matches source" 'hash_match "$ID" "$T/files/big.bin"'

# ---------- test 2: pause / resume keeps progress ----------
say "== test 2: pause/resume on throttled server =="
ID2=$(add "$TS/slow/slow.bin?bps=524288" 4)
sleep 4
curl -s -X POST "$SRV/api/tasks/$ID2/pause" >/dev/null
wait_status "$ID2" paused 10; check "paused" '[ $? -eq 0 ]'
D1=$(jnum "$(view "$ID2")" downloaded)
check "partial progress kept ($D1 bytes)" '[ "$D1" -gt 0 ] && [ "$D1" -lt $((16*1024*1024)) ]'
check "part file on disk" 'ls "$T/dl"/*.part >/dev/null 2>&1'
sleep 2
D1B=$(jnum "$(view "$ID2")" downloaded)
check "no progress while paused" '[ "$D1B" = "$D1" ]'
curl -s -X POST "$SRV/api/tasks/$ID2/resume" >/dev/null
wait_status "$ID2" completed 90; check "resumed to completion" '[ $? -eq 0 ]'
check "sha256 matches source after resume" 'hash_match "$ID2" "$T/files/slow.bin"'

# ---------- test 3: server without range support ----------
say "== test 3: no-range server falls back to single stream =="
ID3=$(add "$TS/norange/nr.bin" 8)
wait_status "$ID3" completed 60; check "completed" '[ $? -eq 0 ]'
V3=$(view "$ID3")
SEGS3=$(grep -o '"start"' <<<"$V3" | wc -l)
check "single segment used (got $SEGS3)" '[ "$SEGS3" -eq 1 ]'
check "sha256 matches source" 'hash_match "$ID3" "$T/files/nr.bin"'

# ---------- test 4: chunked transfer, unknown size ----------
say "== test 4: chunked (no Content-Length) =="
ID4=$(add "$TS/chunked/ch.bin" 8)
wait_status "$ID4" completed 60; check "completed" '[ $? -eq 0 ]'
SZ4=$(jnum "$(view "$ID4")" size)
check "size learned post-hoc ($SZ4)" '[ "$SZ4" = "6291456" ]'
check "sha256 matches source" 'hash_match "$ID4" "$T/files/ch.bin"'

# ---------- test 5: crash recovery (hard kill mid-download) ----------
say "== test 5: hard-kill recovery =="
ID5=$(add "$TS/slow/big.bin?bps=1048576" 4)
sleep 5
kill -9 "$APPID" 2>/dev/null
sleep 1
start_app
S5=$(jstr "$(view "$ID5")" status)
check "task auto-requeued after crash (status: $S5)" '[ "$S5" = "queued" ] || [ "$S5" = "downloading" ]'
wait_status "$ID5" completed 120; check "completed after restart" '[ $? -eq 0 ]'
check "sha256 matches source after crash+resume" 'hash_match "$ID5" "$T/files/big.bin"'

# ---------- test 6: real-world URL (chunked from GitHub) ----------
say "== test 6: real internet URL =="
ID6=$(add "https://codeload.github.com/maxuanquang/idm/zip/refs/heads/main" 8)
if wait_status "$ID6" completed 120; then
  ok "real download completed"
  SZ6=$(jnum "$(view "$ID6")" size)
  check "non-trivial size ($SZ6 bytes)" '[ "$SZ6" -gt 100000 ]'
else
  bad "real download (network-dependent)"
fi

# ---------- summary ----------
say ""
say "== summary: $PASS passed, $FAIL failed =="
exit "$FAIL"
