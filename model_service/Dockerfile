ARG LLAMA_CPP_IMAGE=ghcr.io/ggml-org/llama.cpp@sha256:c50671c463e746cdccf2c6e77ffd6d77dca2e95290e54ca5ba5c7f524c33e234
FROM ${LLAMA_CPP_IMAGE}

ENV LLAMA_CACHE=/models \
    MODEL_REPO=google/gemma-4-E4B-it-qat-q4_0-gguf \
    MODEL_REVISION=4b4a2c1d584be7264f87aac328a1bc739ce81b6c \
    MODEL_FILE=gemma-4-E4B_q4_0-it.gguf \
    LLAMA_ARG_ALIAS=GEMMA_4_E4B \
    MODEL_PARALLEL=2

COPY start.sh /app/start.sh
ENTRYPOINT ["/bin/sh", "/app/start.sh"]

EXPOSE 8080

HEALTHCHECK --interval=10s --timeout=5s --start-period=30m --retries=6 \
    CMD curl --fail --silent http://localhost:8080/health || exit 1

CMD ["--no-mmproj", "--host", "0.0.0.0", "--port", "8080", "--gpu-layers", "0", "--reasoning", "off", "--reasoning-budget", "0"]
