#!/bin/bash
echo "Uninstalling synology-csi pods ..."

plugin_name="csi.san.synology.com"
min_support_minor=19
max_support_minor=20
deploy_k8s_version="v1".$min_support_minor

SCRIPT_PATH="$(realpath "$0")"
SOURCE_PATH="$(realpath "$(dirname "$SCRIPT_PATH")"/../)"

source "$SOURCE_PATH"/scripts/functions.sh

parse_version
echo "Uninstall Version: $deploy_k8s_version"
kubectl delete -f "$SOURCE_PATH"/deploy/kubernetes/$deploy_k8s_version/snapshotter --ignore-not-found
kubectl delete -f "$SOURCE_PATH"/deploy/kubernetes/$deploy_k8s_version --ignore-not-found
echo "End of synology-csi uninstallation."
