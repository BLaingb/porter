# porter

A terminal UI for monitoring listening ports on macOS. Shows each process's port, PID, name, git repo/worktree, and Docker container (if applicable). Kill processes, filter by port or repo, and permanently hide noisy system apps like Chrome or Postman.

![porter screenshot](porter.png)

## Columns

| Column | Description |
|--------|-------------|
| PORT | Listening port number |
| PID | Process ID |
| PROCESS | Process name |
| REPO | Git repository name (if the process runs inside a git repo) |
| TREE | Git worktree name (if running in a linked worktree) |
| CONTAINER | Docker container name (if the port is published by a Docker container) |
| COMMAND | Full command line |

## Docker support

When a port is published by a Docker container, porter shows the container name in the CONTAINER column. Pressing `k` on a Docker-backed port runs `docker stop <container>` instead of sending a signal to the host process — so the container shuts down cleanly rather than killing the low-level proxy.

## Keybindings

| Key | Action |
|-----|--------|
| `↑` / `↓` | Navigate |
| `enter` | Open selected port in browser (`http://localhost:<port>`) |
| `pgup` / `pgdn` | Page up / down |
| `k` | Kill selected process or stop Docker container (prompts for confirmation) |
| `h` | Hide selected process by name (persisted across restarts) |
| `H` | Toggle showing hidden processes |
| `u` | Unhide selected process (while hidden processes are visible) |
| `/` | Filter by port number or repo name |
| `esc` | Clear filter |
| `r` | Force refresh |
| `q` / `ctrl+c` | Quit |

Hidden process names are saved to `~/.porter/hidden.json`.

## Install

**Via Makefile (installs to `/usr/local/bin`):**

```sh
make install
```

**Via `go install`:**

```sh
go install github.com/blaingb/porter@latest
```

## Run without installing

```sh
make build
./porter
```

Or directly:

```sh
go run .
```

## Requirements

- macOS (uses `lsof` and `ps`)
- Go 1.21+ — install with `brew install go`
