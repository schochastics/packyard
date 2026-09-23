# e2e client for the rhel9 job: AlmaLinux 9 with Posit's R builds
# under /opt/R/<version>/. R's RPM dependencies live in EPEL and CRB.
FROM almalinux:9

ARG R_VERSIONS="4.4.3 4.5.3"

RUN dnf install -q -y dnf-plugins-core epel-release \
 && dnf config-manager --set-enabled crb \
 && arch=$(uname -m) \
 && for v in $R_VERSIONS; do \
      dnf install -q -y "https://cdn.posit.co/r/rhel-9/pkgs/R-${v}-1-1.${arch}.rpm"; \
    done \
 && dnf install -q -y jq \
 && dnf clean all

COPY install-tools.sh /tmp/install-tools.sh
RUN bash /tmp/install-tools.sh
