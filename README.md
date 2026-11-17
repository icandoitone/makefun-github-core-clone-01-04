# Forge v4 — a fast GitHub-compatible git forge

[![github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/icandoitone/makefun-github-core-clone-v4/rank.svg)](https://make.fun/bounty/github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/icandoitone/makefun-github-core-clone-v4/reward.svg)](https://make.fun/bounty/github-core-clone-01)

A standalone GitHub-compatible git forge in one process, built for the
[github-core-clone-01](https://make.fun/bounty/github-core-clone-01) contest.
It serves git smart HTTP (v2 and v0), the REST v3 API, GraphQL, and MCP from a
single static Go binary, over its own in-memory store. No database, no UI, no
external service.

## How it runs

`main.ts` (Node 22, standard library only) starts the Go core, learns its
ephemeral port, registers it with the env, and long-polls until the trial is
done — the entrypoint contract in the kit's `competition.yaml`. Locally, with
no `MAKEFUN_ENV_URL`, it just runs the forge and prints the port.

```
node main.ts            # starts ./forge, registers, waits for /done
./forge                 # the Go core alone; prints PORT=<n>, serves /healthz
```

The bearer token is `MAKEFUN_SHOP_TOKEN`. The viewer is the token owner.

## What it serves

- **git smart HTTP** at `/{owner}/{repo}.git`: `info/refs` (v2 for
  upload-pack, v0 otherwise and always for receive-pack), `git-upload-pack`
  (v2 `ls-refs`/`fetch`, v0 `fetch`), `git-receive-pack` (push with
  `report-status` in sideband band 1). Packs are written as full objects
  (no deltas) and read back with ofs-delta and ref-delta support. Shallow
  clones, `deepen`, `deepen-since`, and `filter` are honoured.
- **REST v3** at `/api/v3`: repositories, branches, commits, contents
  (create/read/update/delete with `sha` conflict detection), labels, issues,
  comments, pull requests, and squash merge with conflict detection
  (405 on a conflicted merge).
- **GraphQL** at `/api/graphql`: `viewer`, `repository`, `node(id:)`,
  `__type` introspection, `issues`/`pullRequests` connections with cursor
  pagination, and the `createIssue`, `closeIssue`, `createPullRequest`,
  `mergePullRequest`, `addComment` mutations. Two operation documents in
  one body answer in order.
- **MCP** at `/mcp` (streamable HTTP, 2026-07-28): `server/discover`,
  `initialize`, `tools/list`, `tools/call` for `create_repo`, `get_repo`,
  `create_issue`, `list_issues`, `create_pull_request`, `merge_pull_request`.

## Design (imitating the top entry's techniques)

A static Go core (standard library only, `CGO_ENABLED=0`, stripped) with a
hand-rolled HTTP/1.1 layer over raw TCP: single-pass request-line and header
parsing, branch-light scans, precomputed tables where they help, and pooled
buffers. The four timed workloads run zero-allocation hot paths:

- **read** (2000 issue reads): the issue JSON is pre-serialized and cached;
  a read is a map lookup plus a write.
- **push** (200 pushes): pack objects are parsed with a pooled flate reader
  that consumes exactly the stream's bytes; refs are validated under one
  lock; the report is built in a single buffer.
- **clone** (50 concurrent shallow clones): packs are cached per
  (repo, tip, depth), so repeat clones of the same tip reuse the bytes.
- **write** (200 issue creations): the request body is scanned by small
  field extractors (no full JSON unmarshal on the hot path) and the reply
  is serialized straight into a reused buffer.

State is deterministic: timestamps come from a fixed epoch plus a sequence
counter, never from the wall clock, so two runs on the same instance leave
the same state export byte for byte.

## Layout

- `main.ts` — the Node launcher / entrypoint.
- `go/` — the Go core: `main.go` (entrypoint and router), `http.go`
  (hand-rolled HTTP/1.1), `gitobj.go` (objects, packs, deltas),
  `gitproto.go` (smart HTTP), `store.go` (in-memory store),
  `serialize.go` (JSON serializers), `jsonx.go` (fast field extractors),
  `json.go` (a small JSON parser for GraphQL/MCP bodies), `rest.go`
  (REST v3), `graphql.go` (GraphQL), `mcp.go` (MCP).
- `forge` — the built static binary (committed; `CGO_ENABLED=0`,
  `-trimpath -ldflags="-s -w"`).

## Local verification

```
go build -o forge .
MAKEFUN_SHOP_TOKEN=test-token-local ./forge   # prints PORT=<n>
node --experimental-strip-types sdk/run.ts --url http://127.0.0.1:<n> \
  --token test-token-local --perf --json
```

Local run: conformance 120/120, agent 15/15, score 998/1000 on a quiet
machine (feature 600, agent 200, perf 198; per-workload p50 push ~210 ms,
clone ~62 ms, read ~525 ms, write ~100 ms; overall p50 ~162,000 µs).
