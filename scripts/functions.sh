#!/bin/bash
min_support_minor=19
max_support_minor=20
deploy_k8s_version="v1".$min_support_minor

parse_version(){
    ver=$(kubectl version --output=json | awk -F'"' '/"serverVersion":/ {flag=1} flag && /"gitVersion":/ {print $(NF-1); flag=0}')
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
    echo "Current Server Version: $ver"
}