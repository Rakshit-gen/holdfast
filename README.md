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
  web: 1 prefix, at least 2 of 4 sites keep announcing
```

While it runs:

```
holdfast status            # services, modes, the fleet's view, BGP sessions
holdfast drain dns         # mark dns unhealthy here; the guard decides if it may leave
holdfast undrain dns
```

In [examples](examples):

- [holdfast.json](examples/holdfast.json): one site of four, with dns and web.
- [minimal.json](examples/minimal.json): the least a site needs.
- [holdfast.service](examples/holdfast.service): a hardened systemd unit.
- [bird.conf](examples/bird.conf): the router side in BIRD 2, filtering what holdfast may announce and acting on graceful shutdown. Not tested here; check it with `bird -p`.
- [alerts.yml](examples/alerts.yml): Prometheus alert rules.

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
| `tls_cert`, `tls_key` | serve reports over TLS |
| `ca_file` | check `https://` peers against these certificates instead of the system roots |
| `peers[]` | `site` and `url` of every other site |
| `interval` | how often to poll each peer (5s) |
| `stale` | a report older than this counts as unknown (30s, at least twice `interval`) |
| `min_up`, `min_fraction` | the floor: the larger of the two, and at least one site |
| `quorum` | share of the other sites this site must see before it may withdraw (0.5) |
| `unguarded` | withdraw on any failed check, like a plain health checker; for comparison only |

## Running it

**Starting.** Every service starts withdrawn and stays that way until its checks pass `rise` times. A restarted holdfast doesn't announce a service it hasn't checked.

**Stopping.** On SIGTERM or SIGINT holdfast marks every route for graceful shutdown (if `graceful_shutdown` is set), waits that long, withdraws everything, then closes each session with an administrative shutdown NOTIFICATION that says why (RFC 8203). Routers don't have to wait out the hold timer.

**Draining.** `holdfast drain [service]`, or `touch` the `drain_file`, marks services unhealthy at this site. The guard still decides whether the site may leave, so draining one site at a time is safe and draining all of them at once isn't possible. The status shows what the guard did and why.

**Watching.** `GET /metrics` on the API address serves Prometheus text:

| metric | |
|---|---|
| `holdfast_service_healthy{service}` | checks pass here |
| `holdfast_service_mode{service,mode}` | 0 announce, 1 degrade, 2 withdraw |
| `holdfast_guard_holding{service}` | the guard is keeping this site up although it is unhealthy |
| `holdfast_guard_held_total{service}` | times that has started |
| `holdfast_fleet_sites_announcing{service}` | sites announcing, as this site sees them |
| `holdfast_fleet_peer_reachable{site}` | the last poll of that site worked |
| `holdfast_bgp_established{neighbor}` | session state |
| `holdfast_bgp_prefixes_sent{neighbor}` | prefixes the neighbor has from us |
| `holdfast_bgp_flaps_total{neighbor}` | times the session dropped |

`holdfast_guard_holding` is the one to alert on: a site is unhealthy and the floor is holding it up, so either the floor is protecting you from a bad check or more of the fleet is broken than the floor allows to leave.

## What the simulator says

`holdfast sim` and `holdfast bench` play a fleet minute by minute through failures and count how much user traffic fails. **The world is invented.** Failure rates, how long failures last, and how traffic moves are parameters picked for the simulator, not measurements of any real network. The numbers show how the policies differ under those assumptions and nothing more.

The model: 20 sites for a week. Each site's users go to their own site if it announces normally, otherwise they spread over the sites that do. Degraded sites only get traffic when no site announces normally, except that 20% of a degraded site's own users still reach it, since a longer path doesn't beat every route their network has. Checks are perfect (no rise or fall delay) and sites see each other's reports a minute late. Three policies face the same failures:

- **static:** plain anycast, never withdraws.
- **unguarded:** withdraws whenever its check fails.
- **guarded:** holdfast, floor at 30% of sites.

`holdfast bench -seeds 10`, means over 10 worlds per row:

| scenario | static: failed | unguarded: failed | guarded: failed | unguarded: dark minutes | guarded: dark minutes |
|---|---|---|---|---|---|
| site failures | 0.324% | 0.000% | 0.003% | 0 | 0 |
| false alarms | 0.324% | 0.318% | 0.010% | 32 | 0 |
| regional | 0.468% | 0.000% | 0.004% | 0 | 0 |
| big regional | 0.703% | 0.000% | 0.015% | 0 | 0 |
| partitions | 0.289% | 0.000% | 0.003% | 0 | 0 |
| mixed | 0.310% | 0.276% | 0.003% | 27 | 0 |

