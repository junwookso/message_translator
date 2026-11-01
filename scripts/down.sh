#!/bin/sh
set -eu

CONTAINER_ENGINE=${CONTAINER_ENGINE:-podman}
case "$CONTAINER_ENGINE" in
    podman|docker) ;;
    *) echo 'CONTAINER_ENGINE must be podman or docker' >&2; exit 2 ;;
esac

if [ "$#" -ne 0 ]; then
    echo 'Usage: CONTAINER_ENGINE=podman scripts/down.sh (no arguments; volumes are preserved)' >&2
    exit 2
fi

cd "$(dirname "$0")/.."
# Wait for API shutdown before down can stop the model, even with parallel Compose providers.
"$CONTAINER_ENGINE" compose stop api
"$CONTAINER_ENGINE" compose down
