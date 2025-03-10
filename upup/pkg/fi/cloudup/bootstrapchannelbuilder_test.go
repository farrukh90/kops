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

package cloudup

import (
	"io/ioutil"
	"path"
	"testing"

	kopsapi "k8s.io/kops/pkg/apis/kops"
	"k8s.io/kops/pkg/assets"
	"k8s.io/kops/pkg/client/simple/vfsclientset"
	"k8s.io/kops/pkg/featureflag"
	"k8s.io/kops/pkg/kopscodecs"
	"k8s.io/kops/pkg/model"
	"k8s.io/kops/pkg/model/iam"
	"k8s.io/kops/pkg/templates"
	"k8s.io/kops/pkg/testutils"
	"k8s.io/kops/pkg/testutils/golden"
	"k8s.io/kops/upup/models"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/cloudup/bootstrapchannelbuilder"
	"k8s.io/kops/upup/pkg/fi/fitasks"
	"k8s.io/kops/util/pkg/vfs"
)

func TestBootstrapChannelBuilder_BuildTasks(t *testing.T) {
	h := testutils.NewIntegrationTestHarness(t)
	defer h.Close()

	h.SetupMockAWS()

	runChannelBuilderTest(t, "simple", []string{"dns-controller.addons.k8s.io-k8s-1.12", "kops-controller.addons.k8s.io-k8s-1.16"})
	// Use cilium networking, proxy
	runChannelBuilderTest(t, "cilium", []string{"dns-controller.addons.k8s.io-k8s-1.12", "kops-controller.addons.k8s.io-k8s-1.16"})
	runChannelBuilderTest(t, "weave", []string{})
	runChannelBuilderTest(t, "amazonvpc", []string{"networking.amazon-vpc-routed-eni-k8s-1.12", "networking.amazon-vpc-routed-eni-k8s-1.16"})
	runChannelBuilderTest(t, "amazonvpc-containerd", []string{"networking.amazon-vpc-routed-eni-k8s-1.12", "networking.amazon-vpc-routed-eni-k8s-1.16"})
	runChannelBuilderTest(t, "awsiamauthenticator", []string{"authentication.aws-k8s-1.12"})
}

func TestBootstrapChannelBuilder_AWSCloudController(t *testing.T) {
	h := testutils.NewIntegrationTestHarness(t)
	defer h.Close()

	h.SetupMockAWS()

	featureflag.ParseFlags("+EnableExternalCloudController")
	unsetFeatureFlag := func() {
		featureflag.ParseFlags("-EnableExternalCloudController")
	}
	defer unsetFeatureFlag()
	runChannelBuilderTest(t, "awscloudcontroller", []string{"aws-cloud-controller.addons.k8s.io-k8s-1.18"})
}

func runChannelBuilderTest(t *testing.T, key string, addonManifests []string) {
	basedir := path.Join("tests/bootstrapchannelbuilder/", key)

	clusterYamlPath := path.Join(basedir, "cluster.yaml")
	clusterYaml, err := ioutil.ReadFile(clusterYamlPath)
	if err != nil {
		t.Fatalf("error reading cluster yaml file %q: %v", clusterYamlPath, err)
	}
	obj, _, err := kopscodecs.Decode(clusterYaml, nil)
	if err != nil {
		t.Fatalf("error parsing cluster yaml %q: %v", clusterYamlPath, err)
	}
	cluster := obj.(*kopsapi.Cluster)

	cloud, err := BuildCloud(cluster)
	if err != nil {
		t.Fatalf("error from BuildCloud: %v", err)
	}

	if err := PerformAssignments(cluster, cloud); err != nil {
		t.Fatalf("error from PerformAssignments for %q: %v", key, err)
	}

	fullSpec, err := mockedPopulateClusterSpec(cluster, cloud)
	if err != nil {
		t.Fatalf("error from PopulateClusterSpec for %q: %v", key, err)
	}
	cluster = fullSpec

	templates, err := templates.LoadTemplates(cluster, models.NewAssetPath("cloudup/resources"))
	if err != nil {
		t.Fatalf("error building templates for %q: %v", key, err)
	}

	vfs.Context.ResetMemfsContext(true)

	basePath, err := vfs.Context.BuildVfsPath("memfs://tests")
	if err != nil {
		t.Errorf("error building vfspath: %v", err)
	}
	clientset := vfsclientset.NewVFSClientset(basePath)

	secretStore, err := clientset.SecretStore(cluster)
	if err != nil {
		t.Error(err)
	}
	role := "arn:aws:iam::1234567890108:instance-profile/kops-custom-node-role"
	kopsModel := model.KopsModelContext{
		IAMModelContext: iam.IAMModelContext{
			Cluster:      cluster,
			AWSAccountID: "123456789012",
			AWSPartition: "aws-test",
		},
		Region: "us-east-1",
		InstanceGroups: []*kopsapi.InstanceGroup{
			{
				Spec: kopsapi.InstanceGroupSpec{
					IAM: &kopsapi.IAMProfileSpec{
						Profile: &role,
					},
					Role: kopsapi.InstanceGroupRoleNode,
				},
			},
			{
				Spec: kopsapi.InstanceGroupSpec{
					Role: kopsapi.InstanceGroupRoleNode,
				},
			},
		},
	}

	tf := &TemplateFunctions{
		KopsModelContext: kopsModel,
		cloud:            cloud,
	}
	tf.AddTo(templates.TemplateFunctions, secretStore)

	bcb := bootstrapchannelbuilder.NewBootstrapChannelBuilder(
		&kopsModel,
		nil,
		assets.NewAssetBuilder(cluster, ""),
		templates,
		nil,
	)

	context := &fi.ModelBuilderContext{
		Tasks: make(map[string]fi.Task),
	}
	err = bcb.Build(context)
	if err != nil {
		t.Fatalf("error from BootstrapChannelBuilder Build: %v", err)
	}

	{
		name := cluster.ObjectMeta.Name + "-addons-bootstrap"
		manifestTask := context.Tasks["ManagedFile/"+name]
		if manifestTask == nil {
			t.Fatalf("manifest task not found (%q)", name)
		}

		manifestFileTask := manifestTask.(*fitasks.ManagedFile)
		actualManifest, err := fi.ResourceAsString(manifestFileTask.Contents)
		if err != nil {
			t.Fatalf("error getting manifest as string: %v", err)
		}

		expectedManifestPath := path.Join(basedir, "manifest.yaml")
		golden.AssertMatchesFile(t, actualManifest, expectedManifestPath)
	}

	for _, k := range addonManifests {
		name := cluster.ObjectMeta.Name + "-addons-" + k
		manifestTask := context.Tasks["ManagedFile/"+name]
		if manifestTask == nil {
			for k := range context.Tasks {
				t.Logf("found task %s", k)
			}
			t.Fatalf("manifest task not found (%q)", name)
		}

		manifestFileTask := manifestTask.(*fitasks.ManagedFile)
		actualManifest, err := fi.ResourceAsString(manifestFileTask.Contents)
		if err != nil {
			t.Fatalf("error getting manifest as string: %v", err)
		}

		expectedManifestPath := path.Join(basedir, k+".yaml")
		golden.AssertMatchesFile(t, actualManifest, expectedManifestPath)
	}
}
