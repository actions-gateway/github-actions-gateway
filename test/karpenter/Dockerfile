# Image for the karpenter kwok provider binary that scripts/karpenter-cluster.sh
# builds from the pinned upstream source (Q479). Upstream publishes no image for
# this provider — its own workflow is `ko build` from a checkout — so the harness
# reproduces ko's output: a static binary in an empty base. The chart's pod spec
# supplies the runtime identity (runAsUser/runAsGroup 65536, read-only root), so
# the image needs no user database of its own.
FROM scratch
COPY karpenter-kwok /karpenter-kwok
ENTRYPOINT ["/karpenter-kwok"]
