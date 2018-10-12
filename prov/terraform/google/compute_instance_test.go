// Copyright 2018 Bull S.A.S. Atos Technologies - Bull, Rue Jean Jaures, B.P.68, 78340, Les Clayes-sous-Bois, France.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package google

import (
	"context"
	"github.com/hashicorp/consul/testutil"
	"github.com/ystia/yorc/helper/consulutil"
	"path"
	"testing"

	"github.com/hashicorp/consul/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ystia/yorc/config"
	"github.com/ystia/yorc/deployments"
	"github.com/ystia/yorc/prov/terraform/commons"
)

func loadTestYaml(t *testing.T, kv *api.KV) string {
	deploymentID := path.Base(t.Name())
	yamlName := "testdata/" + deploymentID + ".yaml"
	err := deployments.StoreDeploymentDefinition(context.Background(), kv, deploymentID, yamlName)
	require.NoError(t, err, "Failed to parse "+yamlName+" definition")
	return deploymentID
}

func testSimpleComputeInstance(t *testing.T, kv *api.KV, cfg config.Configuration) {
	t.Parallel()
	deploymentID := loadTestYaml(t, kv)
	infrastructure := commons.Infrastructure{}
	g := googleGenerator{}
	err := g.generateComputeInstance(context.Background(), kv, cfg, deploymentID, "ComputeInstance", "0", 0, &infrastructure, make(map[string]string))
	require.NoError(t, err, "Unexpected error attempting to generate compute instance for %s", deploymentID)

	require.Len(t, infrastructure.Resource["google_compute_instance"], 1, "Expected one compute instance")
	instancesMap := infrastructure.Resource["google_compute_instance"].(map[string]interface{})
	require.Len(t, instancesMap, 1)
	require.Contains(t, instancesMap, "computeinstance-0")

	compute, ok := instancesMap["computeinstance-0"].(*ComputeInstance)
	require.True(t, ok, "computeinstance-0 is not a ComputeInstance")
	assert.Equal(t, "n1-standard-1", compute.MachineType)
	assert.Equal(t, "europe-west1-b", compute.Zone)
	require.NotNil(t, compute.BootDisk, 1, "Expected boot disk")
	assert.Equal(t, "centos-cloud/centos-7", compute.BootDisk.InitializeParams.Image, "Unexpected boot disk image")

	require.Len(t, compute.NetworkInterfaces, 1, "Expected one network interface for external access")
	assert.Equal(t, "", compute.NetworkInterfaces[0].AccessConfigs[0].NatIP, "Unexpected external IP address")

	require.Len(t, compute.ServiceAccounts, 1, "Expected one service account")
	assert.Equal(t, "yorc@yorc.net", compute.ServiceAccounts[0].Email, "Unexpected Service Account")

	assert.Equal(t, []string{"tag1", "tag2"}, compute.Tags)
	assert.Equal(t, map[string]string{"key1": "value1", "key2": "value2"}, compute.Labels)

	require.Contains(t, infrastructure.Resource, "null_resource")
	require.Len(t, infrastructure.Resource["null_resource"], 1)
	nullResources := infrastructure.Resource["null_resource"].(map[string]interface{})

	require.Contains(t, nullResources, "computeinstance-0-ConnectionCheck")
	nullRes, ok := nullResources["computeinstance-0-ConnectionCheck"].(*commons.Resource)
	assert.True(t, ok)
	require.Len(t, nullRes.Provisioners, 1)
	mapProv := nullRes.Provisioners[0]
	require.Contains(t, mapProv, "remote-exec")
	rex, ok := mapProv["remote-exec"].(commons.RemoteExec)
	require.True(t, ok)
	assert.Equal(t, "centos", rex.Connection.User)
	assert.Equal(t, `${file("~/.ssh/yorc.pem")}`, rex.Connection.PrivateKey)

	require.Len(t, compute.ScratchDisks, 2, "Expected 2 scratch disks")
	assert.Equal(t, "SCSI", compute.ScratchDisks[0].Interface, "SCSI interface expected for 1st scratch")
	assert.Equal(t, "NVME", compute.ScratchDisks[1].Interface, "NVME interface expected for 2nd scratch")
}

func testSimpleComputeInstanceMissingMandatoryParameter(t *testing.T, kv *api.KV, cfg config.Configuration) {
	t.Parallel()
	deploymentID := loadTestYaml(t, kv)
	g := googleGenerator{}
	infrastructure := commons.Infrastructure{}

	err := g.generateComputeInstance(context.Background(), kv, cfg, deploymentID, "ComputeInstance", "0", 0, &infrastructure, make(map[string]string))
	require.Error(t, err, "Expected missing mandatory parameter error, but had no error")
	assert.Contains(t, err.Error(), "mandatory parameter zone", "Expected an error on missing parameter zone")
}

