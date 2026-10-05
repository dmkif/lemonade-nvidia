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

## lemonade-guard (guard/)

Go reverse proxy (standard library only) that runs as a sidecar from the same image
(`/opt/lemonade/guard`, config JSON via `-config`). Allowlist for the gateway, VRAM-budget admission
(batch jobs such as music own the GPU; idle models are unloaded LRU-first), idle unload, one self-healing
retry after a 5xx, asynchronous music jobs (`/guard/jobs/audio`), start-up sync of backends and
models, `/guard/status`, `/metrics`, `/guard/drain`. Contract and design:
`specs/008-apps-gpu-worker-lemonade/contracts/guard-api.md` in dmkif/k3s-gitops.
The build runs `go vet` and `go test`.
