module github.com/SynologyOpenSource/synology-csi

go 1.16

require (
	github.com/antonfisher/nested-logrus-formatter v1.3.1
	github.com/cenkalti/backoff/v4 v4.1.1
	github.com/container-storage-interface/spec v1.5.0
	github.com/kubernetes-csi/csi-lib-utils v0.9.1
	github.com/kubernetes-csi/csi-test/v4 v4.3.0
	github.com/sirupsen/logrus v1.7.0
	github.com/spf13/cobra v1.1.3
	google.golang.org/grpc v1.34.0
	google.golang.org/protobuf v1.25.0
	gopkg.in/yaml.v2 v2.4.0
	k8s.io/mount-utils v0.26.4
	k8s.io/utils v0.0.0-20221107191617-1a15be271d1d
)

exclude google.golang.org/grpc v1.37.0
