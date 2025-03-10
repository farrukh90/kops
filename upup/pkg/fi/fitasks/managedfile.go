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

package fitasks

import (
	"bytes"
	"fmt"
	"os"

	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/kops/pkg/acls"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/util/pkg/vfs"
)

// +kops:fitask
type ManagedFile struct {
	Name      *string
	Lifecycle *fi.Lifecycle

	Base     *string
	Location *string
	Contents fi.Resource
	Public   *bool
}

func (e *ManagedFile) Find(c *fi.Context) (*ManagedFile, error) {
	managedFiles, err := getBasePath(c, e)
	if err != nil {
		return nil, err
	}

	location := fi.StringValue(e.Location)
	if location == "" {
		return nil, nil
	}

	existingData, err := managedFiles.Join(location).ReadFile()
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	actual := &ManagedFile{
		Name:     e.Name,
		Base:     e.Base,
		Location: e.Location,
		Contents: fi.NewBytesResource(existingData),
	}

	// Avoid spurious changes
	actual.Lifecycle = e.Lifecycle

	return actual, nil
}

func (e *ManagedFile) Run(c *fi.Context) error {
	return fi.DefaultDeltaRunMethod(e, c)
}

func (s *ManagedFile) CheckChanges(a, e, changes *ManagedFile) error {
	if a != nil {
		if changes.Name != nil {
			return fi.CannotChangeField("Name")
		}
	}
	if e.Contents == nil {
		return field.Required(field.NewPath("Contents"), "")
	}
	return nil
}

func (_ *ManagedFile) Render(c *fi.Context, a, e, changes *ManagedFile) error {
	location := fi.StringValue(e.Location)
	if location == "" {
		return fi.RequiredField("Location")
	}

	data, err := fi.ResourceAsBytes(e.Contents)
	if err != nil {
		return fmt.Errorf("error reading contents of ManagedFile: %v", err)
	}

	p, err := getBasePath(c, e)
	if err != nil {
		return err
	}
	p = p.Join(location)

	var acl vfs.ACL
	if fi.BoolValue(e.Public) {
		switch p.(type) {
		case *vfs.S3Path:
			acl = &vfs.S3Acl{
				RequestACL: fi.String("public-read"),
			}
		default:
			return fmt.Errorf("the %q path does not support public ACL", p.Path())
		}

	} else {

		acl, err = acls.GetACL(p, c.Cluster)
		if err != nil {
			return err
		}
	}

	err = p.WriteFile(bytes.NewReader(data), acl)
	if err != nil {
		return fmt.Errorf("error creating ManagedFile %q: %v", location, err)
	}

	return nil
}

func getBasePath(c *fi.Context, e *ManagedFile) (vfs.Path, error) {
	base := fi.StringValue(e.Base)
	if base != "" {
		p, err := vfs.Context.BuildVfsPath(base)
		if err != nil {
			return nil, fmt.Errorf("error parsing ManagedFile Base %q: %v", base, err)
		}
		return p, nil
	}

	return c.ClusterConfigBase, nil
}
