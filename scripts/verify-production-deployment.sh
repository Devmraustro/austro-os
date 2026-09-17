#!/usr/bin/env bash
#
# AUSTRO OS — static verification of the production deployment layer.
#
# WHAT THIS IS
#   A structural and policy check over the deployment files: the Dockerfile, the
#   production compose file, both reverse-proxy configurations, the operator
#   scripts and .env.example. It needs no Docker daemon and no Go toolchain, so
#   it runs anywhere — including a CI step that must fail before anything is
#   built.
#
# WHAT THIS IS NOT
#   It is NOT `docker compose config`. It does not resolve `${...}`
#   interpolation, validate against the compose schema, or prove that the stack
#   starts. Those are separate gates (a `docker compose config -q` step and a
#   live smoke job in .github/workflows/production-deployment.yml). Treating a
#   PASS here as "the stack works" would be exactly the kind of unearned claim
#   this project avoids; the checks below only assert what they can actually
#   observe in the files.
#
# Exit status: 0 if every check passes, 1 otherwise. Any FAIL or ERROR fails the
# run. Every check is named so a failure identifies itself.

set -Eeuo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

COMPOSE="docker-compose.production.yml"
DOCKERFILE="Dockerfile"
ENV_EXAMPLE=".env.example"
NGINX_CONF="deploy/nginx/nginx.conf"
CADDYFILE="deploy/caddy/Caddyfile"

FROZEN_FILE="tests/phase1_exit_criteria_test.go"
FROZEN_SHA="0a868d3da41f175cc263a6301f806e3bc1125093dab9cf8db98bb17b54e8769a"

PASS_COUNT=0
FAIL_COUNT=0

pass() { printf 'PASS  %s\n' "$1"; PASS_COUNT=$((PASS_COUNT + 1)); }
fail() { printf 'FAIL  %s\n' "$1" >&2; FAIL_COUNT=$((FAIL_COUNT + 1)); }

# assert_grep <description> <pattern> <file>
assert_grep() {
    if grep -Eq -- "$2" "$3"; then pass "$1"; else fail "$1 (pattern not found: $2)"; fi
}

# assert_no_grep <description> <pattern> <file>
assert_no_grep() {
    if grep -Eq -- "$2" "$3"; then
        fail "$1 (forbidden pattern found: $2)"
    else
        pass "$1"
    fi
}

# assert_no_grep_i <description> <extended-regex> <file>
# Case-insensitive negative assertion. grep -E has no inline (?i) flag, so the
# flag has to be passed as an option rather than embedded in the pattern.
assert_no_grep_i() {
    if grep -Eqi -- "$2" "$3"; then
        fail "$1 (forbidden pattern found: $2)"
    else
        pass "$1"
    fi
}

# assert_fixed <description> <literal> <file>
# Fixed-string assertion, for patterns full of shell-brace and quote characters
# that would otherwise need fragile escaping.
assert_fixed() {
    if grep -Fq -- "$2" "$3"; then pass "$1"; else fail "$1 (not found: $2)"; fi
}

# strip_comments <file>
# Removes comment lines (first non-space character is '#').
#
# Used by the negative assertions over the proxy configs. A directive inside a
# comment has no effect, and both config files deliberately NAME the unsafe
# alternative they are avoiding (for example explaining why the append form of
# X-Forwarded-For is rejected here). Scanning raw text would flag that
# explanation as if it were the misconfiguration it warns about.
strip_comments() { grep -Ev '^[[:space:]]*#' "$1" || true; }

# assert_no_grep_active <description> <pattern> <file>
# Negative assertion over a config file with comments removed.
assert_no_grep_active() {
    if strip_comments "$3" | grep -Eq -- "$2"; then
        fail "$1 (forbidden pattern found in active config: $2)"
    else
        pass "$1"
    fi
}

# assert_file <path>
assert_file() {
    if [[ -f "$1" ]]; then pass "file exists: $1"; else fail "file missing: $1"; fi
}

# ---------------------------------------------------------------------------
printf '\n=== 1. Frozen Phase 1 gate ===\n'
# ---------------------------------------------------------------------------
if [[ ! -f "$FROZEN_FILE" ]]; then
    fail "frozen gate file is missing: $FROZEN_FILE"
