# fastvm console

Web console for **fastvm**: a browser UI on top of the `fastvmd` HTTP/JSON API
(sandbox lifecycle, stats, images, event log). Built with React 19, TypeScript,
Tailwind CSS with HyperUI-style components, Headless UI, Heroicons, deterministic DiceBear avatars, TanStack Query, and [Vite+](https://viteplus.dev) (`vp`).

The console talks to the daemon **same-origin**: a small Vite plugin proxies
`/v1/*` (including WebSocket upgrades) to `fastvmd` in both dev and preview. The
proxy prefers the daemon's mTLS TCP API and can fall back to the Unix socket, so
there is no CORS or browser-side certificate handling. Themes:
dark, light, or follow the system appearance via the switch next to the user
avatar (persisted per browser).

## Features

- Keycloak OIDC login (authorization code + PKCE), session persisted per tab.
- Sandbox dashboard: state, live CPU and memory utilization, uptime, wake counts.
- Images menu: cached base images, manual pulls, removal, and Create VM with the
  selected image preselected.
- Create VMs from base-disk images with host-directory and writable block data volumes;
  pause, deep sleep, resume and delete them.
- Detail drawer with live utilization and the lifecycle event log.
- In-browser exec terminal rendered with [ghostty-web](https://github.com/coder/ghostty-web)
  (Ghostty's VT100 parser compiled to WebAssembly) over a WebSocket PTY.
- Dark, light and follow-the-system themes, switched next to the user avatar.

## Screenshots

Dark and light themes, side by side:

![fastvm console in light and dark themes](../docs/assets/console-themes.png)

Exec terminal:

![fastvm console exec terminal](../docs/assets/console-terminal.png)

## Prerequisites

| Requirement       | Notes                                                                                          |
| ----------------- | ---------------------------------------------------------------------------------------------- |
| Node 24.11+ / 26+ | tested with 24.14.0, pinned in `.node-version`; Vite+ does not support Node 25                 |
| pnpm              | used as the package manager; `vp` is run through `pnpm exec`                                   |
| Docker            | only for the Keycloak OIDC stack in `console/compose.yaml`                                     |
| fastvmd           | built from the repo root; needs root/`CAP_NET_ADMIN` for TAP/veth/nftables, or user namespaces |

## Quickstart

From the repository root:

```sh
# 1. Keycloak (realm `fastvm`, demo user demo / demo)
docker compose -f console/compose.yaml up -d

# 2. Console environment (defaults match the compose stack; edit as needed)
cp console/.env.example console/.env

# 3. Install dependencies
cd console
pnpm install            # or: pnpm exec vp install

# 4. In another (root) shell, from the repo root: build and start the daemon
make build
sudo make start         # fastvmd -config hack/config.yaml, mTLS API on 127.0.0.1:8443
make image              # build hack/Dockerfile.example -> demo:latest base disk

# 5. Dev server (HMR). By default the proxy targets
#    https://127.0.0.1:8443 with /tmp/fastvm/certs/client.
export FASTVM_HOST=https://127.0.0.1:8443
export FASTVM_CERT_PATH=/tmp/fastvm/certs/client

# 6. Start the dev server
pnpm exec vp dev        # http://localhost:5173
```

Open http://localhost:5173 and sign in through Keycloak with `demo` / `demo`.

## Scripts

All commands run in `console/` through the Vite+ CLI:

| Command                | What it does                                     |
| ---------------------- | ------------------------------------------------ |
| `pnpm exec vp dev`     | dev server at :5173 with the `/v1` proxy         |
| `pnpm exec vp build`   | typecheck + production build into `dist/`        |
| `pnpm exec vp preview` | serve `dist/` locally, also with the `/v1` proxy |
| `pnpm exec vp test`    | unit tests (Vitest + jsdom)                      |
| `pnpm exec vp check`   | format, lint, and typecheck                      |
| `pnpm exec vp install` | install/refresh dependencies                     |

## The API proxy

A browser cannot present a client certificate or connect to the daemon's Unix
socket directly, so the Vite plugin in
[`vite-plugins/fastvm-proxy.ts`](vite-plugins/fastvm-proxy.ts) proxies
same-origin `/v1/*` traffic. By default it targets the mTLS TCP API at
`https://127.0.0.1:8443` with client certificates from
`/tmp/fastvm/certs/client`:

```sh
export FASTVM_HOST=https://127.0.0.1:8443        # optional: default
export FASTVM_CERT_PATH=/tmp/fastvm/certs/client # optional: default
```

`FASTVM_HOST` and `FASTVM_CERT_PATH` mirror the `fastvmctl` client settings.
Set `FASTVM_SOCKET` to explicitly use the Unix socket instead
(`/run/fastvm/fastvm.sock`).

The plugin is registered via `configureServer` (dev) and
`configurePreviewServer` (preview). It:

- intercepts only request paths starting with `/v1/`;
- forwards method, headers, and body to the daemon over HTTPS with the client
  certificate, and pipes the response back unchanged;
- tunnels WebSocket upgrades for `/v1/*` (the exec terminal) over the same
  TLS connection and raw-pipes the upgraded client socket in both directions;
  upgrades outside `/v1/` (e.g. Vite HMR) are left untouched;
- answers plain HTTP with HTTP 502 and a JSON body
  `{"error": "fastvmd unavailable at ...: ..."}` when the API is unreachable,
  and destroys the client socket if the WebSocket upgrade cannot reach it.

These are **server-side** environment variables, not `VITE_*` values. Restart
the dev/preview server after changing them.

## OIDC configuration

Browser-visible settings live in `console/.env` (copy `.env.example`; `.env` is
git-ignored). They are validated by `src/lib/config.ts`.

| Variable                  | Default                 |
| ------------------------- | ----------------------- |
| `VITE_KEYCLOAK_URL`       | `http://localhost:8081` |
| `VITE_KEYCLOAK_REALM`     | `fastvm`                |
| `VITE_KEYCLOAK_CLIENT_ID` | `fastvm-console`        |
| `VITE_KEYCLOAK_SCOPE`     | `openid profile email`  |

## API endpoints used

Same-origin `/v1/*`, implemented in `src/lib/api.ts`:

| Method   | Path                           | Used for                                  |
| -------- | ------------------------------ | ----------------------------------------- |
| `GET`    | `/v1/version`                  | daemon version                            |
| `GET`    | `/v1/sandboxes`                | sandbox list (polled every 3s)            |
| `GET`    | `/v1/sandboxes/{ref}`          | one sandbox (by name or id)               |
| `GET`    | `/v1/sandboxes/{ref}/stats`    | live utilization (polled every 2s)        |
| `GET`    | `/v1/sandboxes/{ref}/shell/ws` | interactive PTY shell (WebSocket upgrade) |
| `GET`    | `/v1/images`                   | base disk images                          |
| `POST`   | `/v1/images/pull`              | pull an image and build a base disk       |
| `DELETE` | `/v1/images?ref=...`           | remove an image and its tags              |
| `GET`    | `/v1/events?limit=N`           | lifecycle event log (polled every 5s)     |
| `POST`   | `/v1/sandboxes`                | create a sandbox                          |
| `POST`   | `/v1/sandboxes/{ref}/sleep`    | `{ "deep": bool }`                        |
| `POST`   | `/v1/sandboxes/{ref}/wake`     | wake a paused/dormant sandbox             |
| `DELETE` | `/v1/sandboxes/{ref}`          | delete a sandbox                          |

## Terminal / exec

`GET /v1/sandboxes/{ref}/shell/ws` upgrades to a WebSocket that bridges the
sandbox's PTY. Query parameters: `cols`, `rows`, and repeatable `argv` (fastvmd
defaults to `/bin/sh` when no `argv` is given).

Protocol:

| Direction       | Frame  | Meaning                                           |
| --------------- | ------ | ------------------------------------------------- |
| client → server | binary | raw PTY input bytes                               |
| client → server | text   | `{"type":"resize","cols":N,"rows":N}`             |
| server → client | binary | raw PTY output bytes                              |
| server → client | text   | `{"type":"exit","code":N}` then the server closes |
| server → client | text   | `{"type":"error","message":"..."}` then it closes |

`src/lib/shell.ts` implements the typed client used by the terminal UI:

```ts
const handle = connectShell(
  "web",
  { cols: 80, rows: 24 },
  {
    onOpen: () => {},
    onOutput: (bytes: Uint8Array) => term.write(bytes),
    onExit: (code: number | null) => {},
    onError: (message: string) => {},
  },
);
handle.sendInput("ls\r");
handle.resize(120, 40);
handle.close();
```

`buildShellUrl(ref, { cols, rows, argv? })` (and the
`buildShellUrlFromLocation(...)` test seam) expose the URL; `connectShell`
returns `{ state, sendInput, resize, close }` where `state` is `"connecting" |
"open" | "closed"`. A close without an exit frame is surfaced as `onExit(null)`
for a clean close (code 1000) and `onError(...)` otherwise.

The terminal renderer is [ghostty-web](https://github.com/coder/ghostty-web)
(Ghostty's VT parser compiled to WASM, WASM embedded in the package); the UI
dynamic-imports it so that large payload is not part of the initial console
bundle.

The shell is **unauthenticated** exactly like the rest of the fastvmd API — the
proxy adds no authorization, so anyone who can reach the console/proxy origin
can open a shell. For production, terminate authentication in front of the
console and the daemon (see [Security model](#security-model)).

## Host volumes

Sandboxes can mount host directories into the guest with **virtio-fs** (Cloud
Hypervisor + `virtiofsd`), so files are shared directly instead of copied. The
create-VM dialog has a **Host volumes (virtio-fs)** section; the same payload is
accepted by `POST /v1/sandboxes`:

```json
{
  "image": "demo:latest",
  "volumes": [{ "host_path": "/srv/data", "guest_path": "/mnt/data", "read_only": true }]
}
```

- `host_path` must be an absolute host directory. fastvmd validates it against
  the `host_volumes.allowed_prefixes` allowlist (an empty list denies every
  volume), so the console cannot mount arbitrary host paths.
- `guest_path` is the absolute mountpoint inside the guest (no whitespace, `:`
  or `,`); `read_only` is optional and defaults to read-write.
- Volumes are static at create time. They survive pause/dormant/wake and are
  shown in the sandbox detail drawer as `host → guest` with a read-only badge.
- Requires Cloud Hypervisor and `virtiofsd` on the host; see
  [the Cloud Hypervisor migration notes](../docs/CLOUD_HYPERVISOR_MIGRATION.md).

## Security model

Keycloak authenticates people, but `fastvmd`'s API is **not** authenticated:
the Vite proxy forwards `/v1/*` to the unix socket without checking the OIDC
token, so the sign-in screen is a UX gate rather than an API authorization
layer. Keep the daemon socket on a trusted host/localhost, and for a real
deployment terminate authentication in front of both the console and the daemon
(for example with `oauth2-proxy` or an authenticating reverse proxy), exposing
only the proxied API — never the raw socket.

## Production

`pnpm exec vp build` emits static assets in `dist/`. Serve them behind a reverse
proxy that:

1. maps `/v1/` to the `fastvmd` unix socket (e.g. nginx
   `proxy_pass http://unix:/run/fastvm/fastvm.sock:/v1/;`), and
2. serves `index.html` for SPA routes.

Configure the Keycloak client's redirect URI (and web origins) for the deployed
console origin, e.g. `https://console.example.com/*`, in the realm/client
settings. The browser build reads the `VITE_KEYCLOAK_*` values at build time, so
rebuild (or bake them into the build env) for production values.

## Layout

```
console/
├── compose.yaml             # Keycloak dev stack (realm import, port 8081)
├── keycloak/
│   └── realm-fastvm.json    # exported realm: fastvm-console client + demo user
├── vite.config.ts           # Vite+ config: fmt/lint/staged, /v1 proxy, jsdom tests
├── vite-plugins/
│   └── fastvm-proxy.ts      # /v1/* -> fastvmd unix socket (dev + preview)
├── index.html
├── .env.example             # VITE_KEYCLOAK_* (defaults match compose)
├── src/
│   ├── main.tsx             # QueryClientProvider + AuthProvider + App
│   ├── App.tsx              # auth gate: loading / login / dashboard
│   ├── auth/                # OIDC + PKCE (pkce.ts, oidc.ts, AuthProvider, session)
│   ├── lib/                 # zod API types, fetch client, Query hooks, shell WS client
│   ├── pages/               # LoginPage, DashboardPage
│   ├── components/          # sidebar, table, utilization, actions, dialogs, drawer, exec terminal
│   ├── ui/                  # Tailwind + Headless UI primitives (button, form, modal, drawer, menu, toast)
│   ├── utils/               # formatting + error helpers
│   ├── theme.ts             # terminal palettes for ghostty-web
│   ├── theme/               # theme mode (dark/light/auto), provider, system sync
│   ├── styles.css
│   └── test/setup.ts        # jest-dom matchers for Vitest
├── README.md
└── pnpm-lock.yaml
```

---
