# Synology CSI Chart for Kubernetes

This is a
[Helm](https://helm.sh) chart for installing the
[Synology CSI Driver](https://github.com/SynologyOpenSource/synology-csi) in a
[Kubernetes](https://kubernetes.io) cluster.
It has been forked from the original resource manifests bundled with the Synology CSI Driver in order to resolve some
issues and to benefit from the advanced resource management features of Helm.

## Features

+ Customizable images, image pull policies and image tags for all CSI containers.
+ Customizable parameters for `StorageClass` and `VolumeSnapshotClass` resources.
+ Customizable `affinity`, `nodeSelector` and `tolerations` properties for the `StatefulSet` and `DaemonSet` workloads.
+ Automatic installation of two storage classes with `reclaimPolicy=Delete` and `reclaimPolicy=Retain`.
+ Automatic installation of a CSI Snapshotter if the `VolumeSnapshotClass` CRD is installed in your cluster.

## Prerequisites

+ [Helm](https://helm.sh) must be installed to use this chart.
  Please refer to Helm's [documentation](https://helm.sh/docs) to get started.
+ A Synology Diskstation with the SAN Manager package installed (the package name was changed in DSM 7).
+ A cluster with [Kubernetes](https://kubernetes.io) version 1.20 or later.
+ `iscsiadm` installed on every cluster node.
+ `kubectl` installed on your localhost and configured to connect to the cluster.
+ Optional: [CSI Snapshotter](https://github.com/kubernetes-csi/external-snapshotter) installed in the cluster.

## Usage

Once Helm has been set up correctly, add the repo as follows:

    helm repo add synology-csi-chart https://christian-schlichtherle.github.io/synology-csi-chart

If you had already added this repo earlier, run `helm repo update` to retrieve the latest versions of the packages.
You can then run `helm search repo synology-csi-chart` to see the charts.

To install the chart:

    helm install <release-name> synology-csi-chart/synology-csi

To uninstall the chart:

    helm delete <release-name>

### Customizing the Configuration

As usual with Helm, you can override the values in the file [`values.yaml`](values.yaml) to suit your requirements - see
[Customizing the Chart Before Installing](https://helm.sh/docs/intro/using_helm/#customizing-the-chart-before-installing).
In particular, override the section `clientInfoSecret.clients` to match the connection parameters and credentials for
accessing your Synology Diskstation.

You can also use an existing secret (specify using the key `clientInfoSecret.name`) that contains a `clients`
dictionary under `client-info.yaml` key:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: <release-name>-client-info
data:
  client-info.yaml: |-
    Y2xpZW50czoKLSBob3N0OiAxOTIuMTY4LjEuMQogIGh0dHBzOiBmYWxzZQogIHBhc3N3b3JkOiBwYXNzd29yZAogIHBvcnQ6IDUwMDAKICB1c2VybmFtZTogdXNlcm5hbWUKLSBob3N0OiAxOTIuMTY4LjEuMQogIGh0dHBzOiBmYWxzZQogIHBhc3N3b3JkOiBwYXNzd29yZAogIHBvcnQ6IDUwMDEKICB1c2VybmFtZTogdXNlcm5hbWU=
```

It's a good practice to create a dedicated user for accessing the Diskstation Manager (DSM) application.
Your user needs to be a member of the `administrators` group and have permission to access the `DSM` application.
You can reject any other permissions.

## Local Development

### Installing the Chart from Source

Clone this repository and change into its directory.
You can run `helm install ...` as usual.
However, for convenience it's recommended to use the provided `Makefile` and run:

    $ make up

... or just:

    $ make

### Testing the Chart

The Synology CSI Driver is now ready to use.
You can monitor the SAN Manager application on your Synology Diskstation while running the following tests.

#### Automated Testing

    $ make test

This will create a `PersistentVolumeClaim` (PVC) and a `Job` to mount the associated `PersistentVolume` (PV) and write a
file to its filesystem.
If this succeeds, all resources get automatically removed.

### Troubleshooting

First, make sure to run the automated test.
This may hang if the PVC cannot be bound for some reason, for example if you forgot to edit the credentials for
accessing the Diskstation Manager application.
In other cases it may fail with an error message but then the resources do not get deleted so that you can troubleshoot
the issue.

For example, let's assume this chart has been installed using `make` and thus its namespace is `synology-csi`:

```
$ NAMESPACE=synology-csi
```

Now let's look at the pod for the test job:

```
$ kubectl describe pods -n $NAMESPACE -l helm.sh/template=test.yaml
[...]
Volumes:
  data:
    Type:       PersistentVolumeClaim (a reference to a PersistentVolumeClaim in the same namespace)
    ClaimName:  synology-csi-test
    ReadOnly:   false
[...]
Events:
  Type     Reason            Age                 From               Message
  ----     ------            ----                ----               -------
  Warning  FailedScheduling  3m6s                default-scheduler  0/4 nodes are available: 4 pod has unbound immediate PersistentVolumeClaims.
  Warning  FailedScheduling  61s (x1 over 2m1s)  default-scheduler  0/4 nodes are available: 4 pod has unbound immediate PersistentVolumeClaims.
```

The test PVC isn't bound.
Let's examine why:

```
$ kubectl describe pvc -n $NAMESPACE -l helm.sh/template=test.yaml
[...]
Events:
  Type     Reason                Age                     From                                                                  Message
  ----     ------                ----                    ----                                                                  -------
  Normal   Provisioning          4m2s (x9 over 8m17s)    csi.san.synology.com_kolossus-3_c3a0b188-fa91-409b-a4d1-0316d5ffbbd6  External provisioner is provisioning volume for claim "synology-csi/synology-csi-test"
  Warning  ProvisioningFailed    4m2s (x9 over 8m17s)    csi.san.synology.com_kolossus-3_c3a0b188-fa91-409b-a4d1-0316d5ffbbd6  failed to provision volume with StorageClass "synology-csi-delete": rpc error: code = Internal desc = Couldn't find any host available to create Volume
  Normal   ExternalProvisioning  2m25s (x26 over 8m17s)  persistentvolume-controller                                           waiting for a volume to be created, either by external provisioner "csi.san.synology.com" or manually created by system administrator
```

The events mention a provisioner which is running in a container named `plugin` in a controller pod which has been
deployed as part of this chart.
Let's examine the logs of this plugin container:

```
$ kubectl logs -n $NAMESPACE -l helm.sh/template=controller.yaml -c plugin
[...]
2022-01-25T08:19:57Z [INFO] [driver/utils.go:104] GRPC call: /csi.v1.Controller/CreateVolume
2022-01-25T08:19:57Z [INFO] [driver/utils.go:105] GRPC request: {"capacity_range":{"required_bytes":1073741824},"name":"pvc-a3d7962b-0ab5-4184-b545-a44cc424aaf1","parameters":{"fsType":"ext4"},"volume_capabilities":[{"AccessType":{"Mount":{"fs_type":"ext4"}},"access_mode":{"mode":1}}]}
2022-01-25T08:19:57Z [ERROR] [driver/utils.go:108] GRPC error: rpc error: code = Internal desc = Couldn't find any host available to create Volume
```

After all, we found out that the plugin isn't able to connect to the Diskstation.
This is because I didn't edit the `clientInfoSecret.clients` dictionary in the `values.yaml` file.
After fixing that the test passes as expected.

### Uninstalling the Chart

    $ make down

---

**SUCCESS - enjoy!**
