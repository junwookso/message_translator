#!/bin/sh
set -eu

MODEL_PARALLEL=${MODEL_PARALLEL:-2}
case "$MODEL_PARALLEL" in
    ''|*[!0-9]*) echo 'MODEL_PARALLEL must be a positive integer' >&2; exit 2 ;;
esac
while [ "${MODEL_PARALLEL#0}" != "$MODEL_PARALLEL" ]; do
    MODEL_PARALLEL=${MODEL_PARALLEL#0}
done
MODEL_PARALLEL=${MODEL_PARALLEL:-0}
if [ "$MODEL_PARALLEL" -lt 1 ] || [ "$MODEL_PARALLEL" -gt 524287 ]; then
    echo 'MODEL_PARALLEL must be between 1 and 524287 (context size limit)' >&2
    exit 2
fi

MODEL_REPO=${MODEL_REPO:-google/gemma-4-E4B-it-qat-q4_0-gguf}
MODEL_REVISION=${MODEL_REVISION:-4b4a2c1d584be7264f87aac328a1bc739ce81b6c}
MODEL_FILE=${MODEL_FILE:-gemma-4-E4B_q4_0-it.gguf}
case "$MODEL_REVISION" in
    *[!a-f0-9]*|'') echo 'MODEL_REVISION must be a full lowercase commit SHA' >&2; exit 2 ;;
esac
if [ "${#MODEL_REVISION}" -ne 40 ]; then
    echo 'MODEL_REVISION must be a full lowercase commit SHA' >&2; exit 2
fi
case "$MODEL_REPO" in
    *[!A-Za-z0-9_./-]*|*..*|/*|*/|'') echo 'Invalid MODEL_REPO' >&2; exit 2 ;;
esac
case "$MODEL_FILE" in
    *[!A-Za-z0-9_.-]*|*..*|'') echo 'MODEL_FILE must be a GGUF basename' >&2; exit 2 ;;
esac
# Include repository and revision in the local path to avoid reusing a different artifact.
model_dir="${LLAMA_CACHE:-/models}/$MODEL_REPO/$MODEL_REVISION"
mkdir -p "$model_dir"
model_url="https://huggingface.co/$MODEL_REPO/resolve/$MODEL_REVISION/$MODEL_FILE"
# Preserve 4096 tokens per slot when using a fixed total KV context size.
exec /app/llama-server --model "$model_dir/$MODEL_FILE" --model-url "$model_url" --ctx-size "$((MODEL_PARALLEL * 4096))" --parallel "$MODEL_PARALLEL" "$@"
