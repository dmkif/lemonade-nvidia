# lemonade-guard (guard/): reverse proxy in front of Lemonade, see specs/008 in dmkif/k3s-gitops.
# Tests run in the build: a failing `go vet` or `go test` fails the image build (also on pull requests).
FROM docker.io/library/golang:1.26-bookworm@sha256:a688600ca24f8a4d3ca77f95b0dd40704a9fc787c826660eb7ba0b641b8b175d AS guard
WORKDIR /src
COPY guard/ .
RUN go vet ./... && go test -count=1 ./... \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /guard .

# Lemonade with the two libraries NVIDIA's Vulkan ICD needs inside the container.
# The upstream image ships libvulkan1 and libX11 but not libEGL (libglvnd) or libXext;
# without them the NVIDIA ICD fails with "Could not get 'vkCreateInstance'" and
# Lemonade's Vulkan backend falls back to llvmpipe (CPU).
# Drop this part once upstream includes libegl1 + libxext6.
#
# ace-server-lowvram.sh: Lemonade always starts ace-server with --keep-loaded, which keeps the LM
# (4.2 GB), the DiT (4.2 GB) and the text encoder resident. On an 11 GB card every sung generation
# then fails ("synth job ... failed"); instrumental works. The wrapper drops the flag so each stage
# frees its memory. Select it with acestep.vulkan_bin in Lemonade's config.json.
FROM ghcr.io/lemonade-sdk/lemonade-server:v2026.40.0@sha256:7a2822a677bd84665683b19629dd8ce44af45774431b62c09d107c2e856a7acc

USER root
RUN apt-get update \
 && apt-get install -y --no-install-recommends libegl1 libxext6 \
 && rm -rf /var/lib/apt/lists/*
COPY --chmod=0755 ace-server-lowvram.sh /opt/lemonade/ace-server-lowvram.sh
COPY --from=guard --chmod=0755 /guard /opt/lemonade/guard
USER 10001