else
    actual_sha="$(sha256sum "$FROZEN_FILE" | cut -d' ' -f1)"
    if [[ "$actual_sha" == "$FROZEN_SHA" ]]; then
        pass "frozen gate is byte-identical ($FROZEN_SHA)"
    else
        fail "FROZEN GATE MODIFIED: expected $FROZEN_SHA, got $actual_sha"
    fi
fi

# ---------------------------------------------------------------------------
printf '\n=== 2. Required deliverables exist ===\n'
# ---------------------------------------------------------------------------
for f in \
    docs/deployment-targets.md \
    docs/production-configuration.md \
    docs/production-deployment.md \
    docs/operations.md \
    docs/backups-and-restore.md \
    docs/troubleshooting.md \
    docs/FINAL_PRODUCTION_READINESS_REPORT.md \
    "$COMPOSE" \
    "$NGINX_CONF" \
    "$CADDYFILE" \
    scripts/deploy.sh \
    scripts/healthcheck.sh \
    scripts/backup.sh \
    scripts/restore.sh \
    "$DOCKERFILE" \
    "$ENV_EXAMPLE"; do
    assert_file "$f"
done

# ---------------------------------------------------------------------------
printf '\n=== 3. Prohibited technologies and files ===\n'
# ---------------------------------------------------------------------------
if [[ -e vercel.json ]]; then fail "vercel.json must not exist"; else pass "no vercel.json"; fi
if [[ -d k8s || -d kubernetes || -d deploy/kubernetes ]]; then
    fail "Kubernetes manifests must not exist"
else
    pass "no Kubernetes manifests"
fi
assert_no_grep_i "no Kafka dependency in go.mod" 'kafka|confluent' go.mod
assert_no_grep_i "no Kafka service in production compose" 'kafka' "$COMPOSE"

# ---------------------------------------------------------------------------
printf '\n=== 4. Dockerfile hardening ===\n'
# ---------------------------------------------------------------------------
assert_grep "multi-stage build" '^FROM .+ AS builder' "$DOCKERFILE"
assert_grep "builder image is pinned to the CI Go release" '^FROM golang:1\.25\.13-alpine AS builder' "$DOCKERFILE"
assert_grep "runtime image is a minimal pinned Alpine" '^FROM alpine:3\.' "$DOCKERFILE"
assert_grep "runtime runs as a non-root user" '^USER appuser' "$DOCKERFILE"
assert_no_grep "runtime never switches back to root" '^USER root' "$DOCKERFILE"
assert_grep "exec-form CMD for signal handling" '^CMD \["\./main"\]' "$DOCKERFILE"
assert_grep "HEALTHCHECK present" '^HEALTHCHECK ' "$DOCKERFILE"
assert_grep "ca-certificates installed" 'apk add --no-cache ca-certificates' "$DOCKERFILE"
assert_grep "API binary built" 'go build .* -o /out/main \.' "$DOCKERFILE"
assert_grep "worker binary built from the existing entrypoint" 'go build .* -o /out/worker \./cmd/worker' "$DOCKERFILE"
assert_grep "build path is trimmed" '\-trimpath' "$DOCKERFILE"
assert_no_grep_i "no secret baked into the image" '(ARG|ENV) +[A-Z_]*SECRET=' "$DOCKERFILE"
assert_no_grep_i "no secret baked into the image (password)" '(ARG|ENV) +[A-Z_]*PASSWORD=' "$DOCKERFILE"

# ---------------------------------------------------------------------------
printf '\n=== 5. Production compose — topology ===\n'
# ---------------------------------------------------------------------------
# Service block extractor: prints the lines of one service, stopping at the next
# service or at the top-level `volumes:`/`networks:` keys. Service keys are the
# only entries indented by exactly two spaces under `services:`.
service_block() {
    awk -v svc="$1" '
        /^(volumes|networks):/ { inblock = 0; next }
        /^  [A-Za-z0-9_.-]+:/ {
            name = $0
            sub(/^  /, "", name)
            sub(/:.*/, "", name)
            inblock = (name == svc)
        }
        inblock { print }
    ' "$COMPOSE"
}

for svc in postgres redis rabbitmq api worker reverse-proxy; do
    if service_block "$svc" | grep -q .; then
        pass "compose service present: $svc"
    else
        fail "compose service missing: $svc"
    fi
done

for svc in postgres redis rabbitmq api worker; do
    if service_block "$svc" | grep -Eq '^    ports:'; then
        fail "$svc must not publish ports"
    else
        pass "$svc publishes no ports"
    fi
