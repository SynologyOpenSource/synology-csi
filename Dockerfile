# Copyright 2021 Synology Inc.

############## Build stage ##############
FROM golang:1.27.1-alpine AS builder
LABEL stage=synobuilder

RUN apk add --no-cache alpine-sdk
WORKDIR /go/src/synok8scsiplugin
COPY go.mod go.sum ./
RUN go mod download

COPY Makefile .

ARG TARGETPLATFORM

COPY main.go .
COPY pkg ./pkg
COPY synocli ./synocli
RUN env GOARCH=$(echo "$TARGETPLATFORM" | cut -f2 -d/) \
        GOARM=$(echo "$TARGETPLATFORM" | cut -f3 -d/ | cut -c2-) \
        make

############## RHEL package stage ##############
# e2fsprogs, xfsprogs, nfs-utils and cifs-utils are NOT in the free UBI
# repos; they live in the full RHEL 9 BaseOS/AppStream channels, which
# require RHEL entitlement (a free Red Hat Developer Subscription works -
# Simple Content Access, no --auto-attach/pool needed). This stage
# registers with subscription-manager using BuildKit secrets
# (rh_username/rh_password), then only DOWNLOADS the needed packages as
# .rpm files with `dnf download` - it never installs anything here.
#
# --resolve --alldeps (not --resolve alone) is required: this stage runs
# on the full ubi9/ubi image, and plain --resolve only downloads what's
# NOT already satisfied on *that* image - but ubi-minimal's installed set
# is a smaller subset (missing e.g. pam, systemd, python3, keyutils,
# util-linux-core, libfdisk), so packages resolve-only treats as "already
# there" would be silently missing in the final stage, breaking rpm's
# dependency check (verified: --resolve alone left the final `rpm -Uvh`
# unable to resolve nfs-utils/util-linux's real deps). --alldeps instead
# downloads the full closure, including packages ubi-minimal already has
# at the identical NEVRA - harmless, see --replacepkgs below.
#
# --exclude=coreutils,curl,libcurl: ubi-minimal deliberately ships the
# slimmer coreutils-single/curl-minimal/libcurl-minimal instead, which
# RPM-conflict with the full packages (verified: rpm -Uvh failed with
# "coreutils-single conflicts with coreutils" etc. without this
# exclusion). Excluding them lets the resolver fall back to depending on
# whichever variant is already installed.
#
# The final stage installs these pre-fetched RPMs with plain `rpm -Uvh`
# (verified: `microdnf install <local-rpm-path>` does NOT work - it only
# resolves against configured repos, not local files) and needs no
# entitlement itself. So subscription-manager, registration state, and
# entitlement certs never reach the shipped image, and there's no
# register/install/unregister/cleanup sequence to get right inside a
# single layer of the final image.
FROM registry.access.redhat.com/ubi9/ubi:latest AS rpm-fetch
RUN --mount=type=secret,id=rh_username,required=true \
    --mount=type=secret,id=rh_password,required=true \
    subscription-manager register \
        --username="$(cat /run/secrets/rh_username)" \
        --password="$(cat /run/secrets/rh_password)" \
    && subscription-manager repos \
        --enable=rhel-9-for-$(uname -m)-baseos-rpms \
        --enable=rhel-9-for-$(uname -m)-appstream-rpms \
    && mkdir -p /rpms \
    && dnf download -y --resolve --alldeps --destdir=/rpms \
        --exclude=coreutils --exclude=curl --exclude=libcurl \
        e2fsprogs xfsprogs util-linux iproute bash \
        ca-certificates cifs-utils nfs-utils nvme-cli \
    && subscription-manager unregister

############## Final stage ##############
FROM registry.access.redhat.com/ubi9/ubi-minimal:latest

ARG IMAGE_VERSION=dev
ARG IMAGE_RELEASE=1
LABEL name="synology-csi" \
      maintainer="Synology" \
      vendor="Synology Inc." \
      version="${IMAGE_VERSION}" \
      release="${IMAGE_RELEASE}" \
      summary="Synology CSI driver for Kubernetes" \
      description="A Container Storage Interface (CSI) driver for Synology NAS."

# Runtime tools, fetched by the rpm-fetch stage above. blkid is provided
# by util-linux.
#
# btrfs-progs (present in the old Alpine image) is intentionally omitted, which
# drops support for StorageClass `fsType: btrfs` (mkfs runs inside the container,
# see pkg/driver/nodeserver.go NodeStageVolume). Rationale: RHEL 9 removed btrfs
# entirely — no kernel module and no btrfs-progs package — so on RHCOS/OpenShift
# the filesystem could not be mounted even if we shipped the tools, and the UBI/
# RHEL repos have no package to install. Note this IS a behavior change for
# non-RHEL nodes whose kernels support btrfs; it must be called out in the docs/
# release notes. DSM-side btrfs volumes (BLUN location) are unaffected — that is
# an API-level attribute and needs no local tools.
# --replacepkgs: the --alldeps download above includes packages already
# present in this image at the identical NEVRA (same RHEL 9.8 point
# release). Plain `rpm -U` treats an exact-NEVRA duplicate as an error,
# and since all RPMs install as one transaction, that error aborted the
# whole install before any new package landed (verified: RPM_EXIT=245,
# nothing installed). --replacepkgs makes those a no-op reinstall instead
# so the genuinely new packages proceed.
COPY --from=rpm-fetch /rpms /tmp/rpms
RUN rpm -Uvh --replacepkgs /tmp/rpms/*.rpm \
    && rm -rf /tmp/rpms

# Red Hat certification requires a /licenses directory in the image.
COPY LICENSE /licenses/LICENSE

WORKDIR /

# Copy and run CSI driver
COPY --from=builder /go/src/synok8scsiplugin/bin/synology-csi-driver synology-csi-driver

# Declare a non-root user to satisfy the RunAsNonRoot certification check.
# The node DaemonSet still runs privileged with runAsUser: 0 (mount / iscsiadm
# need root); the controller Deployment can run as this user.
USER 1000

ENTRYPOINT ["/synology-csi-driver"]
