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

package model

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"k8s.io/kops/pkg/apis/kops"
	"k8s.io/kops/pkg/apis/kops/util"
	"k8s.io/kops/pkg/assets"
	"k8s.io/kops/pkg/dns"
	"k8s.io/kops/pkg/flagbuilder"
	"k8s.io/kops/pkg/rbac"
	"k8s.io/kops/pkg/systemd"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/nodeup/nodetasks"
	"k8s.io/kops/util/pkg/distributions"

	"k8s.io/kops/util/pkg/proxy"

	"github.com/blang/semver/v4"
	"k8s.io/klog/v2"
)

// ProtokubeBuilder configures protokube
type ProtokubeBuilder struct {
	*NodeupModelContext
}

var _ fi.ModelBuilder = &ProtokubeBuilder{}

// Build is responsible for generating the options for protokube
func (t *ProtokubeBuilder) Build(c *fi.ModelBuilderContext) error {
	useGossip := dns.IsGossipHostname(t.Cluster.Spec.MasterInternalName)

	// check is not a master and we are not using gossip (https://github.com/kubernetes/kops/pull/3091)
	if !t.IsMaster && !useGossip {
		klog.V(2).Infof("skipping the provisioning of protokube on the nodes")
		return nil
	}

	{
		name, res, err := t.Assets.FindMatch(regexp.MustCompile("protokube$"))
		if err != nil {
			return err
		}

		c.AddTask(&nodetasks.File{
			Path:     filepath.Join("/opt/kops/bin", name),
			Contents: res,
			Type:     nodetasks.FileType_File,
			Mode:     fi.String("0755"),
		})
	}

	{
		name, res, err := t.Assets.FindMatch(regexp.MustCompile("channels$"))
		if err != nil {
			return err
		}

		c.AddTask(&nodetasks.File{
			Path:     filepath.Join("/opt/kops/bin", name),
			Contents: res,
			Type:     nodetasks.FileType_File,
			Mode:     fi.String("0755"),
		})
	}

	if t.IsMaster {
		name := nodetasks.PKIXName{
			CommonName:   "kops",
			Organization: []string{rbac.SystemPrivilegedGroup},
		}
		kubeconfig := t.BuildIssuedKubeconfig("kops", name, c)

		c.AddTask(&nodetasks.File{
			Path:     "/var/lib/kops/kubeconfig",
			Contents: kubeconfig,
			Type:     nodetasks.FileType_File,
			Mode:     s("0400"),
		})

		// retrieve the etcd peer certificates and private keys from the keystore
		if !t.UseEtcdManager() && t.UseEtcdTLS() {
			for _, x := range []string{"etcd", "etcd-peer", "etcd-client"} {
				if err := t.BuildCertificateTask(c, x, fmt.Sprintf("%s.pem", x), nil); err != nil {
					return err
				}
			}
			for _, x := range []string{"etcd", "etcd-peer", "etcd-client"} {
				if err := t.BuildPrivateKeyTask(c, x, fmt.Sprintf("%s-key.pem", x), nil); err != nil {
					return err
				}
			}
		}
	}

	envFile, err := t.buildEnvFile()
	if err != nil {
		return err
	}
	c.AddTask(envFile)

	service, err := t.buildSystemdService()
	if err != nil {
		return err
	}
	c.AddTask(service)

	// DBUS is needed for the /var/run/dbus mount on kope.io images (based on Debian 9),
	// at least until we can move to etcd-manager or start protokube as a service
	// See https://github.com/kubernetes/kops/issues/10122#issuecomment-752969613
	if t.Distribution == distributions.DistributionDebian9 {
		c.AddTask(&nodetasks.Package{Name: "dbus"})
	}

	return nil
}

