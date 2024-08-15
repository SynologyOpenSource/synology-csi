#!/bin/bash
echo "Uninstalling synology-csi pods ..."

plugin_name="csi.san.synology.com"
min_support_minor=19
max_support_minor=20
deploy_k8s_version="v1".$min_support_minor

SCRIPT_PATH="$(realpath "$0")"
SOURCE_PATH="$(realpath "$(dirname "$SCRIPT_PATH")"/../)"

parse_version(){
    ver=$(kubectl version | grep Server | awk '{print $3}')
    major=$(echo "${ver##*v}" | cut -d'.' -f1)
    minor=$(echo "${ver##*v}" | cut -d'.' -f2)

    if [[ "$major" != 1 ]]; then
        echo "Version not supported: $ver"
        exit 1
    fi

    case "$minor" in
        19|20)
            deploy_k8s_version="v1".$minor
            ;;
        *)
            if [[ $minor -lt $min_support_minor ]]; then
                deploy_k8s_version="v1".$min_support_minor
            else
                deploy_k8s_version="v1".$max_support_minor
            fi
            ;;
    esac
    echo "Uninstall Version: $deploy_k8s_version"
}


parse_version
kubectl delete -f "$SOURCE_PATH"/deploy/kubernetes/$deploy_k8s_version/snapshotter --ignore-not-found
kubectl delete -f "$SOURCE_PATH"/deploy/kubernetes/$deploy_k8s_version --ignore-not-found
echo "End of synology-csi uninstallation."