done

assert_fixed "reverse proxy publishes port 80 publicly" '"${AUSTRO_HTTP_PORT:-80}:80"' <(service_block reverse-proxy)
assert_fixed "reverse proxy publishes port 443 publicly" '"${AUSTRO_HTTPS_PORT:-443}:443"' <(service_block reverse-proxy)

for svc in postgres redis rabbitmq api worker reverse-proxy; do
    if service_block "$svc" | grep -Eq '^    restart: unless-stopped'; then
        pass "$svc has restart: unless-stopped"
    else
        fail "$svc is missing restart: unless-stopped"
    fi
done

for svc in postgres redis rabbitmq api worker reverse-proxy; do
    if service_block "$svc" | grep -Eq '^    healthcheck:'; then
        pass "$svc defines a healthcheck"
    else
        fail "$svc is missing a healthcheck"
    fi
done

for svc in api worker; do
    block="$(service_block "$svc")"
    if grep -Eq '^    depends_on:' <<<"$block" && grep -Eq 'condition: service_healthy' <<<"$block"; then
        pass "$svc depends on healthy dependencies"
    else
        fail "$svc must gate on service_healthy dependencies"
    fi
done

assert_grep "API health depends on postgres being healthy" 'service_healthy' <(service_block api)
assert_grep "worker waits for the API (serialises the cold-start migration)" 'api:' <(service_block worker)
assert_grep "API graceful stop exceeds the 75s in-flight drain" 'stop_grace_period: 90s' <(service_block api)
assert_grep "application environment is production" 'AUSTRO_ENV: "production"' "$COMPOSE"
assert_grep "postgres is the pgvector image" 'pgvector/pgvector:pg16' "$COMPOSE"
assert_grep "rabbitmq healthcheck uses rabbitmqctl status" 'rabbitmqctl' "$COMPOSE"
assert_grep "redis healthcheck is an authenticated ping" 'redis-cli .*ping' "$COMPOSE"
assert_no_grep "rabbitmq management image is not used" 'rabbitmq:[0-9.]+-management' "$COMPOSE"

# Regression guard. Compose interpolates the WHOLE file, including services
# behind an inactive profile, so a `:?` required variable on a profiled service
# makes every command that merely resolves the file fail — including a plain
# nginx deployment that never enables that profile. CI found exactly this: the
# production smoke job's minimal environment file could not resolve the compose
# file because the Caddy service demanded its ACME variables. Required variables
# belong to services on the default path; a profile-only requirement is enforced
# by scripts/deploy.sh, where it actually applies.
# Comments are stripped first: the block deliberately NAMES the `:?` form it
# is avoiding, and scanning raw text would flag that explanation as the defect.
if strip_comments <(service_block reverse-proxy-caddy) | grep -qF ':?'; then
    fail "reverse-proxy-caddy declares a ':?' required variable, which breaks the default nginx path"
else
    pass "profiled Caddy service declares no ':?' variable (the default path stays resolvable)"
fi

assert_grep "deploy.sh enforces the proxy prerequisites where they apply" 'AUSTRO_PUBLIC_HOSTNAME is required' scripts/deploy.sh

# ---------------------------------------------------------------------------
printf '\n=== 5b. Production compose — structural lint ===\n'
# ---------------------------------------------------------------------------
# This is NOT `docker compose config` and does not claim to be: it resolves no
# anchors, no `${...}` interpolation and no compose schema. `docker compose
# config -q` remains a separate gate in CI, where a daemon exists. What this
# catches is the syntax class that would otherwise only surface at `up` time —
# a tab character, an unbalanced quote, a key repeated inside its own parent, a
# block opened where no parent can hold it — and it runs in environments (like
# the one this was authored in) that have no Docker at all.
COMPOSE_LINT_EXPECTED="postgres,redis,rabbitmq,api,worker,reverse-proxy,reverse-proxy-caddy"

# The linter is written to a temp file once and reused by sections 5b and 5c.
COMPOSE_LINT="$(mktemp)"
COMPOSE_LINT_PROBE="$(mktemp)"
trap 'rm -f "$COMPOSE_LINT" "$COMPOSE_LINT_PROBE"' EXIT

if ! command -v python3 >/dev/null 2>&1; then
    # Not a pass: the check could not run at all.
    fail "YAML structural lint could not run (python3 unavailable) — BLOCKED"
