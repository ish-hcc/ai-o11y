# Prometheus + Thanos sample

To start the whole replicated setup on one host instead, see
[deploy/README.md](../../../deploy/README.md).

One Prometheus taking OTLP from the collectors, a Thanos Sidecar next to it and
a Thanos Querier in front, the shape ai-o11y's Thanos backend was first tested
against. It has no replication: one Prometheus, no object storage.

```
collector ──OTLP/HTTP──▶ Prometheus ◀── Thanos Sidecar ◀──gRPC── Thanos Querier ◀── ai-o11y
```

ai-o11y settings:

```sh
AIO11Y_BACKEND=thanos
AIO11Y_BACKEND_QUERY_URL=http://<host>:10902
AIO11Y_BACKEND_WRITE_URL=http://<host>:9090/api/v1/otlp
```

Prometheus keeps the names collectors send (`otlp.translation_strategy:
NoTranslation` in [prometheus.yml](prometheus.yml)), the names ai-o11y's metric
catalog queries. A Thanos Receive in place of Prometheus would not do: it
rewrites the names on the way in.

This sample has no authentication: the Thanos Querier has none of its own, so
an account for queries needs a proxy in front of it.
