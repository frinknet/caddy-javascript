#!/bin/sh
set -e

cd "$(dirname "$0")"

exec caddy run --config Caddyfile --adapter caddyfile
