#!/bin/sh
# Runs holdfast against a real GoBGP and checks what GoBGP receives:
# the announcement, the longer path when degraded, and the withdrawal
# when holdfast stops.
#
#   go install github.com/osrg/gobgp/v3/cmd/gobgpd@v3.37.0 github.com/osrg/gobgp/v3/cmd/gobgp@v3.37.0
#   scripts/interop.sh
#
# Everything listens on 127.0.0.1 on unprivileged ports, so it needs no root.

set -eu

GOBGPD="${GOBGPD:-gobgpd}"
GOBGP="${GOBGP:-gobgp}"
command -v "$GOBGPD" >/dev/null || { echo "gobgpd not found; see the top of $0" >&2; exit 1; }

dir="$(mktemp -d "${TMPDIR:-/tmp}/holdfast-interop.XXXXXX")"
pids=""
cleanup() {
	for p in $pids; do kill "$p" 2>/dev/null || true; done
	wait 2>/dev/null || true
	rm -rf "$dir"
}
trap cleanup EXIT

cat >"$dir/gobgpd.toml" <<TOML
[global.config]
  as = 65000
  router-id = "10.255.0.1"
  port = 10179
  local-address-list = ["127.0.0.1"]

[[neighbors]]
  [neighbors.config]
    neighbor-address = "127.0.0.1"
    peer-as = 65001
  [neighbors.transport.config]
    passive-mode = true
  [[neighbors.afi-safis]]
    [neighbors.afi-safis.config]
      afi-safi-name = "ipv4-unicast"
  [[neighbors.afi-safis]]
    [neighbors.afi-safis.config]
      afi-safi-name = "ipv6-unicast"
TOML

cat >"$dir/holdfast.json" <<JSON
{
  "site": "test1",
  "asn": 65001,
  "router_id": "10.255.0.2",
  "next_hop": "127.0.0.1",
  "next_hop6": "2001:db8::2",
  "api": "127.0.0.1:19179",
  "graceful_shutdown": "3s",
  "neighbors": [
    {"address": "127.0.0.1", "port": 10179, "asn": 65000, "hold_time": "9s", "families": ["ipv4", "ipv6"]}
  ],
  "services": [
    {
      "name": "dns",
      "prefixes": ["198.51.100.53/32", "2001:db8:53::/48"],
      "communities": ["65001:53"],
      "checks": [{"type": "absent", "path": "$dir/drain"}],
      "interval": "500ms",
      "rise": 1,
      "fall": 1
    }
  ]
}
JSON

api="--port 50071"
"$GOBGPD" -f "$dir/gobgpd.toml" --api-hosts 127.0.0.1:50071 -l warn >"$dir/gobgpd.log" 2>&1 &
pids="$pids $!"
go build -o "$dir/holdfast" ./cmd/holdfast
"$dir/holdfast" run -config "$dir/holdfast.json" >"$dir/holdfast.log" 2>&1 &
hf=$!
pids="$pids $hf"

# wait_for DESCRIPTION COMMAND...: retries for up to 15 seconds.
wait_for() {
	what="$1"; shift
	i=0
	while ! "$@" >/dev/null 2>&1; do
		i=$((i + 1))
		if [ "$i" -gt 30 ]; then
			echo "FAIL: $what" >&2
			echo "--- gobgp rib:" >&2; "$GOBGP" $api global rib -a ipv4 >&2 || true
			echo "--- holdfast log:" >&2; cat "$dir/holdfast.log" >&2
			echo "--- gobgpd log:" >&2; cat "$dir/gobgpd.log" >&2
			exit 1
		fi
		sleep 0.5
	done
	echo "ok: $what"
}

rib4() { "$GOBGP" $api global rib -a ipv4; }
rib6() { "$GOBGP" $api global rib -a ipv6; }
has() {
	flags=""
	[ "$1" = "-i" ] && { flags="-i"; shift; }
	pattern="$1"; shift
	"$@" | grep -q $flags -- "$pattern"
}
lacks() { pattern="$1"; shift; ! "$@" | grep -q -- "$pattern"; }
# The AS path column for a prefix.
path_is() { rib4 | grep 198.51.100.53/32 | grep -q -- "$1"; }

wait_for "session comes up" has Establ "$GOBGP" $api neighbor
wait_for "ipv4 prefix announced" has 198.51.100.53/32 rib4
wait_for "ipv6 prefix announced over MP_REACH" has 2001:db8:53::/48 rib6
wait_for "community attached" has 65001:53 rib4
wait_for "plain path" path_is "65001 "

# One site alone may not withdraw (the floor is one site), so a failing
# check degrades it: the same prefix with a longer path.
touch "$dir/drain"
wait_for "degraded path is prepended" path_is "65001 65001 65001 65001"
rm "$dir/drain"
wait_for "healthy again, plain path" lacks "65001 65001" rib4

kill "$hf"
wait_for "marked for graceful shutdown when holdfast stops" has -i graceful rib4
wait_for "withdrawn when holdfast stops" lacks 198.51.100.53/32 rib4
echo "interop passed"