else
    cat >"$COMPOSE_LINT" <<'PYTHON_LINTER_EOF'
#!/usr/bin/env python3
"""Structural lint for a docker compose YAML file.

Not a YAML parser and not a substitute for `docker compose config`: it resolves
no anchors, no `${...}` interpolation and no schema. It catches the class of
mistake grep cannot see and that would otherwise surface only when docker runs:
tab characters, indentation that is not a whole number of levels, a block opened
where its parent cannot hold one, unbalanced quotes, a key repeated inside its
own parent, and a service list that has drifted from what is documented.
"""
import re
import sys

MAPPING = re.compile(r'^(<<|[A-Za-z0-9_.\-]+):(\s+.*)?$')
LIST_ITEM = re.compile(r'^-(\s+.*)?$')
LIST_MAPPING = re.compile(r'^-\s+[A-Za-z0-9_.\-]+:(\s|$)')
ALLOWED_TOP = {"name", "services", "volumes", "networks"}


def unquote_ok(text):
    """True when every quote on the line is closed (crude, but catches typos)."""
    return text.count('"') % 2 == 0 and text.count("'") % 2 == 0


def check(path, expected_services, allowed_top=None):
    allowed = set(allowed_top) if allowed_top else set(ALLOWED_TOP)
    problems = []
    text = open(path, encoding="utf-8").read()
    seen = {}            # (ancestry path, indent, key) -> first line number
    stack = []           # [(indent, key)] open ancestors
    top_level = []
    in_services = False
    services = []
    prev_indent = 0
    prev_opens = False
    # Indent of an open block scalar (`key: |` or `key: >`). Its following,
    # more-indented lines are literal content and must not be parsed as YAML.
    block_scalar_indent = None

    for number, raw in enumerate(text.split("\n"), 1):
        if "\t" in raw:
            problems.append(f"line {number}: tab character (YAML forbids tab indentation)")
            continue
        line = raw.rstrip()
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        indent = len(line) - len(line.lstrip(" "))
        body = line.lstrip(" ")

        if block_scalar_indent is not None:
            if indent > block_scalar_indent:
                continue          # literal content of the block scalar
            block_scalar_indent = None   # the block ended; parse this line normally

        if indent % 2:
            problems.append(f"line {number}: indentation {indent} is not a multiple of 2")
        if not unquote_ok(body):
            problems.append(f"line {number}: unbalanced quotes: {body}")

        match = MAPPING.match(body)
        value = (match.group(2) or "").strip() if match else ""
        # A key opens a block when it carries no scalar value, or when its value
        # is an anchor (`key: &anchor`) that its children attach to.
        opens = (bool(match) and (value == "" or value.startswith("&"))) or bool(LIST_MAPPING.match(body))
        if match and re.match(r'^[|>][+-]?[0-9]?$', value):
            # `key: |`, `key: >-`, ... — the value is a block scalar.
            block_scalar_indent = indent
            opens = False

        if indent == 0:
            if not match:
                problems.append(f"line {number}: top-level line is not a mapping key: {body}")
            else:
                key = body.split(":")[0]
                top_level.append(key)
                if key in seen:
                    problems.append(f"line {number}: duplicate top-level key '{key}'")
                seen[(0, key)] = number
            stack = []
            in_services = body.startswith("services:")
        else:
            if not (match or LIST_ITEM.match(body)):
                problems.append(f"line {number}: neither a mapping entry nor a list item: {body}")
            if indent > prev_indent and not prev_opens:
                problems.append(
                    f"line {number}: indentation deepened from {prev_indent} to {indent} "
                    f"without a parent block: {body}"
                )

        if indent == 0:
            pass
        elif match:
            key = body.split(":")[0]
            while stack and stack[-1][0] >= indent:
                stack.pop()
            ancestry = ".".join(k for _, k in stack)
            marker = (ancestry, indent, key)
            if marker in seen:
                problems.append(
                    f"line {number}: duplicate key '{key}' under '{ancestry or '<root>'}' "
                    f"(first seen on line {seen[marker]})"
                )
            seen[marker] = number
            if opens:
                stack.append((indent, key))
        else:
            while stack and stack[-1][0] >= indent:
                stack.pop()
            if LIST_MAPPING.match(body):
                # A list item that opens a mapping (`- name: x`) is its own node:
                # the keys indented beneath it belong to THAT item, not to the
                # list's parent. The line number is the discriminator, so two
                # items whose opening text is identical (the same `uses:` step
                # in two places) stay distinct nodes instead of colliding.
                stack.append((indent, f"{body.split(':')[0]}#L{number}"))

        # Service names are the mapping keys directly under `services:`.
        if in_services and indent == 2 and re.match(r'^[A-Za-z0-9_.\-]+:$', body):
            services.append(body[:-1])

        prev_indent, prev_opens = indent, opens

    for key in top_level:
        if key not in allowed and not key.startswith("x-"):
            problems.append(f"unexpected top-level key '{key}'")
    if expected_services and "services" not in top_level:
        problems.append("file declares no services")

    if expected_services:
        missing = [s for s in expected_services if s not in services]
        extra = [s for s in services if s not in expected_services]
        if missing:
            problems.append("services missing: " + ", ".join(missing))
        if extra:
            problems.append("undeclared extra services: " + ", ".join(extra))

    return problems, services


