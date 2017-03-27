# Rancher Cloud Controller Manager

Thank you for visiting the `rancher-cloud-controller-manager` repository!

Rancher Cloud Controller Manager - An external cloud controller manager for running kubernetes in a Rancher cluster.

# Introduction

External cloud providers were introduced as an Alpha feature in Kubernetes release 1.6. This repository contains an implementation of external cloud provider for Rancher clusters. An external cloud provider is a kubernetes controller that runs cloud provider-specific loops required for the functioning of kubernetes. These loops were originally a part of the kube-controller-manager, but they were tightly coupling the kube-controller-manager to cloud-provider specific code. In order to free the kubernetes project of this dependency, the {provider}-cloud-controller-manager was introduced.  

`cloud-controller-manager` allows cloud vendors and kubernetes core to evolve independent of each other. In prior releases, the core Kubernetes code was dependent upon cloud provider-specific code for functionality. In future releases, code specific to cloud vendors should be maintained by the cloud vendor themselves, and linked to `cloud-controller-manager` while running Kubernetes.

As such, you must disable these controller loops in the `kube-controller-manager` if you are running the `rancher-cloud-controller-manager`. You can disable the controller loops by setting the `--cloud-provider` flag to `external` when starting the kube-controller-manager. 

The following controllers are implemented by the `rancher-cloud-controller-manager`:

* Node Controller: For checking the cloud provider to determine if a node has been deleted in the cloud after it stops responding
* Route Controller: For setting up routes in the underlying cloud infrastructure
* Service Controller: For creating, updating and deleting cloud provider load balancers
* Volume Controller: For creating, attaching, and mounting volumes, and interacting with the cloud provider
  to orchestrate volumes

# Developing

`make` will build, test, and package this project. This project uses trash Godeps for dependency management and uses Dapper for consistent build environments. 

# Developing a cloud controller manager for your cloud

In order to create an external cloud-controller-manager for your cloud, simply follow these two steps

1. Import your cloud provider code [here](https://github.com/rancher/rancher-cloud-controller-manager/blob/master/main.go#L18)
2. Set the first argument to [InitCloudProvider](https://github.com/rancher/rancher-cloud-controller-manager/blob/master/main.go#L38) as the output of your cloudprovider.ProviderName() method.

# License
Copyright (c) 2014-2015 [Rancher Labs, Inc.](http://rancher.com)

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

[http://www.apache.org/licenses/LICENSE-2.0](http://www.apache.org/licenses/LICENSE-2.0)

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
