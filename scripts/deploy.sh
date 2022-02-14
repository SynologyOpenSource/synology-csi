#!/bin/bash
plugin_name="csi.san.synology.com"
min_support_minor=19
max_support_minor=20
deploy_k8s_version="v1".$min_support_minor

SCRIPT_PATH="$(realpath "$0")"
SOURCE_PATH="$(realpath "$(dirname "${SCRIPT_PATH}")"/../)"
config_file="${SOURCE_PATH}/config/client-info.yml"
plugin_dir="/var/lib/kubelet/plugins/$plugin_name"

parse_version(){
    ver=$(kubectl version --short | grep Server | awk '{print $3}')
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
    echo "Deploy Version: $deploy_k8s_version"
}

# 1. Build
csi_build(){
    echo "==== Build synology-csi .... ===="
    source "$SOURCE_PATH"/build.sh
}

# 2. Install
csi_install(){
    echo "==== Creates namespace and secrets, then installs synology-csi ===="
    parse_version

    kubectl create ns synology-csi
    kubectl create secret -n synology-csi generic client-info-secret --from-file="$config_file"

    if [ ! -d "$plugin_dir" ]; then
        mkdir -p $plugin_dir
    fi

    kubectl apply -f "$SOURCE_PATH"/deploy/kubernetes/$deploy_k8s_version

    if [ "$basic_mode" == false ]; then
        kubectl apply -f "$SOURCE_PATH"/deploy/kubernetes/$deploy_k8s_version/snapshotter
    fi
}

print_usage(){
    echo "Usage:"
    echo "    deploy.sh run          build and install"
    echo "    deploy.sh [command]    specify an action to be performed"
    echo "Available Commands:"
    echo "    build                  build docker image only"
    echo "    install [flag]         install csi plugin with the specified flag"
    echo "    help                   show help"
    echo "Available Flags:"
    echo "    -a, --all              deploy csi plugin and snapshotter"
    echo "    -b, --basic            deploy basic csi plugin only"
    echo "Examples:"
    echo "    deploy.sh run"
    echo "    deploy.sh install --basic"
}

basic_mode=false
parse_flags(){
    case "$1" in
        -a|--all)
            ;;
        -b|--basic)
            basic_mode=true
            ;;
        *)
            print_usage
            exit 1
            ;;
    esac
}

case "$1" in
    build)
        csi_build
        ;;
    install)
        parse_flags "$2"
        csi_install
        ;;
    run)
        csi_build
        csi_install
        ;;
    *)
        print_usage
        exit 1
        ;;
esac