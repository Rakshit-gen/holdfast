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
