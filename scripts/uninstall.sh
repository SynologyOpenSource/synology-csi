#!/usr/bin/env bash
echo "Uninstalling synology-csi pods ..."

plugin_name="csi.san.synology.com"
deploy_k8s_version="v1.19"

SOURCE_PATH="$(cd "$(dirname "$0")/.." && pwd -P)"

kubectl delete -f "$SOURCE_PATH"/deploy/kubernetes/$deploy_k8s_version/snapshotter --ignore-not-found
kubectl delete -f "$SOURCE_PATH"/deploy/kubernetes/$deploy_k8s_version --ignore-not-found
echo "End of synology-csi uninstallation."