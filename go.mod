module github.com/SynologyOpenSource/synology-csi

go 1.16

require (
	github.com/antonfisher/nested-logrus-formatter v1.3.1
	github.com/cenkalti/backoff/v4 v4.1.1
	github.com/container-storage-interface/spec v1.3.0
	github.com/golang/protobuf v1.4.3
	github.com/kubernetes-csi/csi-lib-utils v0.9.1
	github.com/kubernetes-csi/csi-test/v4 v4.2.0
	github.com/sirupsen/logrus v1.7.0
	github.com/spf13/cobra v1.1.3
	github.com/spf13/pflag v1.0.5
	google.golang.org/grpc v1.34.0
	gopkg.in/yaml.v2 v2.4.0
	k8s.io/mount-utils v0.21.2
	k8s.io/utils v0.0.0-20210527160623-6fdb442a123b
)

exclude google.golang.org/grpc v1.37.0
