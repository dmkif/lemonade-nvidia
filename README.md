# lemonade-nvidia

Thin layer over [`ghcr.io/lemonade-sdk/lemonade-server`](https://github.com/lemonade-sdk/lemonade)
that adds `libegl1` and `libxext6`. NVIDIA's Vulkan ICD (injected by nvidia-container-toolkit)
needs both inside the container; the upstream image ships neither, so Lemonade's Vulkan backend
only sees `llvmpipe`.

Used on the k3s GPU worker `apps-04` (GTX 1080 Ti, Pascal: CUDA backend needs Turing+, so Vulkan)
in [`dmkif/k3s-gitops`](https://github.com/dmkif/k3s-gitops), Feature 008.

- Tags: `v<lemonade-version>-dmkif.<n>` (git tag → image tag), plus `sha-<short>`.
- Base image pinned by digest in `Dockerfile`; Renovate proposes updates.
- Remove once upstream ships `libegl1` and `libxext6`.

## ace-server-lowvram.sh

Wrapper around Lemonade's `ace-server` that removes `--keep-loaded`, so ACE-Step vocals fit on
11 GB GPUs (LM and DiT no longer stay resident together). Use it via
`"acestep": {"vulkan_bin": "/opt/lemonade/ace-server-lowvram.sh"}` in `config.json`.
