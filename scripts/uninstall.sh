#!/bin/bash
echo "Uninstalling synology-csi pods ..."

plugin_name="csi.san.synology.com"
deploy_k8s_version="v1.19"

SCRIPT_PATH="$(realpath "$0")"
SOURCE_PATH="$(realpath "$(dirname "$SCRIPT_PATH")"/../)"

kubectl delete -f "$SOURCE_PATH"/deploy/kubernetes/$deploy_k8s_version/snapshotter --ignore-not-found
kubectl delete -f "$SOURCE_PATH"/deploy/kubernetes/$deploy_k8s_version --ignore-not-found
echo "End of synology-csi uninstallation."