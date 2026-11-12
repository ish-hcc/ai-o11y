# VictoriaMetrics sample

Two single-node VictoriaMetrics (A, B) holding the same data. Collectors write
through a vmauth to a vmagent, which writes every sample to both storages and
buffers on disk for a storage that is down; queries go through a vmauth to the
first storage that is up. Either vmauth, vmagent or storage can stop without
collection or queries stopping.

```
collector ─1st─▶ vmauth-1 ─┐                ┌─▶ storage A
          └2nd─▶ vmauth-2 ─┴─▶ vmagent 1/2 ─┴─▶ storage B
ai-o11y ──query─▶ vmauth-1 / vmauth-2 ──▶ storage A or B
```

vmauth has two accounts: a writer that may only write, for collectors, and a
reader that may only query, for ai-o11y. Their names and passwords are in
`.env` (`node.env` on VMs); change the sample passwords before exposing vmauth.
The samples speak plain HTTP, so the passwords travel in clear text: put TLS in
front of vmauth before it is reachable from outside a private network.

ai-o11y settings:

```sh
AIO11Y_BACKEND=victoria
AIO11Y_BACKEND_QUERY_URL=http://<vmauth-1>:8427,http://<vmauth-2>:8427
AIO11Y_BACKEND_WRITE_URL=http://<vmauth-1>:8427/opentelemetry,http://<vmauth-2>:8427/opentelemetry
AIO11Y_BACKEND_QUERY_USERNAME=<reader>
AIO11Y_BACKEND_QUERY_PASSWORD=<reader password>
AIO11Y_BACKEND_WRITE_USERNAME=<writer>
```

A query entrance that serves the API under a path over https with a private
CA and a read token, such as `https://<node>:8443/metrics/api/v1/query`, takes
the path in the address and the token instead of the reader account:

```sh
AIO11Y_BACKEND_QUERY_URL=https://<node-a>:8443/metrics,https://<node-b>:8443/metrics
AIO11Y_BACKEND_QUERY_BEARER_TOKEN=<read token>
AIO11Y_BACKEND_QUERY_TLS_CA_FILE=<path to the CA's PEM file>
```

`AIO11Y_BACKEND_QUERY_USERNAME` and `_QUERY_PASSWORD` stay empty then: both
an account and a token fail startup.

Each collector reads the writer's password from `AIO11Y_WRITE_PASSWORD` in its
own environment; ai-o11y never holds it. A collector started without it runs,
but every write is refused (401).

VictoriaMetrics keeps the names collectors send (`system.cpu.time`), the names
ai-o11y's metric catalog queries; `-opentelemetry.usePrometheusNaming` must
stay off, its default. Each storage deduplicates the two copies a vmagent
failover may write (`-dedup.minScrapeInterval`, the collection interval).

## On one host (docker compose)

[compose.yaml](compose.yaml) runs all of it on one host, vmauth-1 on port 8427
and vmauth-2 on 8428 by default; see [.env.example](.env.example).

## On two VMs

[vm/](vm/) installs one storage VM: VictoriaMetrics, a vmagent writing to both
VMs' storages and a vmauth preferring its own VM, as systemd services. Run it
on each VM with SELF and PEER swapped:

```sh
cp vm/node.env.example vm/node.env   # set SELF_ADDR, PEER_ADDR and the rest
sudo vm/install.sh vm/node.env
```

It downloads the release from GitHub and checks it against the release's
checksums. Ports: 8427 (vmauth), 8428 (storage), 8429 (vmagent); collectors
and ai-o11y need 8427, the two VMs need 8428 and 8429 between them.