if __name__ == "__main__":
    target = sys.argv[1]
    expected = sys.argv[2].split(",") if len(sys.argv) > 2 and sys.argv[2] else None
    # Third argument, when given, replaces the compose-specific top-level key
    # allowlist so the same structural checks can cover another YAML file.
    allowed_top = sys.argv[3].split(",") if len(sys.argv) > 3 and sys.argv[3] else None
    issues, found = check(target, expected, allowed_top)
    print(f"services found: {', '.join(found) if found else '(none)'}")
    for issue in issues:
        print(f"LINT ERROR: {issue}", file=sys.stderr)
    if issues:
        print(f"compose lint: FAIL ({len(issues)} problem(s))", file=sys.stderr)
        sys.exit(1)
    print("compose lint: PASS")
PYTHON_LINTER_EOF

    if python3 "$COMPOSE_LINT" "$COMPOSE" "$COMPOSE_LINT_EXPECTED" >/dev/null 2>&1; then
        pass "production compose parses as structurally well-formed YAML"
    else
        fail "production compose failed structural lint:"
        python3 "$COMPOSE_LINT" "$COMPOSE" "$COMPOSE_LINT_EXPECTED" >&2 || true
    fi

    # Negative control. A linter that cannot fail proves nothing, so two real
    # faults are injected into a copy and the linter MUST reject it.
    cp "$COMPOSE" "$COMPOSE_LINT_PROBE"
    printf '\tbug: 1\n' >>"$COMPOSE_LINT_PROBE"
    if python3 "$COMPOSE_LINT" "$COMPOSE_LINT_PROBE" "$COMPOSE_LINT_EXPECTED" >/dev/null 2>&1; then
        fail "compose linter accepted a deliberately broken file (check is vacuous)"
    else
        pass "compose linter rejects a deliberately broken file (negative control)"
    fi
fi

# ---------------------------------------------------------------------------
printf '\n=== 5c. CI workflow lint ===\n'
# ---------------------------------------------------------------------------
# The production CI workflow is YAML with the same failure mode as the compose
# file: a structural error is invisible until GitHub tries to run it. The same
# linter is reused with the workflow's own top-level key set.
WORKFLOW_FILE=".github/workflows/production-deployment.yml"
if [[ ! -f "$WORKFLOW_FILE" ]]; then
    fail "file missing: $WORKFLOW_FILE"
elif ! command -v python3 >/dev/null 2>&1; then
    fail "workflow lint could not run (python3 unavailable) — BLOCKED"
elif python3 "$COMPOSE_LINT" "$WORKFLOW_FILE" "" "name,on,permissions,env,jobs" >/dev/null 2>&1; then
    pass "CI workflow parses as structurally well-formed YAML"
else
    fail "CI workflow failed structural lint:"
    python3 "$COMPOSE_LINT" "$WORKFLOW_FILE" "" "name,on,permissions,env,jobs" >&2 || true
fi