// buildSystemdService generates the manifest for the protokube service
func (t *ProtokubeBuilder) buildSystemdService() (*nodetasks.Service, error) {
	k8sVersion, err := util.ParseKubernetesVersion(t.Cluster.Spec.KubernetesVersion)
	if err != nil || k8sVersion == nil {
		return nil, fmt.Errorf("unable to parse KubernetesVersion %q", t.Cluster.Spec.KubernetesVersion)
	}

	protokubeFlags, err := t.ProtokubeFlags(*k8sVersion)
	if err != nil {
		return nil, err
	}
	protokubeRunArgs, err := flagbuilder.BuildFlags(protokubeFlags)
	if err != nil {
		return nil, err
	}

	manifest := &systemd.Manifest{}
	manifest.Set("Unit", "Description", "Kubernetes Protokube Service")
	manifest.Set("Unit", "Documentation", "https://github.com/kubernetes/kops")

	manifest.Set("Service", "ExecStart", "/opt/kops/bin/protokube"+" "+protokubeRunArgs)
	manifest.Set("Service", "EnvironmentFile", "/etc/sysconfig/protokube")
	manifest.Set("Service", "Restart", "always")
	manifest.Set("Service", "RestartSec", "3s")
	manifest.Set("Service", "StartLimitInterval", "0")
	manifest.Set("Install", "WantedBy", "multi-user.target")

	manifestString := manifest.Render()
	klog.V(8).Infof("Built service manifest %q\n%s", "protokube", manifestString)

	service := &nodetasks.Service{
		Name:       "protokube.service",
		Definition: s(manifestString),
	}

	service.InitDefaults()

	return service, nil
}

// ProtokubeFlags are the flags for protokube
type ProtokubeFlags struct {
	ApplyTaints               *bool    `json:"applyTaints,omitempty" flag:"apply-taints"`
	Channels                  []string `json:"channels,omitempty" flag:"channels"`
	Cloud                     *string  `json:"cloud,omitempty" flag:"cloud"`
	Containerized             *bool    `json:"containerized,omitempty" flag:"containerized"`
	DNSInternalSuffix         *string  `json:"dnsInternalSuffix,omitempty" flag:"dns-internal-suffix"`
	DNSProvider               *string  `json:"dnsProvider,omitempty" flag:"dns"`
	DNSServer                 *string  `json:"dns-server,omitempty" flag:"dns-server"`
	EtcdBackupImage           string   `json:"etcd-backup-image,omitempty" flag:"etcd-backup-image"`
	EtcdBackupStore           string   `json:"etcd-backup-store,omitempty" flag:"etcd-backup-store"`
	EtcdImage                 *string  `json:"etcd-image,omitempty" flag:"etcd-image"`
	EtcdLeaderElectionTimeout *string  `json:"etcd-election-timeout,omitempty" flag:"etcd-election-timeout"`
	EtcdHearbeatInterval      *string  `json:"etcd-heartbeat-interval,omitempty" flag:"etcd-heartbeat-interval"`
	InitializeRBAC            *bool    `json:"initializeRBAC,omitempty" flag:"initialize-rbac"`
	LogLevel                  *int32   `json:"logLevel,omitempty" flag:"v"`
	Master                    *bool    `json:"master,omitempty" flag:"master"`
	PeerTLSCaFile             *string  `json:"peer-ca,omitempty" flag:"peer-ca"`
	PeerTLSCertFile           *string  `json:"peer-cert,omitempty" flag:"peer-cert"`
	PeerTLSKeyFile            *string  `json:"peer-key,omitempty" flag:"peer-key"`
	TLSAuth                   *bool    `json:"tls-auth,omitempty" flag:"tls-auth"`
	TLSCAFile                 *string  `json:"tls-ca,omitempty" flag:"tls-ca"`
	TLSCertFile               *string  `json:"tls-cert,omitempty" flag:"tls-cert"`
	TLSKeyFile                *string  `json:"tls-key,omitempty" flag:"tls-key"`
	Zone                      []string `json:"zone,omitempty" flag:"zone"`

	// ManageEtcd is true if protokube should manage etcd; being replaced by etcd-manager
	ManageEtcd bool `json:"manageEtcd,omitempty" flag:"manage-etcd"`

	// RemoveDNSNames allows us to remove dns records, so that they can be managed elsewhere
	// We use it e.g. for the switch to etcd-manager
	RemoveDNSNames string `json:"removeDNSNames,omitempty" flag:"remove-dns-names"`

	// BootstrapMasterNodeLabels applies the critical node-role labels to our node,
	// which lets us bring up the controllers that can only run on masters, which are then
	// responsible for node labels.  The node is specified by NodeName
	BootstrapMasterNodeLabels bool `json:"bootstrapMasterNodeLabels,omitempty" flag:"bootstrap-master-node-labels"`

	// NodeName is the name of the node as will be created in kubernetes.  Primarily used by BootstrapMasterNodeLabels.
	NodeName string `json:"nodeName,omitempty" flag:"node-name"`

	GossipProtocol *string `json:"gossip-protocol" flag:"gossip-protocol"`
	GossipListen   *string `json:"gossip-listen" flag:"gossip-listen"`
	GossipSecret   *string `json:"gossip-secret" flag:"gossip-secret"`

	GossipProtocolSecondary *string `json:"gossip-protocol-secondary" flag:"gossip-protocol-secondary" flag-include-empty:"true"`
	GossipListenSecondary   *string `json:"gossip-listen-secondary" flag:"gossip-listen-secondary"`
	GossipSecretSecondary   *string `json:"gossip-secret-secondary" flag:"gossip-secret-secondary"`
}

