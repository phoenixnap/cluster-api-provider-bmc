<h1 align="center">
  <br>
  <a href="https://phoenixnap.com/bare-metal-cloud"><img src="https://user-images.githubusercontent.com/78744488/109779287-16da8600-7c06-11eb-81a1-97bf44983d33.png" alt="phoenixnap Bare Metal Cloud" width="300"></a>
  <br>
  Bare Metal Cloud API Provider for Kubernetes
  <br>
</h1>

<p align="center">
The Bare Metal Cloud API Provider for Kubernetes allows developers to define, deploy, and manage Bare Metal Cloud servers to provision, upgrade and operate Kubernetes clusters. 
</p>

<p align="center">
  <a href="https://phoenixnap.com/bare-metal-cloud">Bare Metal Cloud</a> •
  <a href="https://developers.phoenixnap.com/">Developers Portal</a> •
  <a href="https://developers.phoenixnap.com/apis">API Documentation</a> •
  <a href="http://phoenixnap.com/kb">Knowledge Base</a> •
  <a href="https://developers.phoenixnap.com/support">Support</a>
</p>

## Creating a Bare Metal Cloud Account

You need to have a Bare Metal Cloud account to use this Kubernetes Cluster API.  

1. Go to the [Bare Metal Cloud signup page](https://support.phoenixnap.com/wap-jpost3/bmcSignup).
2. Follow the prompts to set up your account.
3. Use your credentials to [log in to the Bare Metal Cloud portal](https://bmc.phoenixnap.com).

:arrow_forward: **Video tutorial:** [How to Create a Bare Metal Cloud Account](https://www.youtube.com/watch?v=RLRQOisEB-k)
<br>
:arrow_forward: **Video tutorial:** [Introduction to Bare Metal Cloud](https://www.youtube.com/watch?v=8TLsqgLDMN4)

## Getting Started from Source

1. [Install and/or configure Kubernetes cluster (1.23+)](https://cluster-api.sigs.k8s.io/user/quick-start.html#installation). Cluster API requires an existing Kubernetes cluster accessible via kubectl.
1. [Install clusterctl](https://cluster-api.sigs.k8s.io/user/quick-start.html#install-clusterctl)
1. [Download clusterctl.yaml file](https://github.com/phoenixnap/cluster-api-provider-bmc/releases/latest/download/clusterctl.yaml)
1. [Initialize BMC clusterctl management cluster](https://cluster-api.sigs.k8s.io/user/quick-start.html#initialize-the-management-cluster). See [Avoiding GitHub rate limiting](https://cluster-api.sigs.k8s.io/clusterctl/overview.html?highlight=github%20token#avoiding-github-rate-limiting) if you're experiencing errors from GitHub.
    ```
    export BMC_CLIENT_ID=<bmc client id>
    export BMC_CLIENT_SECRET=<bmc client secret>

    # Initialize the management cluster
    clusterctl init --infrastructure bmc --config clusterctl.yaml
    ```
1. Generate the cluster configuration. This creates a files called `capi-quickstart.yaml` with predefined list of Cluster API objects.
    ```
    export BMC_CONTROL_PLANE_MACHINE_TYPE=s2.c1.medium  # server type of the control plane
    export BMC_LOCATION=PHX                             # location of cluster
    export BMC_NODE_TYPE=s2.c1.medium                   # server types of the node
    export VIP_MANAGER=KUBEVIP                          # use KUBEVIP value to enable HA of control plane nodes, or NONE otherwise

    # Optional variables
    export CNI_VERSION=1.1.1        # version number for Container Network Interface (https://github.com/containernetworking/cni)
    export CONTAINERD_VERSION=1.4.4 # version number for Containerd (https://github.com/containerd/containerd)
    export CRI_VERSION=1.25.0       # version number for Kubernetes Container Runtime Interface (https://github.com/kubernetes-sigs/cri-tools/)
    export RUNC_VERSION=1.1.4       # version number for runc (https://github.com/opencontainers/runc)
    export BGP_PEERPASS=            # In case KUBEVIP value set for VIP_MANAGER, BGP peer password from BMC portal should be set

    # Generate the cluster configuration
    clusterctl generate cluster capi-quickstart \
      --kubernetes-version 1.25.0 \
      --worker-machine-count=3 \
      > capi-quickstart.yaml
    ```
1. Apply the cluster configuration `kubectl apply -f capi-quickstart.yaml`
1. Access the workload cluster.
    ```
    # The cluster will now start provisioning. You can check status with:
    kubectl get cluster

    # You can also get an “at glance” view of the cluster and its resources by running:
    clusterctl describe cluster capi-quickstart

    # To verify the first control plane is up:
    kubectl get kubeadmcontrolplane
    ```
1. Once the control plane is initialized, you can retrieve the workload cluster Kubeconfig.
    ```
    # Control plane initialized
    kubectl get kubeadmcontrolplane

    NAME                    CLUSTER           INITIALIZED   API SERVER AVAILABLE   REPLICAS   READY   UPDATED   UNAVAILABLE   AGE    VERSION
    capi-quickstart-g2trk   capi-quickstart   true                                 1                  1         1             4m7s   v1.25.0
    ``` 

    ```
    # Workload cluster Kubeconfig
    clusterctl get kubeconfig capi-quickstart > capi-quickstart.kubeconfig
    ```
1. Deploy a CNI solution (control plane won't be `Ready` till a CNI is installed)
    ```
    # Ex. deploying calico
    kubectl --kubeconfig=./capi-quickstart.kubeconfig \
      apply -f https://raw.githubusercontent.com/projectcalico/calico/v3.24.1/manifests/calico.yaml

    ```
1. Verify nodes are in a `Ready` status.
    ```
    kubectl --kubeconfig=./capi-quickstart.kubeconfig get nodes

    NAME                                          STATUS   ROLES           AGE   VERSION
    capi-quickstart-g2trk-9xrjv                   Ready    control-plane   12m   v1.25.0
    capi-quickstart-md-0-55x6t-5649968bd7-8tq9v   Ready    <none>          12m   v1.25.0
    capi-quickstart-md-0-55x6t-5649968bd7-glnjd   Ready    <none>          12m   v1.25.0
    capi-quickstart-md-0-55x6t-5649968bd7-sfzp6   Ready    <none>          12m   v1.25.0
    ```
1. Clean up workload cluster
    ```
    kubectl delete cluster capi-quickstart
    ```

## Pulling the Image

The Bare Metal Cloud Cluster API is available as a Docker image [here](https://github.com/phoenixnap/cluster-api-provider-bmc/pkgs/container/cluster-api-provider-bmc).

## Testing and CI

1. Create a GitHub annotated tag
   - `export RELEASE_TAG=<the tag of the release to be cut>` (eg. `export RELEASE_TAG=v1.0.1`)
   - `git tag -a ${RELEASE_TAG} -m ${RELEASE_TAG}`
   - `git tag test/${RELEASE_TAG}` (:warning: MUST NOT be an annotated tag)
1. Push the tag to the GitHub repository. This will automatically trigger a [Github Action](https://github.com/phoenixnap/cluster-api-provider-bmc/actions) to create a draft release.
   > NOTE: `origin` should be the name of the remote pointing to `github.com/phoenixnap/cluster-api-provider-bmc`
   - `git push origin ${RELEASE_TAG}`
   - `git push origin test/${RELEASE_TAG}`
1. Images are built by a [Github Action](https://github.com/phoenixnap/cluster-api-provider-bmc/actions). This pushes the image to the [GitHub Packages](https://github.com/phoenixnap/cluster-api-provider-bmc/pkgs/container/cluster-api-provider-bmc)
1. Review the draft release on GitHub.
1. Publish the release


## Retrieving BMC Credentials

1. [Log in to the Bare Metal Cloud portal](https://bmc.phoenixnap.com). 
2. On the left side menu, click on API Credentials. 
3. Click the Create Credentials button. 
4. Fill in the Name and Description fields, select the permissions scope and click Create. 
5. In the table, click on Actions and select View Credentials from the dropdown.  

:bulb: **Tutorial:** [How to create and manage BMC credentials](https://developers.phoenixnap.com/resources)

## Note to Maintainers

Be careful moving this repository. This project is written in Go and as such uses Git repo URLs as package identifiers. If the code URL is changed the code will need to be changed appropriately.

This is a `kubebuilder` project. Only minimal changes have been made to this codebase from the generated scaffolding so that maintainers can leverage as much off-the-shelf tooling and documentation as possible from the `kubebuilder` project. The bulk of the application code lives in the machine and cluster controller files,`controllers/bmcmachine_controller.go` and `controllers/bmccluster_controller.go`. The API type definitions, defaulting and validating webhook logic live in the directory, `api/v1beta1`.

## Bare Metal Cloud Community
Become part of the Bare Metal Cloud community to get updates on new features, help us improve the platform, and engage with developers and other users. 

-   Follow [@phoenixNAP on Twitter](https://twitter.com/phoenixnap)
-   Join the [official Slack channel](https://phoenixnap.slack.com)
-   Sign up for our [Developers Monthly newsletter](https://phoenixnap.com/developers-monthly-newsletter)

### Bare Metal Cloud Resources
-	[Product page](https://phoenixnap.com/bare-metal-cloud)
-	[Instance pricing](https://phoenixnap.com/bare-metal-cloud/instances)
-	[YouTube tutorials](https://www.youtube.com/watch?v=8TLsqgLDMN4&list=PLWcrQnFWd54WwkHM0oPpR1BrAhxlsy1Rc&ab_channel=PhoenixNAPGlobalITServices)
-	[Developers Portal](https://developers.phoenixnap.com)
-	[Knowledge Base](https://phoenixnap.com/kb)
-	[Blog](https:/phoenixnap.com/blog)

### Documentation
-	[API documentation](https://developers.phoenixnap.com/apis)

### Contact phoenixNAP
Get in touch with us if you have questions or need help with Bare Metal Cloud. 

<p align="left">
  <a href="https://twitter.com/phoenixNAP">Twitter</a> •
  <a href="https://www.facebook.com/phoenixnap">Facebook</a> •
  <a href="https://www.linkedin.com/company/phoenix-nap">LinkedIn</a> •
  <a href="https://www.instagram.com/phoenixnap">Instagram</a> •
  <a href="https://www.youtube.com/user/PhoenixNAPdatacenter">YouTube</a> •
  <a href="https://developers.phoenixnap.com/support">Email</a> 
</p>

<p align="center">
  <br>
  <a href="https://phoenixnap.com/bare-metal-cloud"><img src="https://user-images.githubusercontent.com/81640346/115243282-0c773b80-a123-11eb-9de7-59e3934a5712.jpg" alt="phoenixnap Bare Metal Cloud"></a>
</p>