func testSimpleComputeInstanceWithAddress(t *testing.T, kv *api.KV, srv1 *testutil.TestServer, cfg config.Configuration) {
	t.Parallel()
	deploymentID := loadTestYaml(t, kv)

	// Simulate the google address "ip_address" attribute registration
	srv1.PopulateKV(t, map[string][]byte{
		path.Join(consulutil.DeploymentKVPrefix, deploymentID+"/topology/nodes/address_Compute/type"):                        []byte("yorc.nodes.google.Address"),
		path.Join(consulutil.DeploymentKVPrefix, deploymentID+"/topology/instances/address_Compute/0/attributes/ip_address"): []byte("1.2.3.4"),
	})

	infrastructure := commons.Infrastructure{}
	g := googleGenerator{}
	err := g.generateComputeInstance(context.Background(), kv, cfg, deploymentID, "Compute", "0", 0, &infrastructure, make(map[string]string))
	require.NoError(t, err, "Unexpected error attempting to generate compute instance for %s", deploymentID)

	require.Len(t, infrastructure.Resource["google_compute_instance"], 1, "Expected one compute instance")
	instancesMap := infrastructure.Resource["google_compute_instance"].(map[string]interface{})
	require.Len(t, instancesMap, 1)
	require.Contains(t, instancesMap, "compute-0")

	compute, ok := instancesMap["compute-0"].(*ComputeInstance)
	require.True(t, ok, "compute-0 is not a ComputeInstance")
	assert.Equal(t, "n1-standard-1", compute.MachineType)
	assert.Equal(t, "europe-west1-b", compute.Zone)
	require.NotNil(t, compute.BootDisk, 1, "Expected boot disk")
	assert.Equal(t, "centos-cloud/centos-7", compute.BootDisk.InitializeParams.Image, "Unexpected boot disk image")

	require.Len(t, compute.NetworkInterfaces, 1, "Expected one network interface for external access")
	assert.Equal(t, "1.2.3.4", compute.NetworkInterfaces[0].AccessConfigs[0].NatIP, "Unexpected external IP address")
}

func testSimpleComputeInstanceWithPersistentDisk(t *testing.T, kv *api.KV, srv1 *testutil.TestServer, cfg config.Configuration) {
	t.Parallel()
	deploymentID := loadTestYaml(t, kv)

	// Simulate the google persistent disk "volume_id" attribute registration
	srv1.PopulateKV(t, map[string][]byte{
		path.Join(consulutil.DeploymentKVPrefix, deploymentID+"/topology/nodes/BS1/type"):                       []byte("yorc.nodes.google.PersistentDisk"),
		path.Join(consulutil.DeploymentKVPrefix, deploymentID+"/topology/instances/BS1/0/attributes/volume_id"): []byte("my_vol_id"),
	})

	infrastructure := commons.Infrastructure{}
	g := googleGenerator{}
	err := g.generateComputeInstance(context.Background(), kv, cfg, deploymentID, "Compute", "0", 0, &infrastructure, make(map[string]string))
	require.NoError(t, err, "Unexpected error attempting to generate compute instance for %s", deploymentID)

	require.Len(t, infrastructure.Resource["google_compute_instance"], 1, "Expected one compute instance")
	instancesMap := infrastructure.Resource["google_compute_instance"].(map[string]interface{})
	require.Len(t, instancesMap, 1)
	require.Contains(t, instancesMap, "compute-0")

	compute, ok := instancesMap["compute-0"].(*ComputeInstance)
	require.True(t, ok, "compute-0 is not a ComputeInstance")
	assert.Equal(t, "n1-standard-1", compute.MachineType)
	assert.Equal(t, "europe-west1-b", compute.Zone)
	require.NotNil(t, compute.BootDisk, 1, "Expected boot disk")
	assert.Equal(t, "centos-cloud/centos-7", compute.BootDisk.InitializeParams.Image, "Unexpected boot disk image")

	require.Len(t, infrastructure.Resource["google_compute_attached_disk"], 1, "Expected one attached disk")
	instancesMap = infrastructure.Resource["google_compute_attached_disk"].(map[string]interface{})
	require.Len(t, instancesMap, 1)

	require.Contains(t, instancesMap, "bs1-0-attached-to-compute-0")
	attachedDisk, ok := instancesMap["bs1-0-attached-to-compute-0"].(*ComputeAttachedDisk)
	require.True(t, ok, "bs1-0-attached-to-compute-0 is not a ComputeAttachedDisk")
	assert.Equal(t, "my_vol_id", attachedDisk.Disk)
	assert.Equal(t, "${google_compute_instance.compute-0.name}", attachedDisk.Instance)
	assert.Equal(t, "europe-west1-b", attachedDisk.Zone)
	assert.Equal(t, "foo", attachedDisk.DeviceName)
	assert.Equal(t, "READ_ONLY", attachedDisk.Mode)
}