- **site failures:** each site fails for real 0.2 times a day, 20 minutes on average.
- **false alarms:** plus every check failing at once 0.3 times a day, 30 minutes, while the service works.
- **regional:** plus 30% of sites failing for real together 0.3 times a day, 45 minutes.
- **big regional:** the same, but 80% of sites at once.
- **partitions:** plus each site losing sight of the fleet once a day for 15 minutes.
- **mixed:** site failures, false alarms, 30% regional outages and partitions at lower rates.

"Dark minutes" are minutes in which no site announced at all: the Facebook failure. Unguarded health checks do best when failures are real and local, and turn every false alarm into a total outage. Guarded pays a little on real failures, for the minute a sick site spends degraded before it withdraws and for the sick sites the floor holds up, and never goes dark.

`holdfast replay` plays a single outage minute by minute. A false alarm at 10 sites, trimmed (columns are announcing/degraded/withdrawn sites, then failed traffic):

```
MINUTE  STATIC       UNGUARDED    GUARDED
4       10/0/0   0%  10/0/0   0%  10/0/0   0%
5       10/0/0   0%  0/0/10 100%  0/10/0   0%
6       10/0/0   0%  0/0/10 100%  0/3/7   0%
...
14      10/0/0   0%  0/0/10 100%  0/3/7   0%
15      10/0/0   0%  10/0/0   0%  10/0/0   0%
```

Every guarded site degrades in the first minute, hears that the others did too, and then seven withdraw while three hold. With `-share 0.8` (eight sites really broken), guarded loses 16% of traffic in the first minute while the broken sites are degraded and still keep some of their users, then 2%: the one broken site the floor holds up.

What the floor costs when 80% of sites really are broken:

| floor | failed | dark minutes |
|---|---|---|
| 5% | 0.005% | 0 |
| 10% | 0.005% | 0 |
| 20% | 0.006% | 0 |
| 30% | 0.015% | 0 |
| 50% | 0.034% | 0 |
| 70% | 0.052% | 0 |

A higher floor keeps more broken sites up in a real regional outage. A lower one leaves fewer sites to carry everyone during a false alarm, and the simulator doesn't model capacity, so it can't show that side. Pick the floor from how many sites can carry the whole load.

## Testing

```
go test -race ./...                              # unit tests, plus whole fleets of 2 and 3 sites over loopback
go test -fuzz FuzzRead -fuzztime 1m ./bgp         # the BGP message parser
scripts/interop.sh                               # against a real GoBGP
```

The fleet tests run several holdfast daemons, each with its own stand-in router and a health endpoint the test turns off, and check what the routers end up with: one sick site leaves, every site sick keeps the floor, a cut-off site fails static, a stop withdraws before closing.

`scripts/interop.sh` runs holdfast against GoBGP v3.37.0 and checks, through `gobgp`, that the session comes up, IPv4 and IPv6 (MP_REACH) prefixes and communities arrive, a failing check prepends the path, recovery restores it, and stopping marks the routes for graceful shutdown and then withdraws them. CI runs it on every push.

## Limits

- **Brief overshoot.** Each site decides from its own view of the fleet. If two sites see a third differently (one has a newer report than the other), their rankings can differ and both may withdraw. A withdrawn site stays out while it is unhealthy, so this lasts until one of them recovers. Unknown sites take withdraw slots, which keeps this on the safe side in most cases but not all.
- **Degrade is a hint.** Prepending only moves traffic for networks that prefer shorter AS paths. Networks with local preference on their own routes, or that ignore path length, keep sending to a degraded site. `degrade_communities` helps where the upstream acts on a tag.
- **Floor without capacity.** holdfast doesn't know how much load a site can take. If the floor is two sites and they can't carry everyone, the floor keeps the service reachable but not fast.
- **BGP coverage.** IPv4 and IPv6 unicast only. Four-octet AS, route refresh (answered, not enhanced), graceful restart (offered, as a restart time), administrative shutdown messages. No TCP MD5 or TCP-AO, no BFD, no ADD-PATH, no extended messages, no policy on routes received (holdfast counts them and enforces `max_prefixes`, nothing more).
- **Fleet TLS is server-side only.** Sites prove who they are with the shared token, not client certificates.
- **Hold timers can't be turned off.** `hold_time` 0 means the 90s default.

## References

- RFC 4271, BGP-4
- RFC 4760, multiprotocol extensions (IPv6 in MP_REACH_NLRI)
- RFC 6793, four-octet AS numbers
- RFC 2918, route refresh
- RFC 4724, graceful restart
- RFC 8203, administrative shutdown messages
- RFC 8326, graceful BGP session shutdown (the GRACEFUL_SHUTDOWN community)
- RFC 1997 and RFC 8092, communities and large communities
- RFC 2439, route flap damping, which the health check damping copies
- [Facebook's October 4 2021 outage write-up](https://engineering.fb.com/2021/10/05/networking-traffic/outage-details/)

## License

MIT
