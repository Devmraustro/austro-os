# AUSTRO OS Desktop — Wails v3 Proof of Concept

This directory is an isolated Go module. It does not change the existing root
API entry point, worker, Docker deployment, or browser/Web mode.

## Scope

The proof of concept reuses the existing `internal/webui` embedded assets and
serves them through a Wails v3 desktop window. It is intended to verify the
desktop shell and frontend asset compatibility before integrating the
application services.

**Important limitation:** this first shell does not start the AUSTRO API,
PostgreSQL, Redis, RabbitMQ, or worker. Existing UI API requests therefore do
not become functional merely because the window opens. Offline operation and
full application workflows are explicitly out of scope for this shell-only
proof of concept.

## Toolchain

- Go 1.26.10 (matching the root module toolchain)
- Wails v3.1.0
- Windows 10/11 with Microsoft Edge WebView2 Runtime
- Wails' documented Windows build prerequisites (including the supported
  C/C++ toolchain where required by the selected Wails build configuration)

Wails is MIT-licensed. Its v3 module declares Go 1.26.0 and toolchain 1.26.10.
The exact dependency is pinned to v3.1.0 rather than using an unbounded latest
version.

## Build on Windows

From the repository root:

```powershell
cd desktop
go mod tidy
go test ./...
go install github.com/wailsapp/wails/v3/cmd/wails3@v3.1.0
wails3 build
```

The commands above are instructions, not evidence that a Windows build has
already been run. The build must be executed on a supported Windows runner or
Windows development machine before this PoC can be marked verified.

## Acceptance criteria

1. The application opens an AUSTRO OS window without launching an external
   browser.
2. The window loads the exact embedded HTML, JavaScript, and CSS assets from
   `internal/webui`.
3. Closing the window exits the desktop process cleanly.
4. Existing root Go tests and browser/Web deployment remain unchanged.
5. A later integration phase starts the application core with a desktop-safe
   lifecycle and local persistence/queue adapters; it must not silently
   bypass PostgreSQL RLS, audit durability, or workspace isolation.

## Architecture boundary

The desktop shell is deliberately separate from the root module's `main.go`.
Do not import or invoke the root `main` package. The next step is to extract a
reusable application composition/lifecycle package from the current API
entrypoint, with explicit API-server and desktop-host adapters. That refactor
must be incremental and preserve the existing Web deployment mode.
