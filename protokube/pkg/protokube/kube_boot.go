/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package protokube

import (
	"context"
	"net"
	"path/filepath"
	"time"

	"k8s.io/klog/v2"
)

var (
	// Containerized indicates the etcd is containerized
	Containerized = false
	// RootFS is the root fs path
	RootFS = "/"
)

// KubeBoot is the options for the protokube service
type KubeBoot struct {
	// Channels is a list of channel to apply
	Channels []string
	// InitializeRBAC should be set to true if we should create the core RBAC roles
	InitializeRBAC bool
	// InternalDNSSuffix is the dns zone we are living in
	InternalDNSSuffix string
	// InternalIP is the internal ip address of the node
	InternalIP net.IP
	// ApplyTaints controls whether we set taints based on the master label
	ApplyTaints bool
	// DNS is the dns provider
	DNS DNSProvider
	// ModelDir is the model directory
	ModelDir string
	// Kubernetes holds a kubernetes client
	Kubernetes *KubernetesContext
	// Master indicates we are a master node
	Master bool

	// ManageEtcd is true if we should manage etcd.
	// Deprecated in favor of etcd-manager.
	ManageEtcd bool
	// EtcdBackupImage is the image to use for backing up etcd
	EtcdBackupImage string
	// EtcdBackupStore is the VFS path to which we should backup etcd
	EtcdBackupStore string
	// Etcd container registry location.
	EtcdImageSource string
	// EtcdElectionTimeout is the leader election timeout
	EtcdElectionTimeout string
	// EtcdHeartbeatInterval is the heartbeat interval
	EtcdHeartbeatInterval string
	// TLSAuth indicates we should enforce peer and client verification
	TLSAuth bool
	// TLSCA is the path to a client ca for etcd
	TLSCA string
	// TLSCert is the path to a tls certificate for etcd
	TLSCert string
	// TLSKey is the path to a tls private key for etcd
	TLSKey string
	// PeerCA is the path to a peer ca for etcd
	PeerCA string
	// PeerCert is the path to a peer certificate for etcd
	PeerCert string
	// PeerKey is the path to a peer private key for etcd
	PeerKey string

	// BootstrapMasterNodeLabels controls the initial application of node labels to our node
	// The node is found by matching NodeName
	BootstrapMasterNodeLabels bool

	// NodeName is the name of our node as it will be registered in k8s.
	// Used by BootstrapMasterNodeLabels
	NodeName string

	volumeMounter   *VolumeMountController
	etcdControllers map[string]*EtcdController
}

// Init is responsible for initializing the controllers
func (k *KubeBoot) Init(volumesProvider Volumes) {
	k.volumeMounter = newVolumeMountController(volumesProvider)
	k.etcdControllers = make(map[string]*EtcdController)
}

// RunSyncLoop is responsible for provision the cluster
func (k *KubeBoot) RunSyncLoop() {
	for {
		ctx := context.Background()
		if err := k.syncOnce(ctx); err != nil {
			klog.Warningf("error during attempt to bootstrap (will sleep and retry): %v", err)
		}

		time.Sleep(1 * time.Minute)
	}
}

func (k *KubeBoot) syncOnce(ctx context.Context) error {
	if k.Master && k.ManageEtcd {
		// attempt to mount the volumes
		volumes, err := k.volumeMounter.mountMasterVolumes()
		if err != nil {
			return err
		}

		for _, v := range volumes {
			for _, etcdSpec := range v.Info.EtcdClusters {
				key := etcdSpec.ClusterKey + "::" + etcdSpec.NodeName
				etcdController := k.etcdControllers[key]
				if etcdController == nil {
					klog.Infof("Found etcd cluster spec on volume %q: %v", v.ID, etcdSpec)
					etcdController, err := newEtcdController(k, v, etcdSpec)
					if err != nil {
						klog.Warningf("error building etcd controller: %v", err)
					} else {
						k.etcdControllers[key] = etcdController
						go etcdController.RunSyncLoop()
					}
				}
			}
		}
	}

	if k.Master {
		if k.BootstrapMasterNodeLabels {
			if err := bootstrapMasterNodeLabels(ctx, k.Kubernetes, k.NodeName); err != nil {
				klog.Warningf("error bootstrapping master node labels: %v", err)
			}
		}
		if k.ApplyTaints {
			if err := applyMasterTaints(ctx, k.Kubernetes); err != nil {
				klog.Warningf("error updating master taints: %v", err)
			}
		}
		if k.InitializeRBAC {
			if err := applyRBAC(ctx, k.Kubernetes); err != nil {
				klog.Warningf("error initializing rbac: %v", err)
			}
		}
		for _, channel := range k.Channels {
			if err := applyChannel(channel); err != nil {
				klog.Warningf("error applying channel %q: %v", channel, err)
			}
		}
	}

	return nil
}

func pathFor(hostPath string) string {
	if hostPath[0] != '/' {
		klog.Fatalf("path was not absolute: %q", hostPath)
	}
	return RootFS + hostPath[1:]
}

func pathForSymlinks(hostPath string) string {
	path := pathFor(hostPath)

	symlink, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path
	}

	return symlink
}

func (k *KubeBoot) String() string {
	return DebugString(k)
}