// ProtokubeFlags is responsible for building the command line flags for protokube
func (t *ProtokubeBuilder) ProtokubeFlags(k8sVersion semver.Version) (*ProtokubeFlags, error) {
	imageVersion := t.Cluster.Spec.EtcdClusters[0].Version
	// overrides imageVersion if set
	etcdContainerImage := t.Cluster.Spec.EtcdClusters[0].Image

	var leaderElectionTimeout string
	var heartbeatInterval string

	if v := t.Cluster.Spec.EtcdClusters[0].LeaderElectionTimeout; v != nil {
		leaderElectionTimeout = convEtcdSettingsToMs(v)
	}

	if v := t.Cluster.Spec.EtcdClusters[0].HeartbeatInterval; v != nil {
		heartbeatInterval = convEtcdSettingsToMs(v)
	}

	f := &ProtokubeFlags{
		Channels:                  t.NodeupConfig.Channels,
		Containerized:             fi.Bool(false),
		EtcdLeaderElectionTimeout: s(leaderElectionTimeout),
		EtcdHearbeatInterval:      s(heartbeatInterval),
		LogLevel:                  fi.Int32(4),
		Master:                    b(t.IsMaster),
	}

	f.ManageEtcd = false
	if len(t.NodeupConfig.EtcdManifests) == 0 {
		klog.V(4).Infof("no EtcdManifests; protokube will manage etcd")
		f.ManageEtcd = true
	}

	if f.ManageEtcd {
		for _, e := range t.Cluster.Spec.EtcdClusters {
			// Because we can only specify a single EtcdBackupStore at the moment, we only backup main, not events
			if e.Name != "main" {
				continue
			}

			if e.Backups != nil {
				if f.EtcdBackupImage == "" {
					f.EtcdBackupImage = e.Backups.Image
				}

				if f.EtcdBackupStore == "" {
					f.EtcdBackupStore = e.Backups.BackupStore
				}
			}
		}

		// TODO this is duplicate code with etcd model
		image := fmt.Sprintf("k8s.gcr.io/etcd:%s", imageVersion)
		// override image if set as API value
		if etcdContainerImage != "" {
			image = etcdContainerImage
		}
		assets := assets.NewAssetBuilder(t.Cluster, "")
		remapped, err := assets.RemapImage(image)
		if err != nil {
			return nil, fmt.Errorf("unable to remap container %q: %v", image, err)
		}

		image = remapped
		f.EtcdImage = s(image)

		// check if we are using tls and add the options to protokube
		if t.UseEtcdTLS() {
			f.PeerTLSCaFile = s(filepath.Join(t.PathSrvKubernetes(), "ca.crt"))
			f.PeerTLSCertFile = s(filepath.Join(t.PathSrvKubernetes(), "etcd-peer.pem"))
			f.PeerTLSKeyFile = s(filepath.Join(t.PathSrvKubernetes(), "etcd-peer-key.pem"))
			f.TLSCAFile = s(filepath.Join(t.PathSrvKubernetes(), "ca.crt"))
			f.TLSCertFile = s(filepath.Join(t.PathSrvKubernetes(), "etcd.pem"))
			f.TLSKeyFile = s(filepath.Join(t.PathSrvKubernetes(), "etcd-key.pem"))
		}
		if t.UseEtcdTLSAuth() {
			enableAuth := true
			f.TLSAuth = b(enableAuth)
		}
	}

	f.InitializeRBAC = fi.Bool(true)

	zone := t.Cluster.Spec.DNSZone
	if zone != "" {
		if strings.Contains(zone, ".") {
			// match by name
			f.Zone = append(f.Zone, zone)
		} else {
			// match by id
			f.Zone = append(f.Zone, "*/"+zone)
		}
	} else {
		klog.Warningf("DNSZone not specified; protokube won't be able to update DNS")
		// @TODO: Should we permit wildcard updates if zone is not specified?
		//argv = append(argv, "--zone=*/*")
	}

	if dns.IsGossipHostname(t.Cluster.Spec.MasterInternalName) {
		klog.Warningf("MasterInternalName %q implies gossip DNS", t.Cluster.Spec.MasterInternalName)
		f.DNSProvider = fi.String("gossip")
		if t.Cluster.Spec.GossipConfig != nil {
			f.GossipProtocol = t.Cluster.Spec.GossipConfig.Protocol
			f.GossipListen = t.Cluster.Spec.GossipConfig.Listen
			f.GossipSecret = t.Cluster.Spec.GossipConfig.Secret

			if t.Cluster.Spec.GossipConfig.Secondary != nil {
				f.GossipProtocolSecondary = t.Cluster.Spec.GossipConfig.Secondary.Protocol
				f.GossipListenSecondary = t.Cluster.Spec.GossipConfig.Secondary.Listen
				f.GossipSecretSecondary = t.Cluster.Spec.GossipConfig.Secondary.Secret
			}
		}

		// @TODO: This is hacky, but we want it so that we can have a different internal & external name
		internalSuffix := t.Cluster.Spec.MasterInternalName
		internalSuffix = strings.TrimPrefix(internalSuffix, "api.")
		f.DNSInternalSuffix = fi.String(internalSuffix)
	}

	if t.Cluster.Spec.CloudProvider != "" {
		f.Cloud = fi.String(t.Cluster.Spec.CloudProvider)

		if f.DNSProvider == nil {
			switch kops.CloudProviderID(t.Cluster.Spec.CloudProvider) {
			case kops.CloudProviderAWS:
				f.DNSProvider = fi.String("aws-route53")
			case kops.CloudProviderDO:
				f.DNSProvider = fi.String("digitalocean")
			case kops.CloudProviderGCE:
				f.DNSProvider = fi.String("google-clouddns")
			default:
				klog.Warningf("Unknown cloudprovider %q; won't set DNS provider", t.Cluster.Spec.CloudProvider)
			}
		}
	}

	if f.DNSInternalSuffix == nil {
		f.DNSInternalSuffix = fi.String(".internal." + t.Cluster.ObjectMeta.Name)
	}

	if k8sVersion.Major == 1 && k8sVersion.Minor >= 16 {
		f.BootstrapMasterNodeLabels = true

		nodeName, err := t.NodeName()
		if err != nil {
			return nil, fmt.Errorf("error getting NodeName: %v", err)
		}
		f.NodeName = nodeName
	}

	// Remove DNS names if we're using etcd-manager
	if !f.ManageEtcd {
		var names []string

		// Mirroring the logic used to construct DNS names in protokube/pkg/protokube/etcd_cluster.go
		suffix := fi.StringValue(f.DNSInternalSuffix)
		if !strings.HasPrefix(suffix, ".") {
			suffix = "." + suffix
		}

		for _, c := range t.Cluster.Spec.EtcdClusters {
			clusterName := "etcd-" + c.Name
			if clusterName == "etcd-main" {
				clusterName = "etcd"
			}
			for _, m := range c.Members {
				name := clusterName + "-" + m.Name + suffix
				names = append(names, name)
			}
		}

		f.RemoveDNSNames = strings.Join(names, ",")
	}

	return f, nil
}

