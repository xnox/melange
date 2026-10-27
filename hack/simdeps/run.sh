#!/usr/bin/env bash
# Simulate rebuilding every package with versioned "depends:" and check
# that stereo still resolves.  See README.md.
#
# Usage: hack/simdeps/run.sh [index|stereo|baseline|sim|report|all]
#
# Environment:
#   WORK     work directory (default: ./simdeps-work); needs ~2GB
#   STEREO   stereo checkout to clone from (default: ~/chainguard-dev/stereo)
#   OPTOUT   origin globs passed to -no-versioned-provides
#   ARCHES   architectures to rewrite (default: "x86_64 aarch64")
#   JOBS     parallelism (default: 12)
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)
WORK=$(realpath -m "${WORK:-./simdeps-work}")
STEREO=${STEREO:-$HOME/chainguard-dev/stereo}
OPTOUT=${OPTOUT:-}
ARCHES=${ARCHES:-x86_64 aarch64}
JOBS=${JOBS:-12}

REPOS=(
	https://apk.cgr.dev/chainguard
	https://apk.cgr.dev/extra-packages
	https://apk.cgr.dev/chainguard-private
	https://packages.cgr.dev/extras
	https://packages.wolfi.dev/os
)

mkdir -p "$WORK"

tools() {
	(cd "$ROOT" && go build -o "$WORK/simdeps" ./hack/simdeps && go build -o "$WORK/melange" .)
}

# Rewrite and sign every index.  Fetched .melange.yaml files are cached
# in $WORK/sim/cache, so reruns only fetch new builds.
index() {
	tools
	for arch in $ARCHES; do
		"$WORK/simdeps" -arch "$arch" -out "$WORK/sim" -j 48 \
			${OPTOUT:+-no-versioned-provides "$OPTOUT"} "${REPOS[@]}"
	done
	[ -f "$WORK/sim-signing.rsa" ] || "$WORK/melange" keygen "$WORK/sim-signing.rsa"
	find "$WORK/sim" -name APKINDEX.tar.gz -print0 |
		xargs -0 -n1 "$WORK/melange" sign-index --force --signing-key "$WORK/sim-signing.rsa"
}

# A fresh clone of stereo, so that the user's checkout is untouched.
stereo() {
	rm -rf "$WORK/stereo"
	git clone -q --shared "$STEREO" "$WORK/stereo"
	git -C "$WORK/stereo" config user.name simdeps
	git -C "$WORK/stereo" config user.email simdeps@localhost
	make -C "$WORK/stereo" -s tools/stereo
}

# Run compile-all and lock-update; $1 names the logs.  "make
# lock-update" stops at the first "art resolve" failure, so "art lock"
# is run on its own too.
run() {
	local name=$1
	(cd "$WORK/stereo" && ./tools/stereo compile --lock --all --write -j "$JOBS") \
		>"$WORK/compile-$name.log" 2>&1 || true
	(cd "$WORK/stereo/containers" && make lock-update) >"$WORK/lock-$name.log" 2>&1 || true
	(cd "$WORK/stereo/containers" && art lock ./images/...) >"$WORK/lock-$name-lock.log" 2>&1 || true

	grep -v '^{' "$WORK/compile-$name.log" | grep -E '^[a-zA-Z0-9._+-]+: ' | cut -d: -f1 | sort -u >"$WORK/compile-$name.failed" || true
	grep '^ERROR' "$WORK/lock-$name.log" | cut -d: -f1 | sort -u >"$WORK/lock-$name-resolve.failed" || true
	grep '^ERROR' "$WORK/lock-$name-lock.log" | cut -d: -f1 | sort -u >"$WORK/lock-$name-lock.failed" || true
}

baseline() {
	run baseline
	git -C "$WORK/stereo" add -A
	git -C "$WORK/stereo" commit -qm "baseline locks"
}

# Point stereo and art at the rewritten indexes, then rerun.
sim() {
	local s=$WORK/sim
	sed -i -E "s#\"https://apk\.cgr\.dev/(chainguard|extra-packages|chainguard-private)\"#\"$s/apk.cgr.dev/\1\"#" \
		"$WORK/stereo/cmd/stereo/main.go"
	make -C "$WORK/stereo" -s -B tools/stereo
	(cd "$WORK/stereo/containers" &&
		sed -i -E "s#\"https://(apk\.cgr\.dev/(chainguard|chainguard-private|extra-packages)|packages\.cgr\.dev/extras|packages\.wolfi\.dev/os)\"#\"$s/\1\"#g" \
			$(cat "$HERE/stereo-cue-files.txt"))
	cat >"$WORK/stereo/containers/dev_config.cue" <<EOF
package main

cfg: {
	keyring: ["$WORK/sim-signing.rsa.pub"]
	repositories: []
}
EOF
	run sim
}

report() {
	cd "$WORK"
	echo "== new compile-all failures"
	comm -13 compile-baseline.failed compile-sim.failed
	echo "== new art resolve failures"
	comm -13 lock-baseline-resolve.failed lock-sim-resolve.failed
	echo "== new art lock failures"
	comm -13 lock-baseline-lock.failed lock-sim-lock.failed
	echo "== most changed package selections (locks, ignoring repository URLs)"
	git -C stereo diff -U0 -- '*.yaml.lock' '*package_lock.cue' |
		grep '^[-+] ' | grep -v "https://\|$WORK/sim" | sed 's/=.*//' |
		sort | uniq -c | sort -rn | head -30
}

case ${1:-all} in
index) index ;;
stereo) stereo ;;
baseline) baseline ;;
sim) sim ;;
report) report ;;
all) index && stereo && baseline && sim && report ;;
*) echo "usage: $0 [index|stereo|baseline|sim|report|all]" >&2; exit 2 ;;
esac
