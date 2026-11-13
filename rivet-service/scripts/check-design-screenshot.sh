#!/bin/sh
# Opt-in nonroot fixture: no production containers or policies are changed.
set -eu
rivet_design_image=${1:?Usage: check-design-screenshot.sh IMAGE}
case "$rivet_design_image" in -*) echo 'image must not start with -' >&2; exit 2;; esac
rivet_design_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
rivet_design_arch=$(docker image inspect --format '{{.Architecture}}' "$rivet_design_image")
case "$rivet_design_arch" in arm64|amd64) ;; *) echo 'unsupported fixture image architecture' >&2; exit 2;; esac
rivet_design_tmp=$(mktemp -d)
trap 'rm -rf -- "$rivet_design_tmp"' EXIT HUP INT TERM
(cd "$rivet_design_root" && GOOS=linux GOARCH="$rivet_design_arch" CGO_ENABLED=0 go test -c ./internal/design -o "$rivet_design_tmp/fixture")
chmod 755 "$rivet_design_tmp/fixture"
docker run --rm --network none --memory 1g --cpus 2 --read-only \
 --tmpfs /tmp:rw,nosuid,nodev,size=256m \
 --security-opt "seccomp=$rivet_design_root/scripts/design-chromium-seccomp.json" \
 --mount "type=bind,src=$rivet_design_tmp/fixture,dst=/fixture,readonly" \
 --env RIVET_DESIGN_CHROMIUM_TEST=/usr/bin/chromium-browser \
 --entrypoint /fixture "$rivet_design_image" -test.run '^TestChromiumRealScreenshot$' -test.v
