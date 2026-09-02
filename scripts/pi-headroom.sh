#!/usr/bin/env bash
# Estimate whether livecaption fits inside a 1 GB Raspberry Pi under the
# stress load we actually expect: 50 SSE viewers and 5 audio listeners.
#
# This is an ESTIMATE, not proof. It runs on x86 with the app pinned to 4
# cores; the CPU projection multiplies measured work by a per-model factor.
# Everything the Pi's own hardware decides — USB audio capture, thermals,
# wifi, USB bus contention — is unmeasurable here and is printed as such.
#
#   ./scripts/pi-headroom.sh 5m
#   ./scripts/pi-headroom.sh 3h
set -euo pipefail

DUR_ARG=${1:-5m}
REPLAY=${REPLAY:-replays/260816_0938.mp3}
PORT=${PORT:-8099}
SSE=${SSE:-50}
LISTENERS=${LISTENERS:-5}
RAM_BUDGET_MB=1024
CORES=4

# Factor = how much slower one Pi core is than one core of this machine, for
# this workload. Published-spec estimates. Measured load is ~0.03 core-sec/sec,
# so these only matter if they are wrong by two orders of magnitude.
declare -A FACTOR=([3B+]=5.0 [4]=3.0 [5]=1.7)

die() { echo "pi-headroom: $*" >&2; exit 1; }

# 90s -> 90, 5m -> 300, 3h -> 10800
to_secs() {
  case $1 in
    *h) echo $(( ${1%h} * 3600 ));;
    *m) echo $(( ${1%m} * 60 ));;
    *s) echo "${1%s}";;
    *)  echo "$1";;
  esac
}

DUR=$(to_secs "$DUR_ARG")
[[ $DUR -ge 30 ]] || die "duration must be at least 30s"
[[ -f $REPLAY ]] || die "no replay file at $REPLAY (set REPLAY=...)"
command -v jq >/dev/null || die "needs jq"

cd "$(dirname "$0")/.."
go build -o bin/livecaption ./cmd/livecaption

