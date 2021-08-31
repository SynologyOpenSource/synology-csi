#!/usr/bin/env bash
# This script is only used in the container, see Dockerfile.

DIR="/host" # csi-node mount / of the node to /host in the container
BIN="$(basename "$0")"

if [ -d "$DIR" ]; then
    exec chroot $DIR /usr/bin/env -i PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" "$BIN" "$@"
fi

echo -n "Couldn't find hostPath: $DIR in the CSI container"
exit 1