func (t *ProtokubeBuilder) buildEnvFile() (*nodetasks.File, error) {
	var envVars = make(map[string]string)

	envVars["KUBECONFIG"] = "/var/lib/kops/kubeconfig"

	// Pass in gossip dns connection limit
	if os.Getenv("GOSSIP_DNS_CONN_LIMIT") != "" {
		envVars["GOSSIP_DNS_CONN_LIMIT"] = os.Getenv("GOSSIP_DNS_CONN_LIMIT")
	}

	// Pass in required credentials when using user-defined s3 endpoint
	if os.Getenv("AWS_REGION") != "" {
		envVars["AWS_REGION"] = os.Getenv("AWS_REGION")
	}

	if os.Getenv("S3_ENDPOINT") != "" {
		envVars["S3_ENDPOINT"] = os.Getenv("S3_ENDPOINT")
		envVars["S3_REGION"] = os.Getenv("S3_REGION")
		envVars["S3_ACCESS_KEY_ID"] = os.Getenv("S3_ACCESS_KEY_ID")
		envVars["S3_SECRET_ACCESS_KEY"] = os.Getenv("S3_SECRET_ACCESS_KEY")
	}

	if os.Getenv("OS_AUTH_URL") != "" {
		for _, envVar := range []string{
			"OS_TENANT_ID", "OS_TENANT_NAME", "OS_PROJECT_ID", "OS_PROJECT_NAME",
			"OS_PROJECT_DOMAIN_NAME", "OS_PROJECT_DOMAIN_ID",
			"OS_DOMAIN_NAME", "OS_DOMAIN_ID",
			"OS_USERNAME",
			"OS_PASSWORD",
			"OS_AUTH_URL",
			"OS_REGION_NAME",
			"OS_APPLICATION_CREDENTIAL_ID",
			"OS_APPLICATION_CREDENTIAL_SECRET",
		} {
			envVars[envVar] = os.Getenv(envVar)
		}
	}

	if kops.CloudProviderID(t.Cluster.Spec.CloudProvider) == kops.CloudProviderDO && os.Getenv("DIGITALOCEAN_ACCESS_TOKEN") != "" {
		envVars["DIGITALOCEAN_ACCESS_TOKEN"] = os.Getenv("DIGITALOCEAN_ACCESS_TOKEN")
	}

	if os.Getenv("OSS_REGION") != "" {
		envVars["OSS_REGION"] = os.Getenv("OSS_REGION")
	}

	if os.Getenv("ALIYUN_ACCESS_KEY_ID") != "" {
		envVars["ALIYUN_ACCESS_KEY_ID"] = os.Getenv("ALIYUN_ACCESS_KEY_ID")
		envVars["ALIYUN_ACCESS_KEY_SECRET"] = os.Getenv("ALIYUN_ACCESS_KEY_SECRET")
	}

	if os.Getenv("AZURE_STORAGE_ACCOUNT") != "" {
		envVars["AZURE_STORAGE_ACCOUNT"] = os.Getenv("AZURE_STORAGE_ACCOUNT")
	}

	for _, envVar := range proxy.GetProxyEnvVars(t.Cluster.Spec.EgressProxy) {
		envVars[envVar.Name] = envVar.Value
	}

	switch t.Distribution {
	case distributions.DistributionFlatcar:
		envVars["PATH"] = fmt.Sprintf("/opt/kops/bin:%v", os.Getenv("PATH"))
	}

	var sysconfig = ""
	for key, value := range envVars {
		sysconfig += key + "=" + value + "\n"
	}

	task := &nodetasks.File{
		Path:     "/etc/sysconfig/protokube",
		Contents: fi.NewStringResource(sysconfig),
		Type:     nodetasks.FileType_File,
	}

	return task, nil
}