# ---------------------------------------------------------------------------
printf '\n=== 6. Production compose — security posture ===\n'
# ---------------------------------------------------------------------------
assert_no_grep "no privileged containers" 'privileged: *true' "$COMPOSE"
assert_no_grep "no host network namespace" 'network_mode: *host' "$COMPOSE"
assert_no_grep "docker socket is not mounted" '/var/run/docker\.sock' "$COMPOSE"
assert_grep "internal network is declared internal" 'internal: true' "$COMPOSE"
assert_grep "api runs read-only" 'read_only: true' <(service_block api)
assert_grep "worker runs read-only" 'read_only: true' <(service_block worker)
assert_grep "api disables privilege escalation" 'no-new-privileges:true' <(service_block api)
assert_grep "worker disables privilege escalation" 'no-new-privileges:true' <(service_block worker)
assert_grep "postgres data is on a named volume" 'postgres_data:/var/lib/postgresql/data' "$COMPOSE"
assert_grep "redis data is on a named volume" 'redis_data:/data' "$COMPOSE"
assert_grep "rabbitmq data is on a named volume" 'rabbitmq_data:/var/lib/rabbitmq' "$COMPOSE"
assert_grep "required secrets fail the compose parse when absent" '\$\{[A-Z_]+:\?' "$COMPOSE"

for svc in postgres redis rabbitmq; do
    if service_block "$svc" | grep -Eq 'austro_internal'; then
        pass "$svc is attached to the internal network"
    else
        fail "$svc must be attached to the internal network"
    fi
done

# ---------------------------------------------------------------------------
printf '\n=== 7. Reverse proxy configuration ===\n'
# ---------------------------------------------------------------------------
assert_grep "nginx proxies to the api service" 'server api:8080;' "$NGINX_CONF"
assert_grep "nginx redirects HTTP to HTTPS" 'return 301 https://\$host\$request_uri;' "$NGINX_CONF"
assert_grep "nginx TLS floor is 1.2" 'ssl_protocols TLSv1\.2 TLSv1\.3;' "$NGINX_CONF"
assert_grep "nginx sends HSTS" 'Strict-Transport-Security' "$NGINX_CONF"
assert_grep "nginx sets nosniff" 'X-Content-Type-Options' "$NGINX_CONF"
assert_grep "nginx caps request bodies above the Go 1 MiB limit" 'client_max_body_size 2m;' "$NGINX_CONF"
assert_grep "nginx sets proxy timeouts" 'proxy_read_timeout' "$NGINX_CONF"
# The append form would preserve client-supplied X-Forwarded-For values.
assert_no_grep_active "nginx does not append client X-Forwarded-For" 'proxy_add_x_forwarded_for' "$NGINX_CONF"
assert_grep "nginx overwrites X-Forwarded-For from the observed peer" 'X-Forwarded-For +\$remote_addr;' "$NGINX_CONF"

assert_grep "caddy proxies to the api service" 'reverse_proxy api:8080' "$CADDYFILE"
assert_grep "caddy TLS floor is 1.2" 'protocols tls1\.2 tls1\.3' "$CADDYFILE"
assert_grep "caddy sends HSTS" 'Strict-Transport-Security' "$CADDYFILE"
assert_grep "caddy caps request bodies" 'max_size 2MB' "$CADDYFILE"
assert_no_grep_active "caddy does not trust third-party proxies" 'trusted_proxies' "$CADDYFILE"
assert_grep "caddy overwrites client X-Forwarded-For" 'header_up X-Forwarded-For +\{remote_host\}' "$CADDYFILE"

# ---------------------------------------------------------------------------
printf '\n=== 8. Operator scripts ===\n'
# ---------------------------------------------------------------------------
for s in scripts/deploy.sh scripts/healthcheck.sh scripts/backup.sh scripts/restore.sh \
         scripts/verify-production-deployment.sh; do
    if [[ ! -f "$s" ]]; then
        fail "script missing: $s"
        continue
    fi
    if bash -n "$s" 2>/dev/null; then pass "shell syntax valid: $s"; else fail "shell syntax error: $s"; fi
    if grep -Eq '^set -[a-zA-Z]*E?e[a-zA-Z]*u[a-zA-Z]*o' "$s"; then
        pass "strict mode enabled: $s"
    else
        fail "strict mode (set -euo pipefail) missing: $s"
    fi
    # `set -x` would trace expansions into CI logs, including secrets.
    if grep -Eq '^\s*set -x' "$s"; then fail "$s enables shell tracing (set -x)"; else pass "$s does not enable set -x"; fi
    if [[ -x "$s" ]]; then pass "script is executable: $s"; else fail "script is not executable: $s"; fi
done

