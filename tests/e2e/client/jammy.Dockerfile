# e2e client for the jammy job: Ubuntu 22.04 with Posit's R builds
# under /opt/R/<version>/ — the layout Workbench images use.
FROM ubuntu:22.04

ARG TARGETARCH
ARG R_VERSIONS="4.4.3 4.5.3"
ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update -qq \
 && apt-get install -qq -y --no-install-recommends ca-certificates curl jq \
 && for v in $R_VERSIONS; do \
      curl -fsSLo /tmp/r.deb "https://cdn.posit.co/r/ubuntu-2204/pkgs/r-${v}_1_${TARGETARCH}.deb" \
      && apt-get install -qq -y --no-install-recommends /tmp/r.deb \
      && rm /tmp/r.deb; \
    done \
 && rm -rf /var/lib/apt/lists/*

COPY install-tools.sh /tmp/install-tools.sh
RUN bash /tmp/install-tools.sh
