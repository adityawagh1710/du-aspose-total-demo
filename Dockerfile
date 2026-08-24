# Multi-stage Dockerfile for the GO orchestrator (Phase 7 of the Go migration).
#
# Same shape + same /v1 contract as the Python Dockerfile, but the runtime
# stage drops the Python interpreter + uvicorn and runs a single statically
# linked Go binary instead. The C++ Aspose worker builder stage is UNCHANGED —
# Go shells out to the same five per-product binaries over the same JSON-stdio
# protocol.
#
# NOTE (footprint): the runtime CANNOT be scratch/distroless — LibreOffice +
# the Aspose .so trees + fontconfig need a full glibc userland. The Go binary
# is static (CGO_ENABLED=0); the image stays Aspose/LibreOffice-dominated, so
# the net saving over the Python image is roughly the interpreter layer only.
#
# Build:
#     docker build -t office-convert:go -f go.Dockerfile .
#
# Run (identical to the Python image — same port, env, license mount):
#     docker run --rm -p 127.0.0.1:8080:8080 \
#         -v $(pwd)/Aspose.TotalforC++.lic:/aspose/license.lic:ro \
#         --cap-drop=ALL --read-only --tmpfs /tmp --tmpfs /var/run \
#         office-convert:go

# =============================================================================
# Stage 1: C++ builder — compile the five per-product Aspose worker binaries.
# (Identical to the Python Dockerfile's builder stage.)
# =============================================================================
FROM debian:bookworm AS builder

