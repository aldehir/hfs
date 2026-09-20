# hfs

Control a Hugging Face Space that runs llama.cpp.

- **`hfs`** (local CLI): drives the HF API for lifecycle (start, pause, hardware,
  Dev Mode) and runs Ansible over the Dev Mode SSH endpoint to provision.
- **`hfsd`** (daemon in the Space): serves the app port (proxy to `llama-server`,
  status page when it's down, so the Space always passes its health check),
  supervises `llama-server`, and runs long jobs (build, bench) that outlive the
  SSH session that started them.

```
hfs up [profile]          start the Space, enable Dev Mode, provision
hfs down                  pause the Space
hfs status                stage, hardware, server health, what's provisioned
hfs info                  endpoint URL, OpenAI base URL, SSH target
hfs provision [profile]   build llama.cpp and (re)start llama-server
hfs bench [profile]       run llama-bench, fetch JSON into results/
hfs ssh [-- cmd]          tmux session on the Space, or run a command
hfs ctl [args]            hfsd client on the Space (default: ps)
hfs logs [name] [-f]      hfsd process log: llama-server (default), build, bench
                          or the Space's own logs: run, build-image
```

## Setup

```sh
make            # bin/hfs (local) and bin/hfsd (static linux/amd64)
mkdir -p ~/.config/hfs && ln -s "$PWD/hfs.yml" ~/.config/hfs/hfs.yml
```

Requires `ansible-playbook` and `ssh` locally, an HF token in `$HF_TOKEN` or
`~/.cache/huggingface/token`, and an SSH key registered with your HF account
(`ssh.identity_file` in `hfs.yml`).

`hfs.yml` names the one Space `hfs` manages. Every API call is scoped to it.

## Profiles

A profile is an Ansible vars file in `profiles/`. It picks the llama.cpp source,
the model, and the server/bench arguments. Defaults live in
`ansible/group_vars/all.yml`.

```yaml
llama_repo: aldehir/llama.cpp
llama_ref: my-branch            # or llama_pr: 12345
model: ggml-org/Qwen3.6-27B-GGUF:Q4_K_M
server_args:
  - [-c, 262144]
  - --no-mmap
```

Override per run with `--repo`, `--ref`, `--pr`, `--model`. Anything after `--`
goes to `ansible-playbook` (`-- -v`, `-- --check`, `-- -e key=value`).

## Provisioning

| Stage (`--tags`) | Does |
|---|---|
| always | installs `hfsd` from `/data/hfs/hfsd` (uploaded by `hfs` when it changes) and starts it |
| `build` | fetches the ref, builds `llama-server` + `llama-bench` as an hfsd job, installs to `~/.hfs/builds/<sha>-<flags>` |
| `server` | `hfsd run llama-server`: replaced only if the build, model, or args changed; waits for `/health` |

- Changing only server args: `hfs provision --tags server` (seconds).
- Finished builds are cached in `/data/builds`, so a fresh container restores
  the binaries instead of recompiling. ccache lives on `/data` too.
- Hacking on the remote checkout (`~/llama.cpp`): `hfs provision --no-update`
  builds the tree as-is. A dirty tree installs to `builds/dirty` and skips the
  cache. Without `--no-update`, provisioning refuses to touch a dirty tree.
- A failed build or an unhealthy server fails the play and prints the log tail.
- Lost your connection mid-build? The job keeps going. Re-run `hfs provision`
  and it re-attaches instead of starting over.

## hfsd

One binary: `hfsd serve` is the daemon, everything else is a client over
`~/.hfs/hfsd.sock`. Control is only reachable through SSH; the public port
serves just the proxy and a status page.

```
hfsd run NAME [--restart always|on-failure] [--env K=V] [--dir D] [--replace] [--wait|--follow] -- CMD...
hfsd wait NAME [--timeout 60s]     exit with the job's status; 75 = still running
hfsd ps | status NAME | logs NAME [-f] [-n N]
hfsd stop|start|restart|rm NAME
hfsd shutdown
```

- `run` with the same name + spec as a live process is a no-op (`unchanged`);
  a different spec needs `--replace`.
- `--restart never` (default) is a job; anything else is a service. Services
  are recorded in `~/.hfs/procs.json` and relaunched when hfsd restarts.
- A service that dies within 10s five times in a row is marked `failed`.
- Run anything through it: `hfs ctl run eval --follow -- python3 eval.py`.

The Space's `entrypoint.sh` runs `/data/hfs/hfsd serve` in a loop (falling back
to nginx if it was never uploaded). Upgrading hfsd shuts the daemon down, which
**stops its jobs and services**; services come back, jobs don't.

## Remote layout

```
~/.hfs/
  bin/hfsd          hfsd.sock  procs.json
  builds/<key>/     current -> builds/<key>
  logs/             <name>.log (previous run in <name>.log.1)
  state.json        what `hfs status` reports
~/llama.cpp         source + build tree (container-local)
/data/hfs/hfsd      boot copy of hfsd
/data/builds        build cache      /data/cache   models
/data/bench         bench results    /data/.ccache
```

Everything outside `/data` is gone when the Space restarts; `hfs up`
re-provisions.

## SSH gateway quirks

Handled in `ansible/ansible.cfg`:

- No sftp subsystem → `transfer_method = piped`.
- Multiplexed master connections get dropped → `ControlMaster=no`.
- The SSH user is the Space subdomain, not a remote account → fixed `remote_tmp`.
- Piping files through a tty truncates them → `usetty = False`. Large binaries
  still get corrupted, so `hfs` uploads hfsd itself over a plain ssh pipe.
- **Sessions that are silent for 5 minutes are killed** (SSH keepalives don't
  count). Long work runs as an hfsd job and Ansible polls it with
  `hfsd wait --timeout 60s`. `hfs logs -f` on a quiet log will drop too.