WORK=$(mktemp -d)
CLIENTS=()
APP_PID=""
cleanup() {
  [[ ${#CLIENTS[@]} -gt 0 ]] && kill "${CLIENTS[@]}" 2>/dev/null || true
  [[ -n $APP_PID ]] && kill "$APP_PID" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

# One process for the whole run, so RSS drift means something: loop the replay
# file (stream copy, no re-encode) to just past the requested duration.
src_secs=$(ffprobe -v error -show_entries format=duration -of csv=p=0 "$REPLAY" | cut -d. -f1)
loops=$(( DUR / src_secs + 1 ))
echo "preparing ${loops}x looped input (${DUR}s run from ${src_secs}s source)..."
ffmpeg -v error -stream_loop "$loops" -i "$REPLAY" -c copy -y "$WORK/loop.mp3"

echo "starting livecaption on :$PORT, pinned to $CORES cores..."
taskset -c 0-$((CORES-1)) ./bin/livecaption replay "$WORK/loop.mp3" \
  --engine mock --no-transcript --addr ":$PORT" >"$WORK/app.log" 2>&1 &
APP_PID=$!

for _ in $(seq 30); do
  curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null && break
  sleep 1
done
curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null || { cat "$WORK/app.log"; die "server never came up"; }

echo "attaching $SSE SSE viewers and $LISTENERS audio listeners..."
for _ in $(seq "$SSE"); do
  curl -sN --max-time "$((DUR+60))" "http://127.0.0.1:$PORT/events" >/dev/null 2>&1 &
  CLIENTS+=($!)
done
for _ in $(seq "$LISTENERS"); do
  curl -sN --max-time "$((DUR+60))" "http://127.0.0.1:$PORT/audio.mp3" >/dev/null 2>&1 &
  CLIENTS+=($!)
done

# Sample once a second. CPU is summed over livecaption plus its live ffmpeg
# children (cutime only counts reaped children, so the children are read
# directly). Samples stream to awk; nothing is written to disk.
echo "sampling for ${DUR}s (Ctrl-C to stop early)..."
sample_stream() {
  local tick prev=-1 end=$(( $(date +%s) + DUR ))
  tick=$(getconf CLK_TCK)
  while [[ $(date +%s) -lt $end ]]; do
    kill -0 "$APP_PID" 2>/dev/null || { echo "DIED"; return; }
    local jiffies=0 rss=0 p f
    for p in "$APP_PID" $(pgrep -P "$APP_PID" 2>/dev/null); do
      [[ -r /proc/$p/stat ]] || continue
      # utime (14) + stime (15), after the comm field which may contain spaces
      f=$(sed 's/.*) //' "/proc/$p/stat" 2>/dev/null) || continue
      jiffies=$(( jiffies + $(echo "$f" | cut -d' ' -f12) + $(echo "$f" | cut -d' ' -f13) ))
      rss=$(( rss + $(awk '/^VmRSS:/{print $2}' "/proc/$p/status" 2>/dev/null || echo 0) ))
    done
    if [[ $prev -ge 0 ]]; then
      awk -v d="$((jiffies - prev))" -v t="$tick" -v r="$rss" \
        'BEGIN{printf "S %.4f %.1f\n", d/t, r/1024}'
    fi
    prev=$jiffies
    sleep 1
  done
}

SUMMARY=$(sample_stream | awk -v cores="$CORES" -v budget="$RAM_BUDGET_MB" '
  /^DIED/ { died=1; next }
  /^S/ { n++; cpu[n]=$2; sum+=$2; if($3>peak)peak=$3; rss[n]=$3 }
  END {
    if (died) print "DIED"
    if (n < 4) { print "SHORT"; exit }
    asort(cpu); q=int(n/4)
    for(i=1;i<=q;i++) first+=rss[i]
    for(i=n-q+1;i<=n;i++) last+=rss[i]
    printf "N %d\nMEAN %.3f\nP95 %.3f\nPEAK %.1f\nDRIFT %.1f\n", \
      n, sum/n, cpu[int(n*0.95)], peak, last/q - first/q
  }')

STATS=$(curl -sf "http://127.0.0.1:$PORT/api/stats" || echo '{}')
kill "${CLIENTS[@]}" 2>/dev/null || true

get() { awk -v k="$1" '$1==k{print $2}' <<<"$SUMMARY"; }
MEAN=$(get MEAN); P95=$(get P95); PEAK=$(get PEAK); DRIFT=$(get DRIFT); N=$(get N)
[[ -n $MEAN ]] || die "run ended before it could be measured; see $WORK/app.log"

echo
echo "=========================================================="
printf "duration %ss   samples %s   load %s SSE + %s audio\n" "$DUR" "$N" "$SSE" "$LISTENERS"
grep -q DIED <<<"$SUMMARY" && echo "*** the app exited during the run — see log ***"
echo
printf "RSS peak      %.1f MB / %s MB budget\n" "$PEAK" "$RAM_BUDGET_MB"
printf "RSS drift     %+.1f MB (first quarter -> last quarter)\n" "$DRIFT"
printf "CPU/wall-sec  mean %.2f   p95 %.2f   (of %s cores here)\n" "$MEAN" "$P95" "$CORES"
echo
jq -r '"drops         xruns \(.source.xruns_total)  frames \(.source.frames_dropped_total)  " +
       "ffmpeg-restarts \(.source.ffmpeg_restarts_total)  audio \(.audio.chunks_dropped_total)  " +
       "monitor \(.monitor.frames_dropped_total)\nhealth        \(.health)"' <<<"$STATS" 2>/dev/null \
  || echo "drops         (stats unavailable)"
echo
echo "projection    (measured p95 work x per-model core factor)"
for m in "3B+" "4" "5"; do
  awk -v p="$P95" -v f="${FACTOR[$m]}" -v c="$CORES" -v m="$m" \
    'BEGIN{v=p*f; printf "              %-4s %.2f / %.1f cores   %s\n", m, v, c,
           (v<c*0.5 ? "ok" : v<c*0.75 ? "tight" : "no")}'
done
echo
echo "unproven      USB audio capture, thermals, wifi, USB bus contention (3B+ only),"
echo "              and real STT upload — this run used --engine mock."
echo "=========================================================="