RUN apt-get update \
    && apt-get upgrade -y \
    && apt-get install -y --no-install-recommends \
        gcc-12 g++-12 \
        cmake \
        make \
        build-essential \
        libfontconfig1 \
        libfontconfig-dev \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/*

ENV CC=gcc-12 CXX=g++-12

COPY vendor/aspose/Words  /opt/aspose/Words
COPY vendor/aspose/Cells  /opt/aspose/Cells
COPY vendor/aspose/Slides /opt/aspose/Slides
COPY vendor/aspose/PDF    /opt/aspose/PDF
COPY vendor/aspose/Email  /opt/aspose/Email

WORKDIR /build
COPY worker_cpp/ ./worker_cpp/

RUN cmake -S worker_cpp -B build \
        -DCMAKE_BUILD_TYPE=Release \
        -DVENDOR_ROOT=/opt/aspose \
        -DRUNTIME_ASPOSE_ROOT=/opt/aspose \
    && cmake --build build -j"$(nproc)" \
        --target office-convert-worker-docx \
                 office-convert-worker-pptx \
                 office-convert-worker-xlsx \
                 office-convert-worker-pdf \
                 office-convert-worker-email

# =============================================================================
# Stage 2: Go builder — compile the statically linked orchestrator binary.
# vendor/ (the Aspose C++ libs) is deliberately NOT copied, so Go does not
# enter vendor mode; go.sum drives a reproducible module build.
# =============================================================================
FROM golang:1.27-bookworm AS gobuilder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
# CGO_ENABLED=0 → fully static binary (no glibc link), trimmed symbols.
# The dashboard + landing HTML are baked in via go:embed (internal/server/templates).
RUN CGO_ENABLED=0 GOFLAGS=-mod=mod go build -trimpath -ldflags="-s -w" \
        -o /out/office-convert-orchestrator ./cmd/orchestrator

# =============================================================================
# Stage 3: runtime — debian-slim + qpdf + LibreOffice + Aspose .so + Go binary.
# No Python. Same env + port + non-root posture as the Python image.
# =============================================================================
FROM debian:bookworm-slim AS runtime

RUN apt-get update \
    && apt-get upgrade -y \
    && apt-get install -y --no-install-recommends \
        qpdf \
        util-linux \
        libstdc++6 \
        libgcc-s1 \
        libfontconfig1 \
        libfreetype6 \
        libpng16-16 \
        libexpat1 \
        libuuid1 \
        libxml2 \
        ca-certificates \
        fontconfig \
        fonts-dejavu-core \
        libreoffice-core-nogui \
        libreoffice-draw-nogui \
    && rm -rf /var/lib/apt/lists/* \
    && fc-cache -f

# Non-root user (same uid/gid as the Python image).
RUN groupadd --system --gid 1000 appgroup \
    && useradd --system --uid 1000 --gid appgroup --home /app --shell /usr/sbin/nologin appuser

# The Go orchestrator binary.
COPY --from=gobuilder /out/office-convert-orchestrator /usr/local/bin/office-convert-orchestrator
RUN chmod 755 /usr/local/bin/office-convert-orchestrator

# The five C++ worker binaries (each isolating one Aspose product's CodePorting).
COPY --from=builder /build/build/office-convert-worker-docx  /usr/local/bin/office-convert-worker-docx
COPY --from=builder /build/build/office-convert-worker-pptx  /usr/local/bin/office-convert-worker-pptx
COPY --from=builder /build/build/office-convert-worker-xlsx  /usr/local/bin/office-convert-worker-xlsx
COPY --from=builder /build/build/office-convert-worker-pdf   /usr/local/bin/office-convert-worker-pdf
COPY --from=builder /build/build/office-convert-worker-email /usr/local/bin/office-convert-worker-email
RUN chmod 755 /usr/local/bin/office-convert-worker-*

# Each product's .so tree (per-binary RPATHs reference these exact paths).
COPY --from=builder /opt/aspose/Words/Aspose.Words.Cpp/lib                            /opt/aspose/Words/Aspose.Words.Cpp/lib
COPY --from=builder /opt/aspose/Words/Aspose.Words.Shaping.HarfBuzz.Cpp/lib           /opt/aspose/Words/Aspose.Words.Shaping.HarfBuzz.Cpp/lib
COPY --from=builder /opt/aspose/Words/CodePorting.Translator.Cs2Cpp.Framework/lib     /opt/aspose/Words/CodePorting.Translator.Cs2Cpp.Framework/lib
COPY --from=builder /opt/aspose/Cells/Aspose.Cells/lib                                /opt/aspose/Cells/Aspose.Cells/lib
COPY --from=builder /opt/aspose/Slides/Aspose.Slides.Cpp/lib                          /opt/aspose/Slides/Aspose.Slides.Cpp/lib
COPY --from=builder /opt/aspose/Slides/CodePorting.Translator.Cs2Cpp.Framework/lib    /opt/aspose/Slides/CodePorting.Translator.Cs2Cpp.Framework/lib
COPY --from=builder /opt/aspose/PDF/lib                                                /opt/aspose/PDF/lib
COPY --from=builder /opt/aspose/Email/lib                                              /opt/aspose/Email/lib

# Runtime configuration defaults (operator overrides via -e). Identical to the
# Python image so the Helm chart + ConfigMap apply unchanged.
ENV OFFICE_CONVERT_LICENSE_PATH=/aspose/license.lic \
    OFFICE_CONVERT_WORKER_BINARY_PREFIX=/usr/local/bin/office-convert-worker \
    OFFICE_CONVERT_ASPOSE_LIB_DIR=/opt/aspose \
    OFFICE_CONVERT_SCRATCH_DIR=/tmp/office-convert \
    OFFICE_CONVERT_LOG_FORMAT=json \
    OFFICE_CONVERT_LOG_LEVEL=info \
    OFFICE_CONVERT_MAX_JOBS=1 \
    OFFICE_CONVERT_PARALLEL=2 \
    OFFICE_CONVERT_CHUNK_TIMEOUT_SECONDS=300 \
    OFFICE_CONVERT_MAX_INPUT_BYTES=1073741824 \
    OFFICE_CONVERT_WORKER_RAM_BYTES=4294967296

EXPOSE 8080

# The binary probes its own /health (the runtime image is Python-free and has no
# curl/wget; this works under the read-only rootfs). Mirrors the compose +
# Python-image health timings. Exits 0 only when the server reports ready.
HEALTHCHECK --interval=10s --timeout=5s --start-period=30s --retries=3 \
    CMD ["/usr/local/bin/office-convert-orchestrator", "healthcheck"]

RUN mkdir -p /cache && chown appuser:appgroup /cache

USER appuser
WORKDIR /app

# The Go binary serves on :8080 (OFFICE_CONVERT_PORT overrides) and handles
# SIGTERM with a graceful shutdown, so no separate process manager is needed.
CMD ["/usr/local/bin/office-convert-orchestrator"]
