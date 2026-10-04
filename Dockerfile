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
USER 10001
