# The agentiscaled container image (ghcr.io/whiteredhouse/agentiscaled).
# GoReleaser builds it from the release binaries: it puts each platform's
# binary at $TARGETPLATFORM/agentiscaled in the build context. The final
# stage only copies files, so a multi-platform build needs no emulation.
ARG DEBIAN=debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2

# CA certificates and the node's user, prepared on the build machine.
FROM --platform=$BUILDPLATFORM ${DEBIAN} AS base
RUN apt-get update \
	&& apt-get install -y --no-install-recommends ca-certificates \
	&& groupadd --system --gid 65532 agentiscaled \
	&& useradd --system --uid 65532 --gid 65532 --home-dir /home/agentiscaled --shell /bin/bash agentiscaled \
	&& mkdir -p /out/home/agentiscaled/.config/agentiscale \
	&& chown -R 65532:65532 /out/home/agentiscaled \
	&& chmod -R 700 /out/home/agentiscaled

FROM ${DEBIAN}
ARG TARGETPLATFORM
COPY --from=base /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=base /etc/passwd /etc/group /etc/
COPY --from=base /out/home/ /home/
COPY LICENSE NOTICE /usr/share/doc/agentiscaled/
COPY $TARGETPLATFORM/agentiscaled /usr/bin/agentiscaled
USER agentiscaled
ENV HOME=/home/agentiscaled
WORKDIR /home/agentiscaled
# The node keeps its identity and settings in its data directory,
# $HOME/.config/agentiscale by default: mount a volume there. A new named
# volume takes the directory's owner from the image.
ENTRYPOINT ["/usr/bin/agentiscaled"]
