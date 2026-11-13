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

package test

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v9"
	"github.com/samber/lo"

	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/fake"
	"github.com/Azure/karpenter-provider-azure/pkg/operator/options"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/flex"
	"github.com/Azure/karpenter-provider-azure/pkg/utils"
)

const (
	FlexPoolName         = "flexpool"
	FlexIdentityClientID = "11111111-2222-4333-8444-555555555555"
	FlexHostImageVersion = "24.04.202611010"
)

func WithFlex(subscriptionID, clusterName string) AKSNodeClassOption {
	return func(nodeClass *v1beta1.AKSNodeClass) {
		nodeClass.Spec.ImageFamily = lo.ToPtr(v1beta1.Ubuntu2404ImageFamily)
		nodeClass.Spec.FIPSMode = nil
		nodeClass.Spec.VNETSubnetID = lo.ToPtr(fmt.Sprintf(
			"/subscriptions/%s/resourceGroups/external-network/providers/Microsoft.Network/virtualNetworks/external-vnet/subnets/workers",
			subscriptionID,
		))
		nodeClass.Spec.Flex = &v1beta1.FlexNodeConfiguration{
			AgentPoolID: fmt.Sprintf("/subscriptions/%s/resourceGroups/test-cluster-resource-group/providers/Microsoft.ContainerService/managedClusters/%s/agentPools/%s",
				subscriptionID, clusterName, FlexPoolName),
			IdentityID: fmt.Sprintf("/subscriptions/%s/resourceGroups/identity-rg/providers/Microsoft.ManagedIdentity/userAssignedIdentities/flex-host",
				subscriptionID),
			IdentityClientID: FlexIdentityClientID,
			HostImageVersion: FlexHostImageVersion,
		}
	}
}

// ConfigureFlex installs Azure fakes only for tests that explicitly opt in.
// The caller remains responsible for setting options and NodeClass readiness.
func ConfigureFlex(ctx context.Context, env *Environment, nodeClass *v1beta1.AKSNodeClass) *flex.Provider {
	opts := options.FromContext(ctx)
	provider := flex.NewProvider(env.SubscriptionID, "test-cluster-resource-group", opts.ClusterName, env.region,
		&fake.VirtualNetworksAPI{Location: env.region},
		&fake.VirtualMachineImagesAPI{},
		&fake.UserAssignedIdentitiesAPI{ClientID: FlexIdentityClientID},
		env.AKSMachinesAPI, env.AKSAgentPoolsAPI,
	)
	env.AZClient.SetFlexProvider(provider)
	env.AKSDataStorage.AgentPools.Store(fake.MkAgentPoolID("test-cluster-resource-group", opts.ClusterName, FlexPoolName),
		armcontainerservice.AgentPool{
			Properties: &armcontainerservice.ManagedClusterAgentPoolProfileProperties{
				Type:                       lo.ToPtr(armcontainerservice.AgentPoolType("FlexNodes")),
				ProvisioningState:          lo.ToPtr("Succeeded"),
				CurrentOrchestratorVersion: nodeClass.Status.KubernetesVersion,
				MaxPods:                    lo.ToPtr(utils.GetMaxPods(nodeClass, opts.NetworkPlugin, opts.NetworkPluginMode)),
			},
		},
	)
	return provider
}
