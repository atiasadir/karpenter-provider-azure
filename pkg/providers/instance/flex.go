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

package instance

import (
	"context"
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/flex"
	"github.com/Azure/karpenter-provider-azure/pkg/utils"
	"github.com/samber/lo"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

func (p *DefaultVMProvider) PrepareFlex(ctx context.Context, nodeClass *v1beta1.AKSNodeClass, nodeClaim *karpv1.NodeClaim) (map[string]string, error) {
	if nodeClass.Spec.Flex == nil {
		return nil, nil
	}
	if nodeClass.Spec.Flex.GetLocation(p.location) != p.location {
		return nil, fmt.Errorf("flex location does not match the selected VM provider")
	}
	provider, err := p.azClient.Flex()
	if err != nil {
		return nil, err
	}
	image, err := provider.Resolve(ctx, nodeClass)
	if err != nil {
		return nil, err
	}
	if !lo.ContainsBy(nodeClass.Status.Images, func(candidate v1beta1.NodeImage) bool { return candidate.ID == image.ID }) {
		return nil, fmt.Errorf("flex host image readiness has not reconciled the current configuration")
	}
	return map[string]string{
		flex.PoolAnnotation:       nodeClass.Spec.Flex.AgentPoolID,
		flex.SpecHashAnnotation:   nodeClass.Hash(),
		flex.ProviderIDAnnotation: p.flexProviderID(ctx, GenerateResourceName(nodeClaim.Name)),
		flex.LocationAnnotation:   p.location,
	}, nil
}

func (p *DefaultVMProvider) FlexMetadata(ctx context.Context, vm *armcompute.VirtualMachine) (map[string]string, error) {
	poolName := lo.FromPtr(vm.Tags[flex.PoolTag])
	if poolName == "" {
		return nil, nil
	}
	if lo.FromPtr(vm.Name) == "" {
		return nil, fmt.Errorf("cannot recover Flex ownership without a VM name")
	}
	provider, err := p.azClient.Flex()
	if err != nil {
		return nil, err
	}
	id := provider.AgentPoolID(poolName)
	if err := provider.ValidatePoolID(id); err != nil {
		return nil, err
	}
	return map[string]string{
		flex.PoolAnnotation:       id,
		flex.SpecHashAnnotation:   lo.FromPtr(vm.Tags[flex.SpecHashTag]),
		flex.ProviderIDAnnotation: p.flexProviderID(ctx, *vm.Name),
		flex.LocationAnnotation:   strings.ToLower(lo.FromPtr(vm.Location)),
	}, nil
}

func (p *DefaultVMProvider) flexProviderID(ctx context.Context, resourceName string) string {
	return utils.VMResourceIDToProviderID(ctx, fmt.Sprintf(
		"/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/virtualMachines/%s",
		p.subscriptionID, p.resourceGroup, resourceName,
	))
}

func (p *DefaultVMProvider) cleanupFlexResources(ctx context.Context, poolID, resourceName string) error {
	provider, err := p.azClient.Flex()
	if err != nil {
		return err
	}
	if err := provider.ValidatePoolID(poolID); err != nil {
		return err
	}
	// Stop the agent before removing its enrollment. A restarting host must not
	// recreate a Machine that cleanup just removed.
	if err := p.cleanupAzureResources(ctx, resourceName, true); err != nil {
		return err
	}
	return provider.DeleteMachine(ctx, poolID, resourceName)
}

func validateFlexCreationOwnership(ctx context.Context, nodeClass *v1beta1.AKSNodeClass, nodeClaim *karpv1.NodeClaim) error {
	if err := flex.ValidateConfiguration(ctx, nodeClass); err != nil {
		return err
	}
	if nodeClass.Spec.Flex == nil {
		if nodeClaim.Annotations[flex.PoolAnnotation] != "" {
			return fmt.Errorf("cannot change an existing Flex NodeClaim to native provisioning")
		}
		return nil
	}
	if !lo.Contains(nodeClaim.Finalizers, flex.TerminationFinalizer) ||
		!strings.EqualFold(nodeClaim.Annotations[flex.PoolAnnotation], nodeClass.Spec.Flex.AgentPoolID) ||
		nodeClaim.Annotations[flex.SpecHashAnnotation] != nodeClass.Hash() ||
		nodeClaim.Annotations[flex.ProviderIDAnnotation] == "" {
		return fmt.Errorf("flex creation requires persisted, matching enrollment ownership")
	}
	return nil
}
