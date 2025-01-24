# check=skip=RedundantTargetPlatform
# setup build image
FROM --platform=$TARGETPLATFORM mcr.microsoft.com/oss/go/microsoft/golang:1.23-fips-bullseye AS operator-build

ENV GOEXPERIMENT=systemcrypto

WORKDIR /app

ARG DEBUG_TOOLS
RUN if [ "$DEBUG_TOOLS" = "true" ]; then \
      GOBIN=/app/build/_output/bin go install github.com/go-delve/delve/cmd/dlv@latest; \
    fi

COPY go.mod go.sum ./
RUN go mod download -x

COPY pkg ./pkg
COPY cmd ./cmd

ARG GO_LINKER_ARGS
ARG GO_BUILD_TAGS
ARG TARGETARCH
ARG TARGETOS

RUN --mount=type=cache,target="/root/.cache/go-build" \
    --mount=type=cache,target="/go/pkg" \
    CGO_ENABLED=1 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -tags "${GO_BUILD_TAGS}" -trimpath -ldflags="${GO_LINKER_ARGS}" \
    -o ./build/_output/bin/dynatrace-operator ./cmd/

# ---------------- Install Packages in Final Image -----------------------
# platform is required, otherwise the copy command will copy the wrong architecture files, don't trust GitHub Actions linting warnings
FROM --platform=$TARGETPLATFORM registry.access.redhat.com/ubi9-micro:9.5-1736426761@sha256:f6e0a71b7e0875b54ea559c2e0a6478703268a8d4b8bdcf5d911d0dae76aef51 AS base
FROM --platform=$TARGETPLATFORM registry.access.redhat.com/ubi9:9.5-1736404036@sha256:53d6c19d664f4f418ce5c823d3a33dbb562a2550ea249cf07ef10aa063ace38f AS dependency
RUN mkdir -p /tmp/rootfs-dependency
COPY --from=base / /tmp/rootfs-dependency
RUN dnf install --installroot /tmp/rootfs-dependency \
      util-linux-core tar \
      --releasever 9 \
      --setopt install_weak_deps=false \
      --nodocs -y \
 && dnf --installroot /tmp/rootfs-dependency clean all \
 && rm -rf \
      /tmp/rootfs-dependency/var/cache/* \
      /tmp/rootfs-dependency/var/log/dnf* \
      /tmp/rootfs-dependency/var/log/yum.*

# Build and Install OpenSSL
# Version must be FIPS certified
ENV OPENSSL_BUILD_VERSION="3.0.9"
# Get this from trusted source (e.g. Github release)
ENV OPENSSL_BUILD_TARBALL_SHA256="eb1ab04781474360f77c318ab89d8c5a03abc38e63d65a603cabbf1b00a1dc90"
ENV OPENSSL_BUILD_CONFIGURE_ARGS="enable-fips"
# Dependencies
RUN dnf install --setopt install_weak_deps=false --nodocs -y make gcc perl

WORKDIR /openssl_build

RUN curl -L -o src.tgz https://github.com/openssl/openssl/releases/download/openssl-${OPENSSL_BUILD_VERSION}/openssl-${OPENSSL_BUILD_VERSION}.tar.gz && \
    sha256sum --quiet -c - <<< "${OPENSSL_BUILD_TARBALL_SHA256}  src.tgz" && \
    tar --strip-components=1 -xzf src.tgz

# Disable the aflag test because it doesn't work on qemu (aka arm build), maybe do it conditional (see https://github.com/openssl/openssl/pull/17945)
RUN ./Configure ${OPENSSL_BUILD_CONFIGURE_ARGS} && make && make test TESTS="-test_afalg"
# Do not install man pages
RUN make DESTDIR=/tmp/rootfs-dependency install_sw install_ssldirs install_fips

# ---------------- Assemble Final Image -----------------------
# platform is required, otherwise the copy command will copy the wrong architecture files, don't trust GitHub Actions linting warnings
FROM --platform=$TARGETPLATFORM base

ARG TARGETPLATFORM

COPY --from=dependency /tmp/rootfs-dependency /

# operator binary
COPY --from=operator-build /app/build/_output/bin /usr/local/bin

# csi binaries
COPY --from=registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.13.0@sha256:d7138bcc3aa5f267403d45ad4292c95397e421ea17a0035888850f424c7de25d /csi-node-driver-registrar /usr/local/bin
COPY --from=registry.k8s.io/sig-storage/livenessprobe:v2.15.0@sha256:2c5f9dc4ea5ac5509d93c664ae7982d4ecdec40ca7b0638c24e5b16243b8360f /livenessprobe /usr/local/bin

COPY ./third_party_licenses /usr/share/dynatrace-operator/third_party_licenses
COPY LICENSE /licenses/

# custom scripts
COPY hack/build/bin /usr/local/bin

LABEL name="Dynatrace Operator" \
      vendor="Dynatrace LLC" \
      maintainer="Dynatrace LLC" \
      version="1.x" \
      release="1" \
      url="https://www.dynatrace.com" \
      summary="The Dynatrace Operator is an open source Kubernetes Operator for easily deploying and managing Dynatrace components for Kubernetes / OpenShift observability. By leveraging the Dynatrace Operator you can innovate faster with the full potential of Kubernetes / OpenShift and Dynatrace’s best-in-class observability and intelligent automation." \
      description="Automate Kubernetes observability with Dynatrace" \
      io.k8s.description="Automate Kubernetes observability with Dynatrace" \
      io.k8s.display-name="Dynatrace Operator" \
      io.openshift.tags="observability,monitoring,dynatrace,operator,logging,metrics,tracing,prometheus,alerts" \
      vcs-url="https://github.com/Dynatrace/dynatrace-operator.git" \
      vcs-type="git" \
      changelog-url="https://github.com/Dynatrace/dynatrace-operator/releases"

ENV OPERATOR=dynatrace-operator \
    USER_UID=1001 \
    USER_NAME=dynatrace-operator

RUN /usr/local/bin/user_setup

# FIPS 
ENV LIBGCRYPT_FORCE_FIPS_MODE=1
ENV GOFIPS=1

# Generate openssl FIPS config and run self-tests (these are required to be compliant)
RUN case "${TARGETPLATFORM}" in \
        *amd64) LIB_DIR=/usr/local/lib64 ;; \
        *arm64) LIB_DIR=/usr/local/lib ;; \
        *) echo $TARGETPLATFORM ; exit 2 ;; \
    esac; \
    # Otherwise openssl will still use system libs
    ldconfig "${LIB_DIR}" && \
    openssl fipsinstall -out /usr/local/ssl/fipsmodule.cnf -module "${LIB_DIR}/ossl-modules/fips.so"

# Always use FIPS (sets the default openssl config to use the FIPS provider), also the config dir for the self-built openssl is /usr/local/ssl and NOT /etc/ssl
RUN sed -i '/\.include fipsmodule\.cnf/s/^# //g' /usr/local/ssl/openssl.cnf && sed -i '/fips = fips_sect/s/^# //g' /usr/local/ssl/openssl.cnf && sed -i 's#\.include fipsmodule\.cnf#\.include /usr/local/ssl/fipsmodule\.cnf#g' /usr/local/ssl/openssl.cnf

ENTRYPOINT ["/usr/local/bin/entrypoint"]

USER ${USER_UID}:${USER_UID}
