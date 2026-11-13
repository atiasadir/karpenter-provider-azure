/*
Portions Copyright (c) Microsoft Corporation.

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

package flex_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v9"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"

	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/consts"
	"github.com/Azure/karpenter-provider-azure/pkg/fake"
	"github.com/Azure/karpenter-provider-azure/pkg/operator/options"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/flex"
	"github.com/Azure/karpenter-provider-azure/pkg/test"
)

type fixture struct {
	ctx        context.Context
	opts       *options.Options
	nodeClass  *v1beta1.AKSNodeClass
	provider   *flex.Provider
	networks   *fake.VirtualNetworksAPI
	images     *fake.VirtualMachineImagesAPI
	identities *fake.UserAssignedIdentitiesAPI
	pools      *fake.AKSAgentPoolsAPI
	storage    *fake.AKSDataStorage
	pool       armcontainerservice.AgentPool
}

func newFixture() *fixture {
	f := &fixture{
		opts: test.Options(test.OptionsFields{
			EnableFlexNodes: lo.ToPtr(true),
			NetworkPlugin:   lo.ToPtr(consts.NetworkPluginNone),
		}),
		nodeClass:  test.AKSNodeClass(test.WithFlex("subscriptionID", "test-cluster")),
		networks:   &fake.VirtualNetworksAPI{Location: "eastus2"},
		images:     &fake.VirtualMachineImagesAPI{},
		identities: &fake.UserAssignedIdentitiesAPI{ClientID: test.FlexIdentityClientID},
		storage:    fake.NewAKSDataStorage(),
	}
	f.ctx = options.ToContext(context.Background(), f.opts)
	f.nodeClass.Spec.MaxPods = lo.ToPtr(int32(110))
	f.nodeClass.Status.KubernetesVersion = lo.ToPtr("1.35.9")
	f.nodeClass.StatusConditions().SetTrue(v1beta1.ConditionTypeKubernetesVersionReady)
	f.pool = armcontainerservice.AgentPool{
		Properties: &armcontainerservice.ManagedClusterAgentPoolProfileProperties{
			Type:                       lo.ToPtr(armcontainerservice.AgentPoolType("FlexNodes")),
			ProvisioningState:          lo.ToPtr("Succeeded"),
			CurrentOrchestratorVersion: lo.ToPtr("1.35.9"),
			MaxPods:                    lo.ToPtr(int32(110)),
		},
	}
	f.storage.AgentPools.Store(fake.MkAgentPoolID("test-cluster-resource-group", "test-cluster", test.FlexPoolName), f.pool)
	f.pools = fake.NewAKSAgentPoolsAPI(f.storage)
	f.provider = flex.NewProvider("subscriptionID", "test-cluster-resource-group", "test-cluster", "eastus2",
		f.networks, f.images, f.identities, fake.NewAKSMachinesAPI(f.storage), f.pools)
	return f
}

func TestResolveFlexPrerequisites(t *testing.T) {
	g := NewWithT(t)
	f := newFixture()
	image, err := f.provider.Resolve(f.ctx, f.nodeClass)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(image.ID).To(Equal("Canonical:ubuntu-24_04-lts:server:" + test.FlexHostImageVersion))
	g.Expect(image.Requirements).To(HaveLen(2))
	g.Expect(f.pools.AgentPoolDeleteMachinesBehavior.CalledWithInput.Len()).To(BeZero())

	f.pool.Properties.CurrentOrchestratorVersion = nil
	f.pool.Properties.OrchestratorVersion = lo.ToPtr("1.35.9")
	_, err = f.provider.Resolve(f.ctx, f.nodeClass)
	g.Expect(err).ToNot(HaveOccurred())
}

func TestResolveFlexRejectsUnsupportedPrerequisites(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		change func(*fixture)
	}{
		{"disabled", "FlexNodesDisabled", func(f *fixture) { f.opts.EnableFlexNodes = false }},
		{"machine API", "FlexUnsupportedProvisionMode", func(f *fixture) { f.opts.ProvisionMode = consts.ProvisionModeAKSMachineAPI }},
		{"batched machine API", "FlexUnsupportedProvisionMode", func(f *fixture) { f.opts.ProvisionMode = consts.ProvisionModeAKSMachineAPIHeaderBatch }},
		{"bootstrap client", "FlexUnsupportedProvisionMode", func(f *fixture) { f.opts.ProvisionMode = consts.ProvisionModeBootstrappingClient }},
		{"managed addon", "FlexUnsupportedProvisionMode", func(f *fixture) { f.opts.UseSIG = true }},
		{"managed Azure CNI", "FlexRequiresNoCNICluster", func(f *fixture) {
			f.opts.NetworkPlugin = consts.NetworkPluginAzure
			f.opts.NetworkPluginMode = consts.NetworkPluginModeOverlay
		}},
		{"cross-region subnet", "FlexRegionMismatch", func(f *fixture) { f.networks.Location = "westus2" }},
		{"unknown subnet region", "FlexRegionMismatch", func(f *fixture) { f.networks.Location = "" }},
		{"different cluster pool", "FlexInvalidPool", func(f *fixture) {
			f.nodeClass.Spec.Flex.AgentPoolID = "/subscriptions/subscriptionID/resourceGroups/other/providers/Microsoft.ContainerService/managedClusters/other/agentPools/flexpool"
		}},
		{"native pool", "FlexPoolNotReady", func(f *fixture) {
			f.pool.Properties.Type = lo.ToPtr(armcontainerservice.AgentPoolType("VirtualMachines"))
		}},
		{"pool still creating", "FlexPoolNotReady", func(f *fixture) { f.pool.Properties.ProvisioningState = lo.ToPtr("Creating") }},
		{"pool version mismatch", "FlexPoolVersionMismatch", func(f *fixture) { f.pool.Properties.CurrentOrchestratorVersion = lo.ToPtr("1.34.9") }},
		{"unpinned pool version", "FlexPoolVersionMismatch", func(f *fixture) {
			f.nodeClass.Status.KubernetesVersion = lo.ToPtr("1.35.0")
			f.pool.Properties.CurrentOrchestratorVersion = nil
			f.pool.Properties.OrchestratorVersion = lo.ToPtr("1.35")
		}},
		{"pool density mismatch", "FlexPoolMaxPodsMismatch", func(f *fixture) { f.pool.Properties.MaxPods = lo.ToPtr(int32(250)) }},
		{"identity mismatch", "FlexIdentityMismatch", func(f *fixture) { f.identities.ClientID = "22222222-2222-4333-8444-555555555555" }},
		{"unpinned host image", "FlexInvalidHostImageVersion", func(f *fixture) { f.nodeClass.Spec.Flex.HostImageVersion = "latest" }},
		{"native image pin", "FlexUnsupportedImagePin", func(f *fixture) {
			f.nodeClass.Spec.Versions = &v1beta1.Versions{KubernetesVersion: lo.ToPtr("1.35.9"), NodeImageVersion: lo.ToPtr("202611.01.0")}
		}},
		{"public IP", "FlexUnsupportedConfiguration", func(f *fixture) {
			f.nodeClass.Spec.NodePublicIP = &v1beta1.NodePublicIP{Enabled: lo.ToPtr(true)}
		}},
		{"missing image properties", "FlexUnsupportedHostImage", func(f *fixture) {
			f.images.GetFunc = func(context.Context, string, string, string, string, string, *armcompute.VirtualMachineImagesClientGetOptions) (armcompute.VirtualMachineImagesClientGetResponse, error) {
				return armcompute.VirtualMachineImagesClientGetResponse{}, nil
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			f := newFixture()
			tc.change(f)
			_, err := f.provider.Resolve(f.ctx, f.nodeClass)
			var configError *flex.ConfigurationError
			g.Expect(errors.As(err, &configError)).To(BeTrue(), "error: %v", err)
			g.Expect(configError.Reason).To(Equal(tc.reason))
			g.Expect(f.pools.AgentPoolDeleteMachinesBehavior.CalledWithInput.Len()).To(BeZero())
		})
	}
}

func TestResolveFlexPreservesAzureErrors(t *testing.T) {
	g := NewWithT(t)
	f := newFixture()
	expected := errors.New("image API unavailable")
	f.images.GetFunc = func(context.Context, string, string, string, string, string, *armcompute.VirtualMachineImagesClientGetOptions) (armcompute.VirtualMachineImagesClientGetResponse, error) {
		return armcompute.VirtualMachineImagesClientGetResponse{}, expected
	}
	_, err := f.provider.Resolve(f.ctx, f.nodeClass)
	g.Expect(errors.Is(err, expected)).To(BeTrue())
}

func TestFlexCleanupIsScopedAndIdempotent(t *testing.T) {
	g := NewWithT(t)
	f := newFixture()
	owned := fake.MkMachineID("test-cluster-resource-group", "test-cluster", test.FlexPoolName, "aks-worker")
	other := fake.MkMachineID("test-cluster-resource-group", "test-cluster", test.FlexPoolName, "aks-other")
	f.storage.AKSMachines.Store(owned, armcontainerservice.Machine{})
	f.storage.AKSMachines.Store(other, armcontainerservice.Machine{})
	f.opts.EnableFlexNodes = false

	g.Expect(f.provider.DeleteMachine(f.ctx, f.nodeClass.Spec.Flex.AgentPoolID, "aks-worker")).To(Succeed())
	g.Expect(f.provider.DeleteMachine(f.ctx, f.nodeClass.Spec.Flex.AgentPoolID, "aks-worker")).To(Succeed())
	_, exists := f.storage.AKSMachines.Load(owned)
	g.Expect(exists).To(BeFalse())
	_, exists = f.storage.AKSMachines.Load(other)
	g.Expect(exists).To(BeTrue())
	_, exists = f.storage.AgentPools.Load(fake.MkAgentPoolID("test-cluster-resource-group", "test-cluster", test.FlexPoolName))
	g.Expect(exists).To(BeTrue())
	g.Expect(f.pools.AgentPoolDeleteMachinesBehavior.CalledWithInput.Len()).To(Equal(1))
}

func TestFlexCleanupRetriesWithoutLosingOwnership(t *testing.T) {
	g := NewWithT(t)
	f := newFixture()
	owned := fake.MkMachineID("test-cluster-resource-group", "test-cluster", test.FlexPoolName, "aks-worker")
	f.storage.AKSMachines.Store(owned, armcontainerservice.Machine{})
	expected := errors.New("delete temporarily unavailable")
	f.pools.AgentPoolDeleteMachinesBehavior.Error.Set(expected)

	err := f.provider.DeleteMachine(f.ctx, f.nodeClass.Spec.Flex.AgentPoolID, "aks-worker")
	g.Expect(errors.Is(err, expected)).To(BeTrue())
	_, exists := f.storage.AKSMachines.Load(owned)
	g.Expect(exists).To(BeTrue())
	g.Expect(f.provider.DeleteMachine(f.ctx, f.nodeClass.Spec.Flex.AgentPoolID, "aks-worker")).To(Succeed())
	_, exists = f.storage.AKSMachines.Load(owned)
	g.Expect(exists).To(BeFalse())
}

func TestFlexCleanupRejectsNativePools(t *testing.T) {
	g := NewWithT(t)
	f := newFixture()
	f.pool.Properties.Type = lo.ToPtr(armcontainerservice.AgentPoolType("VirtualMachines"))
	g.Expect(f.provider.DeleteMachine(f.ctx, f.nodeClass.Spec.Flex.AgentPoolID, "aks-worker")).ToNot(Succeed())
	g.Expect(f.pools.AgentPoolDeleteMachinesBehavior.CalledWithInput.Len()).To(BeZero())
}
