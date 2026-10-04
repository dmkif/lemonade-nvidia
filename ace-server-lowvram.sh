#!/bin/sh
# ace-server ohne --keep-loaded: jede Stufe (LM, DiT, VAE) laedt und gibt ihren Speicher wieder frei,
# sonst passen LM + DiT + Encoder nicht in 11 GB (GTX 1080 Ti).
real=/opt/lemonade/.cache/lemonade/bin/acestep/vulkan/ace-server
for a in "$@"; do
  shift
  [ "$a" = "--keep-loaded" ] || set -- "$@" "$a"
done
exec "$real" "$@"
