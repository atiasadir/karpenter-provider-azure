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
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/flex"
)

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
