FROM redhat/ubi8:latest

# Note: See "linux_host" note the IBox Ansible vars yaml
# file in git.infinidat.com:PSUS/webinar-automate-sla.git.
# This is affected by the base image choice.
# Base image is specified in Makefile and must match.

MAINTAINER partners.infi@infinidat.com

ARG   BLAME_MACHINE
ARG   BLAME_USER
ARG   DOCKER_IMAGE_TAG
ARG   VCS_REF
ENV   BLAME_MACHINE=$BLAME_MACHINE
ENV   BLAME_USER=$BLAME_USER
ENV   DOCKER_IMAGE_TAG=$DOCKER_IMAGE_TAG
ENV   VCS_REF=$VCS_REF
LABEL BLAME_MACHINE=$BLAME_MACHINE
LABEL BLAME_USER=$BLAME_USER
LABEL DOCKER_IMAGE_TAG=$DOCKER_IMAGE_TAG
LABEL VCS_REF=$VCS_REF
LABEL description="A CSI Driver image for InfiniBox"
LABEL name="infinibox-csi-driver"
LABEL org.opencontainers.image.authors="partners.infi@infinidat.com"
LABEL summary="Infinidat CSI-Plugin"
LABEL vendor="Infinidat"

COPY licenses /licenses
COPY setenv.sh /setenv.sh
RUN chmod +x /setenv.sh
COPY infinibox-csi-driver /infinibox-csi-driver
RUN chmod +x /infinibox-csi-driver

RUN yum -y install file lsof hostname && \
	yum -y update && \
    yum -y clean all && rm -rf /var/cache

RUN mkdir /ibox
ADD host-chroot.sh /ibox
RUN chmod 777 /ibox/host-chroot.sh
RUN \
       ln -s /ibox/host-chroot.sh /ibox/blkid \
    && ln -s /ibox/host-chroot.sh /ibox/blockdev \
    && ln -s /ibox/host-chroot.sh /ibox/cat \
    && ln -s /ibox/host-chroot.sh /ibox/chown \
    && ln -s /ibox/host-chroot.sh /ibox/chmod \
    && ln -s /ibox/host-chroot.sh /ibox/dmsetup \
    && ln -s /ibox/host-chroot.sh /ibox/file \
    && ln -s /ibox/host-chroot.sh /ibox/find \
    && ln -s /ibox/host-chroot.sh /ibox/fsck \
    && ln -s /ibox/host-chroot.sh /ibox/hostnamectl \
    && ln -s /ibox/host-chroot.sh /ibox/iscsiadm \
    && ln -s /ibox/host-chroot.sh /ibox/lsblk \
    && ln -s /ibox/host-chroot.sh /ibox/lsof \
    && ln -s /ibox/host-chroot.sh /ibox/lsscsi \
    && ln -s /ibox/host-chroot.sh /ibox/mkdir \
    && ln -s /ibox/host-chroot.sh /ibox/mkfs.ext3 \
    && ln -s /ibox/host-chroot.sh /ibox/mkfs.ext4 \
    && ln -s /ibox/host-chroot.sh /ibox/mkfs.xfs \
    && ln -s /ibox/host-chroot.sh /ibox/mount \
    && ln -s /ibox/host-chroot.sh /ibox/multipath \
    && ln -s /ibox/host-chroot.sh /ibox/multipathd \
    && ln -s /ibox/host-chroot.sh /ibox/rescan-scsi-bus.sh \
    && ln -s /ibox/host-chroot.sh /ibox/rmdir \
    && ln -s /ibox/host-chroot.sh /ibox/rpcbind \
    && ln -s /ibox/host-chroot.sh /ibox/umount \
    && ln -s /ibox/host-chroot.sh /ibox/vi \
    && ln -s /ibox/host-chroot.sh /ibox/vim \
    && ln -s /ibox/host-chroot.sh /ibox/whoami \
    && ln -s /ibox/host-chroot.sh /ibox/xfs_admin \
    && ln -s /ibox/host-chroot.sh /ibox/xfs_db

ENV PATH="/ibox:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"


ENTRYPOINT ["/setenv.sh"]
