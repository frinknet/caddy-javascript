# ---------------------------------------------------------------------------
# Stage A: Aquire toybox, SSL certs, tzdata. No source dependency.
# ---------------------------------------------------------------------------
FROM caddy:2.11.9-builder-alpine AS build

RUN apk add --no-cache ca-certificates tzdata curl upx

ARG TARGETARCH
RUN case "${TARGETARCH}" in \
      "amd64") TOYBOX_ARCH="x86_64" ;; \
      "arm64") TOYBOX_ARCH="aarch64" ;; \
      *) echo "Unsupported arch: ${TARGETARCH}" && exit 1 ;; \
    esac \
    && mkdir -p /out/licenses/caddy /out/licenses/toybox \
    && curl -fsSL "https://codeberg.org/landley/toybox/raw/branch/master/LICENSE" -o /out/licenses/toybox/LICENSE \
    && curl -fsSL "https://landley.net/toybox/bin/toybox-${TOYBOX_ARCH}" -o /out/toybox \
    && chmod +x /out/toybox \
    && upx --best --lzma /out/toybox

# ---------------------------------------------------------------------------
# Stage B: Build caddy binary. Source-dependent, uses shared go cache.
# ---------------------------------------------------------------------------
WORKDIR /build
COPY . /src/caddy-javascript

RUN --mount=type=cache,id=gomod,target=/go/pkg/mod \
    --mount=type=cache,id=gobuild,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux \
    xcaddy build \
    --with github.com/frinknet/caddy-javascript \
    --replace github.com/frinknet/caddy-javascript=/src/caddy-javascript \
    --output /out/caddy

# Collect third-party licenses, then compress. Order matters: upx rewrites the
# ELF and strips the Go build info that `go version -m` reads to enumerate the
# linked modules, and the exit status of that command is checked so a silent
# empty list cannot slip through again.
RUN --mount=type=cache,id=gomod,target=/go/pkg/mod \
    GOMODCACHE=$(go env GOMODCACHE) && \
    OUT=/out/licenses && \
    BIN=/out/caddy && \
    SRC=/src/caddy-javascript && \
    rm -rf "$OUT/caddy" "$OUT/plugins" "$OUT/MISSING" && \
    mkdir -p "$OUT/caddy" "$OUT/plugins" && \
    : > "$OUT/MISSING" && \
    for f in "$GOMODCACHE"/github.com/caddyserver/caddy/v2@*/LICENSE*; do \
        [ -f "$f" ] || continue; \
        cp "$f" "$OUT/caddy/"; \
    done && \
    go version -m "$BIN" > /tmp/deps.txt && \
    awk '$1 == "dep" { print $2, $3 }' /tmp/deps.txt | sort -u | \
    while read -r mod ver; do \
        escaped=$(printf '%s' "$mod@$ver" | sed 's/\([A-Z]\)/!\1/g' | tr 'A-Z' 'a-z'); \
        dir="$GOMODCACHE/$escaped"; \
        [ -d "$dir" ] || continue; \
        dest="$OUT/plugins/$(printf '%s' "$mod" | tr / _)"; \
        mkdir -p "$dest"; \
        find "$dir" -maxdepth 1 -type f \
            \( -iname 'licen[cs]e*' -o -iname 'copying*' \
            -o -iname 'notice*' -o -iname 'unlicense*' \) \
            -exec cp {} "$dest/" \; ; \
        [ -n "$(ls -A "$dest")" ] || { rmdir "$dest"; echo "$mod" >> "$OUT/MISSING"; }; \
    done && \
    mod=$(awk '$1 == "module" { print $2; exit }' "$SRC/go.mod") && \
    dest="$OUT/plugins/$(printf '%s' "$mod" | tr / _)" && \
    mkdir -p "$dest" && \
    find "$SRC" -maxdepth 1 -type f \
        \( -iname 'licen[cs]e*' -o -iname 'copying*' \
        -o -iname 'notice*' -o -iname 'unlicense*' \) \
        -exec cp {} "$dest/" \; && \
    sort -u -o "$OUT/MISSING" "$OUT/MISSING" && \
    echo "licenses: $(find "$OUT/plugins" -mindepth 1 -type d | wc -l) modules, $(find "$OUT/plugins" -type f | wc -l) files" && \
    { [ ! -s "$OUT/MISSING" ] || sed 's/^/  ships no license text: /' "$OUT/MISSING"; } && \
    upx --best --lzma "$BIN"

# ---------------------------------------------------------------------------
# Stage C: Compose scratch image
# ---------------------------------------------------------------------------
FROM scratch

COPY --from=build /out/licenses             /usr/share/licenses
COPY --from=build /etc/ssl/certs           /etc/ssl/certs
COPY --from=build /usr/share/zoneinfo      /usr/share/zoneinfo
COPY --from=build /out/caddy               /bin/caddy
COPY --from=build /out/toybox              /bin/toybox
COPY             www                       /srv/www

RUN ["/bin/toybox", "sh", "-c", "cd /bin && for i in $(./toybox); do if [ \"$i\" != toybox ]; then ./toybox ln -s toybox \"$i\"; fi; done && test -L /bin/sh && test -L /bin/ls"]
RUN mkdir -p /etc/caddy /var/lib/caddy /var/cache/caddy /tmp && chmod 1777 /tmp
RUN mv /srv/www/Caddyfile /etc/caddy/Caddyfile

VOLUME ["/var/lib/caddy"]

ENV PATH="/bin:/usr/bin:/usr/local/bin"
ENV XDG_CONFIG_HOME="/etc/caddy"
ENV XDG_DATA_HOME="/var/lib/caddy"
ENV XDG_CACHE_HOME="/var/cache/caddy"
ENV TMPDIR="/tmp"
ENV HOME="/tmp"

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD /bin/toybox wget -O /dev/null -T 2 http://127.0.0.1:2019/config/ >/dev/null 2>&1

EXPOSE 80 443 443/udp

WORKDIR /srv/www
ENTRYPOINT ["/bin/caddy"]
CMD ["run", "--config", "/etc/caddy/Caddyfile", "--adapter", "caddyfile"]
