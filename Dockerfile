FROM gcr.io/distroless/base-debian13:nonroot

ARG TARGETPLATFORM

COPY --chown=nonroot:nonroot deploy/docker/data/ /data/
COPY --chown=nonroot:nonroot ${TARGETPLATFORM}/aethos /usr/local/bin/aethos

ENV AETHOS_DATA_DIR=/data/aethos
ENV HOME=/data/home

WORKDIR /data/workspace
USER nonroot:nonroot
VOLUME ["/data"]

ENTRYPOINT ["/usr/local/bin/aethos"]
