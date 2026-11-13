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

package cloudprovider

import (
	"context"
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/flex"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/instance"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/instancetype"
	nodeclaimutils "github.com/Azure/karpenter-provider-azure/pkg/utils/nodeclaim"
	corecloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
)

type RegionalProviders struct {
	InstanceTypes instancetype.Provider
	Instances     instance.VMProvider
}

// Each configured region has its own SKU, pricing, quota and instance state.
// The home provider continues to own all native and AKS Machine API nodes.
func (c *CloudProvider) WithFlexLocations(home string, locations map[string]RegionalProviders) *CloudProvider {
	c.location = home
	c.flexLocations = map[string]*CloudProvider{home: c}
	for location, providers := range locations {
		if location == home {
			continue
		}
		regional := New(providers.InstanceTypes, providers.Instances, c.aksMachineInstanceProvider,
			c.recorder, c.kubeClient, c.imageProvider, c.instanceTypeStore, c.legacyRepairPolicies)
		regional.location = location
		regional.flexLocations = c.flexLocations
		c.flexLocations[location] = regional
	}
	return c
}

func (c *CloudProvider) instanceTypesForVM(ctx context.Context, nodePool *karpv1.NodePool, vm *armcompute.VirtualMachine) ([]*corecloudprovider.InstanceType, error) {
	provider, err := c.forVM(vm)
	if err != nil {
		return nil, err
	}
	nodeClass, err := c.resolveNodeClassFromNodePool(ctx, nodePool)
	if err != nil {
		return nil, err
	}
	return provider.instanceTypeProvider.List(ctx, nodeClass)
}

func (c *CloudProvider) forVM(vm *armcompute.VirtualMachine) (*CloudProvider, error) {
	if lo.FromPtr(vm.Tags[flex.PoolTag]) == "" || len(c.flexLocations) == 0 {
		return c, nil
	}
	provider := c.flexLocations[strings.ToLower(lo.FromPtr(vm.Location))]
	if provider == nil {
		return nil, fmt.Errorf("flex VM location %q has no configured regional scheduling provider", lo.FromPtr(vm.Location))
	}
	return provider, nil
}

func (c *CloudProvider) forNodeClass(nodeClass *v1beta1.AKSNodeClass) (*CloudProvider, error) {
	location := nodeClass.Spec.Flex.GetLocation(c.location)
	if location == c.location {
		return c, nil
	}
	if regional, ok := c.flexLocations[location]; ok {
		return regional, nil
	}
	return nil, fmt.Errorf("flex location %q has no configured regional provider", location)
}

func (c *CloudProvider) deleteFlex(ctx context.Context, nodeClaim *karpv1.NodeClaim) error {
	providerID := lo.CoalesceOrEmpty(nodeClaim.Status.ProviderID, nodeClaim.Annotations[flex.ProviderIDAnnotation])
	name, err := nodeclaimutils.GetVMName(providerID)
	if err != nil {
		return err
	}
	location, err := c.flexDeletionLocation(ctx, nodeClaim, name)
	if err != nil {
		return err
	}
	provider := c
	if regional := c.flexLocations[location]; regional != nil {
		provider = regional
	} else if location != c.location && len(c.flexLocations) != 0 {
		log.FromContext(ctx).Info("cleaning up Flex VM without a configured scheduling provider; regional quota adjustment is skipped", "location", location)
	}
	return provider.vmInstanceProvider.Delete(ctx, nodeClaim, name)
}

func (c *CloudProvider) flexDeletionLocation(ctx context.Context, nodeClaim *karpv1.NodeClaim, name string) (string, error) {
	recorded := nodeClaim.Annotations[flex.LocationAnnotation]
	vm, err := c.vmInstanceProvider.Get(ctx, name)
	if corecloudprovider.IsNodeClaimNotFoundError(err) {
		return recorded, nil
	}
	if err != nil {
		return "", err
	}
	actual := strings.ToLower(lo.FromPtr(vm.Location))
	if recorded != "" && actual != recorded {
		return "", fmt.Errorf("flex VM location differs from its creation-time ownership")
	}
	return actual, nil
}

// Persist intent before creating anything in Azure. The additional finalizer
// covers failed launches, for which core can have no status.providerID to delete.
func (c *CloudProvider) recordFlexOwnership(ctx context.Context, nodeClass *v1beta1.AKSNodeClass, nodeClaim *karpv1.NodeClaim) error {
	if nodeClass.Spec.Flex == nil {
		if nodeClaim.Annotations[flex.PoolAnnotation] != "" {
			return fmt.Errorf("cannot change an existing Flex NodeClaim to native provisioning")
		}
		return nil
	}
	annotations, err := c.vmInstanceProvider.PrepareFlex(ctx, nodeClass, nodeClaim)
	if err != nil {
		return err
	}
	if err := validateFlexOwnership(nodeClaim.Annotations, annotations); err != nil {
		return err
	}
	updated := nodeClaim.DeepCopy()
	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}
	for key, value := range annotations {
		if updated.Annotations[key] == "" {
			updated.Annotations[key] = value
		}
	}
	controllerutil.AddFinalizer(updated, flex.TerminationFinalizer)
	if equality.Semantic.DeepEqual(nodeClaim, updated) {
		return nil
	}

	if err := c.kubeClient.Patch(ctx, updated, client.MergeFromWithOptions(nodeClaim, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("persisting Flex enrollment ownership before provisioning: %w", err)
	}
	nodeClaim.ObjectMeta = updated.ObjectMeta
	return nil
}

func validateFlexOwnership(existing, expected map[string]string) error {
	for key, value := range expected {
		if previous := existing[key]; previous != "" && !strings.EqualFold(previous, value) {
			return fmt.Errorf("flex NodeClaim creation-time ownership differs from the current NodeClass")
		}
	}
	return nil
}

func (c *CloudProvider) restoreFlexOwnership(ctx context.Context, vm *armcompute.VirtualMachine, nodeClaim *karpv1.NodeClaim) error {
	if lo.FromPtr(vm.Tags[flex.PoolTag]) == "" {
		return nil
	}
	metadata, err := c.vmInstanceProvider.FlexMetadata(ctx, vm)
	if err != nil {
		return err
	}
	nodeClaim.Annotations = lo.Assign(nodeClaim.Annotations, metadata)
	nodeClaim.Status.ProviderID = metadata[flex.ProviderIDAnnotation]
	return nil
}
