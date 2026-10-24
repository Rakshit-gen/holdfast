# holdfast

holdfast announces a site's anycast prefixes over BGP while the service behind them passes its health checks, and withdraws them when it doesn't. That part is what every anycast health checker does. The difference is that holdfast sites talk to each other, and a site only withdraws if enough other sites will still be announcing. A check that fails everywhere at once can't take the whole service off the internet.

It is one static Go binary with no dependencies outside the standard library. It runs its own BGP speaker, so there is no ExaBGP or BIRD to drive.

## The problem

On 2021-10-04 a maintenance command took down Facebook's backbone, and its DNS servers lost their connection to the data centers. Each DNS site had a health check for exactly that case: if the data centers are unreachable, withdraw the anycast routes so users go to a healthier site. Every site saw the same failure at the same moment, so every site withdrew, and Facebook's DNS disappeared from the internet ([Facebook's write-up](https://engineering.fb.com/2021/10/05/networking-traffic/outage-details/)).

Each site did the right thing for one site. Nobody checked what the fleet was doing as a whole. A health check that withdraws on failure assumes that failures are local. When the failure is shared (a backbone, a config push, a dependency every check calls) the safety mechanism becomes the outage.

holdfast keeps per-site withdrawal, and adds a floor: a set number or share of sites keep announcing each service no matter what their checks say. If the checks are right and the service is broken everywhere, users reach a broken service either way. If the checks are wrong, the floor is what keeps the service up.

## How it works

Each site runs holdfast next to its edge routers. For every service it has:

1. **A probe.** TCP, HTTP, DNS, a command, or a drain file, repeated every few seconds. Rise and fall counts stop one bad result from moving routes, and optional damping in the style of RFC 2439 holds down a check that keeps flapping.
2. **A fleet view.** Each site serves its own state over HTTP and polls the others. A site's report says whether it is healthy and what it is announcing. There is no coordinator.
3. **The guard.** It picks one of three modes for the service at this site:
   - **announce:** normal routes.
   - **degrade:** the same prefixes with the AS path prepended (3 times by default) and optional extra communities. Routers prefer any other site, but if nothing else is left, this site still answers.
   - **withdraw:** no routes.
4. **BGP sessions** to the site's routers, which send only what changed.

The guard's rules, in order:

- A healthy site announces.
- A site that turns unhealthy degrades first. It may withdraw only once it has a fresh report from every other site that is newer than its own change. If many sites failed in the same second, each of them learns that before anyone leaves. Without this step they would all act on each other's old "healthy" reports and withdraw together.
- A site that is already withdrawn and still unhealthy stays withdrawn. The floor holds sites up; it never brings one back that hasn't passed its checks, including one that is starting up.
- If this site can't see at least half of the other sites (reports older than 30 seconds by default), it keeps degrading and doesn't withdraw. Fail static: a site that can't see the fleet can't know it's safe to leave.
- Otherwise every unhealthy site runs the same calculation. Sites that are stale or have never reported count as unhealthy and still announcing, so they use up withdraw slots. The unhealthy sites are ranked: those already withdrawn first, so decisions stick, then by a hash of site and service name, so the same site isn't always the one kept up. The first `sites - floor` of them withdraw and the rest degrade.

The floor is the larger of `min_up` sites and `min_fraction` of the fleet, and at least one. A service can set its own.

## Install

```
curl -fsSL https://raw.githubusercontent.com/Rakshit-gen/holdfast/main/scripts/install.sh | sh
```

That downloads the release for your OS and architecture, checks its SHA256, and puts `holdfast` in `~/.local/bin`. Or build it:

```
go install github.com/Rakshit-gen/holdfast/cmd/holdfast@latest
```

## Quick start

Write a config (see [examples/holdfast.json](examples/holdfast.json) for a four-site fleet), then:

```
holdfast check -config /etc/holdfast/holdfast.json          # validate it
holdfast check -config /etc/holdfast/holdfast.json -probe   # and run every check once
holdfast run   -config /etc/holdfast/holdfast.json
```

`check` reports every problem in the config at once, and prints each service's floor so you can see what the guard will hold:

```
examples/holdfast.json: ok. Site ams1, 2 neighbors, 4 sites in the fleet.
  dns: 2 prefixes, at least 3 of 4 sites keep announcing
  web: 1 prefixes, at least 2 of 4 sites keep announcing
```

While it runs:

```
holdfast status            # services, modes, the fleet's view, BGP sessions
holdfast drain dns         # mark dns unhealthy here; the guard decides if it may leave
holdfast undrain dns
```

There is a hardened systemd unit in [examples/holdfast.service](examples/holdfast.service).

## Config

JSON. Unknown fields are errors, since a misspelled `min_up` quietly falling back to its default is the kind of mistake this tool exists to prevent.

Top level:

| field | meaning |
|---|---|
| `site` | this site's name, as the other sites know it |
| `asn`, `router_id` | local AS (four-octet is fine) and BGP identifier |
| `next_hop`, `next_hop6` | next hops for IPv4 and IPv6 routes; each is required if a service has prefixes of that family |
| `listen` | address for incoming BGP, like `:179`; needed only for passive neighbors |
| `api` | local API for `status`, `drain` and `/metrics`; must be loopback, since it has no auth |
| `drain_file` | while this file exists every service at the site counts as unhealthy |
| `graceful_shutdown` | before withdrawing, re-announce with the GRACEFUL_SHUTDOWN community (RFC 8326) for this long, up to 5m |

`neighbors[]`: `address`, `port` (179), `asn`, `passive`, `local_address`, `hold_time` (90s, at least 3s), `families` (`ipv4`, `ipv6`, both by default), `max_prefixes` the peer may send (10000), `graceful_restart` (RFC 4724 restart time; off unless set). eBGP neighbors get the local AS prepended, iBGP neighbors get LOCAL_PREF 100.

`services[]`:

| field | meaning |
|---|---|
| `name`, `prefixes` | a set of prefixes announced and withdrawn together |
| `communities`, `large_communities`, `med` | attributes on every route |
| `degrade_prepend` | extra copies of the local AS when degraded, 0 to 10 (3) |
| `degrade_communities` | extra communities when degraded, for routers that act on a tag rather than path length |
| `checks[]` | all must pass; `tcp` (`addr`), `http` (`url`, `host`, `status`, `contains`; redirects are not followed), `dns` (`server`, `name`), `exec` (`command` as a list, no shell), `absent` (`path`) |
| `interval`, `timeout` | how often to check (2s) and how long each check may take (the interval) |
| `rise`, `fall` | passes in a row to come up (2) and failures in a row to go down (3) |
| `damping` | hold down a flapping check: penalty 1000 a fall, halving every 5m, suppressed above 2500 until under 750 |
| `min_up`, `min_fraction` | this service's floor, replacing the fleet's |

`fleet`:

| field | meaning |
|---|---|
| `listen` | where this site serves its reports to the others |
| `token_file` | shared bearer token, checked in constant time |
| `peers[]` | `site` and `url` of every other site |
| `interval` | how often to poll each peer (5s) |
| `stale` | a report older than this counts as unknown (30s, at least twice `interval`) |
| `min_up`, `min_fraction` | the floor: the larger of the two, and at least one site |
| `quorum` | share of the other sites this site must see before it may withdraw (0.5) |
| `unguarded` | withdraw on any failed check, like a plain health checker; for comparison only |
