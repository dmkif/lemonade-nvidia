# Lemonade with the two libraries NVIDIA's Vulkan ICD needs inside the container.
# The upstream image ships libvulkan1 and libX11 but not libEGL (libglvnd) or libXext;
# without them the NVIDIA ICD fails with "Could not get 'vkCreateInstance'" and
# Lemonade's Vulkan backend falls back to llvmpipe (CPU).
# Drop this image once upstream includes libegl1 + libxext6.
FROM ghcr.io/lemonade-sdk/lemonade-server:v2026.40.0@sha256:7a2822a677bd84665683b19629dd8ce44af45774431b62c09d107c2e856a7acc

USER root
RUN apt-get update \
 && apt-get install -y --no-install-recommends libegl1 libxext6 \
 && rm -rf /var/lib/apt/lists/*
USER 10001
