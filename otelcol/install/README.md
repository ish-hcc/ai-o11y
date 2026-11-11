# Installing the collector on a VM

`install.sh` puts the ai-o11y collector on a Linux VM with systemd, as the
service `aio11y-otelcol`:

1. registers the VM with ai-o11y (`POST /aio11y/targets`, by its CB-Tumblebug
   `nsId`, `infraId` and `nodeId`; ai-o11y reads the rest from CB-Tumblebug),
2. fetches the VM's collector configuration
   (`GET /aio11y/targets/{targetId}/collectorConfig`),
3. checks it with `aio11y-otelcol validate`; when it fails nothing is installed
   and a running collector is left as it is,
4. installs the binary, configuration and unit, and starts the service.

Copy this directory and the collector binary (`make otelcol`, or a URL to
download it from) to the VM, then:

```sh
export AIO11Y_AUTH_TOKEN=...       # when ai-o11y sets AIO11Y_AUTH_TOKEN
export AIO11Y_WRITE_PASSWORD=...   # when the backend's write account uses basic auth
sudo --preserve-env=AIO11Y_AUTH_TOKEN,AIO11Y_WRITE_PASSWORD ./install.sh \
  --server http://ai-o11y:8094 --ns ns01 --infra infra01 --node node01 \
  --binary ./aio11y-otelcol        # or --binary-url <URL>
```

The token and the password are read from the environment only, never from
arguments, so they do not show in the process list. The token goes to curl in a
header file, and the password is written to `/etc/aio11y-otelcol/env` and never
printed.

| Command | What |
|---|---|
| `install.sh --server ... --ns ... --infra ... --node ... --binary ...` | Install, or install again: the VM is registered again (its description is replaced, its id stays), and the binary, configuration and unit are replaced. Without `--binary`/`--binary-url` the installed binary is kept; without `--server` the one saved at install is used. Without `AIO11Y_WRITE_PASSWORD` the saved password is kept. |
| `install.sh --config-only` | Fetch the configuration again, as after a change of the backend or the collectors, check it and restart. No registration, binary or unit change. |
| `uninstall.sh` | Stop and delete the service, its binary, configuration and disk queue. The target stays registered. |
| `uninstall.sh --deregister` | Also delete the target from ai-o11y (`DELETE /aio11y/targets/{targetId}`); its stored series stay in the backend. When ai-o11y refuses, nothing is removed. A target ai-o11y no longer has counts as deleted. |

## What it installs

| Path | What |
|---|---|
| `/usr/local/bin/aio11y-otelcol` | The collector. |
| `/etc/aio11y-otelcol/config.yaml` | The configuration ai-o11y rendered for the VM (0640). |
| `/etc/aio11y-otelcol/env` | `AIO11Y_WRITE_PASSWORD`, which the configuration reads as `${env:AIO11Y_WRITE_PASSWORD}` (0600). Absent when no password was given. |
| `/etc/aio11y-otelcol/target-id`, `server` | The target id and the ai-o11y address, for `--config-only` and `uninstall.sh --deregister`. |
| `/etc/systemd/system/aio11y-otelcol.service` | The unit ([aio11y-otelcol.service](aio11y-otelcol.service)). |
| `/var/lib/aio11y-otelcol` | The service's state directory (systemd `StateDirectory`), where the exporters keep their disk queue. |

The service runs as root: the GPU receivers run `nvidia-smi` and `amd-smi`
(which opens `/dev/kfd`), XID counting reads `/dev/kmsg`, and the process
scraper reads every process. It restarts on failure and is limited to 512 MiB
of memory (`MemoryMax`).

## Requirements

Linux with systemd, bash 4.4 or later and curl on the VM. The VM reaches
ai-o11y at `--server`, and the backend's write address in the configuration.

## Tests

```sh
make -C otelcol test    # runs test/run.sh among the collector's tests
otelcol/install/test/run.sh
```

`test/run.sh` runs both scripts end to end without root: a fake ai-o11y
(`test/fake_server.py`, needs python3), fake `systemctl` and `journalctl`, a
fake collector binary, and every path under a scratch directory named by
`AIO11Y_ROOT`, which only tests set.