# ---------------------------------------------------------------------------
printf '\n=== 9. Repository hygiene and secret scanning ===\n'
# ---------------------------------------------------------------------------
assert_no_grep "no private key material in the Dockerfile" 'BEGIN [A-Z ]*PRIVATE KEY' "$DOCKERFILE"

# Scan every file this change introduces or modifies for secret-shaped values.
scanned=0
secret_hits=0
for f in "$DOCKERFILE" "$ENV_EXAMPLE" "$COMPOSE" "$NGINX_CONF" "$CADDYFILE"; do
    [[ -f "$f" ]] || continue
    scanned=$((scanned + 1))
    if grep -Eq 'BEGIN [A-Z ]*PRIVATE KEY|AKIA[0-9A-Z]{16}|sk-[A-Za-z0-9]{20,}' "$f"; then
        fail "secret-shaped value found in $f"
        secret_hits=$((secret_hits + 1))
    fi
done
if [[ "$secret_hits" -eq 0 ]]; then pass "no secret-shaped values across $scanned deployment files"; fi

# .env.example must stay a template. The frozen Phase-1 gate asserts these two
# properties, so they are checked here as well: a placeholder must survive, and
# a real-looking secret must never appear.
if grep -q 'change-me' "$ENV_EXAMPLE"; then
    pass ".env.example still carries change-me placeholders"
else
    fail ".env.example lost its placeholders (the frozen gate requires change-me)"
fi
if grep -q 'production-secret-value' "$ENV_EXAMPLE"; then
    fail ".env.example contains production-secret-value (forbidden by the frozen gate)"
else
    pass ".env.example contains no production-secret-value"
fi

# A committed .env / key / certificate is a leak regardless of content.
tracked_secrets="$(git ls-files -- '.env' '.env.production' '*.pem' '*.key' '*.p12' '*.pfx' 2>/dev/null || true)"
if [[ -n "$tracked_secrets" ]]; then
    fail "secret files are tracked by git: $tracked_secrets"
else
    pass "no .env, key or certificate files are tracked by git"
fi

# ---------------------------------------------------------------------------
printf '\n=== 10. Configuration contract coverage ===\n'
# ---------------------------------------------------------------------------
# Every variable the application actually reads must be documented, or an
# operator cannot know it exists. This list is derived from internal/config and
# the infrastructure packages, not from the documentation.
for var in \
    AUSTRO_POSTGRES_DSN \
    AUSTRO_POSTGRES_PASSWORD \
    AUSTRO_POSTGRES_RUNTIME_PASSWORD \
    AUSTRO_POSTGRES_RUNTIME_DSN \
    AUSTRO_POSTGRES_RUNTIME_USER \
    AUSTRO_REDIS_ADDR \
    AUSTRO_REDIS_PASSWORD \
    AUSTRO_RABBITMQ_USER \
    AUSTRO_RABBITMQ_PASSWORD \
    AUSTRO_RABBITMQ_URL \
    AUSTRO_RABBITMQ_QUEUE \
    AUSTRO_JWT_SECRET \
    AUSTRO_JWT_REFRESH_SECRET \
    AUSTRO_FOUNDER_USERNAME \
    AUSTRO_FOUNDER_PASSWORD \
    AUSTRO_AI_BACKEND \
    AUSTRO_AI_MODEL \
    AUSTRO_AI_BASE_URL \
    AUSTRO_AI_API_KEY \
    AUSTRO_PUBLISH_BACKEND \
    AUSTRO_PUBLISH_WEBHOOK_URL \
    AUSTRO_PUBLISH_TOKEN \
    AUSTRO_ENV; do
    if grep -q "$var" "$ENV_EXAMPLE"; then
        pass "documented in .env.example: $var"
    else
        fail "undocumented in .env.example: $var"
    fi
done

# ---------------------------------------------------------------------------
printf '\n=== SUMMARY ===\n'
# ---------------------------------------------------------------------------
printf 'checks passed: %d\n' "$PASS_COUNT"
printf 'checks failed: %d\n' "$FAIL_COUNT"
if [[ "$FAIL_COUNT" -gt 0 ]]; then
    printf '\nPRODUCTION DEPLOYMENT STATIC VERIFICATION: FAIL\n' >&2
    exit 1
fi
printf '\nPRODUCTION DEPLOYMENT STATIC VERIFICATION: PASS\n'
printf 'NOTE: this does not run `docker compose config` or start the stack.\n'
exit 0
