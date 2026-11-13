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

package garbagecollection

import (
	"context"
	"fmt"
	"time"

	"github.com/awslabs/operatorpkg/reasonable"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/Azure/karpenter-provider-azure/pkg/providers/flex"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/instance"
	nodeclaimutils "github.com/Azure/karpenter-provider-azure/pkg/utils/nodeclaim"
)

// Flex owns cleanup intent even when launch failed before core recorded a provider ID.
type Flex struct {
	kubeClient client.Client
	instances  instance.VMProvider
}

func NewFlex(kubeClient client.Client, instances instance.VMProvider) *Flex {
	return &Flex{kubeClient: kubeClient, instances: instances}
}

func (c *Flex) Reconcile(ctx context.Context, nodeClaim *karpv1.NodeClaim) (reconcile.Result, error) {
	if nodeClaim.DeletionTimestamp.IsZero() || !controllerutil.ContainsFinalizer(nodeClaim, flex.TerminationFinalizer) {
		return reconcile.Result{}, nil
	}
	vmName, err := flexCleanupVMName(nodeClaim)
	if err != nil {
		return reconcile.Result{}, err
	}
	node, err := c.nodeForCleanup(ctx, nodeClaim, vmName)
	if err != nil {
		return reconcile.Result{}, err
	}
	// Core owns draining and termination of registered Nodes. Its normal
	// cloud-provider Delete call also removes the Flex enrollment.
	if node != nil && nodeClaim.StatusConditions().Get(karpv1.ConditionTypeRegistered).IsTrue() {
		return reconcile.Result{RequeueAfter: 5 * time.Second}, nil
	}
	deleteErr := c.instances.Delete(ctx, nodeClaim, vmName)
	if cloudprovider.IgnoreNodeClaimNotFoundError(deleteErr) != nil {
		return reconcile.Result{}, deleteErr
	}
	if !cloudprovider.IsNodeClaimNotFoundError(deleteErr) {
		return reconcile.Result{RequeueAfter: 5 * time.Second}, nil
	}
	return c.finalize(ctx, nodeClaim, node)
}

func flexCleanupVMName(nodeClaim *karpv1.NodeClaim) (string, error) {
	providerID := nodeClaim.Annotations[flex.ProviderIDAnnotation]
	if providerID == "" || nodeClaim.Annotations[flex.PoolAnnotation] == "" {
		return "", fmt.Errorf("flex cleanup ownership is incomplete")
	}
	vmName, err := nodeclaimutils.GetVMName(providerID)
	if err != nil || vmName != instance.GenerateResourceName(nodeClaim.Name) {
		return "", fmt.Errorf("flex cleanup provider ID does not match the NodeClaim's VM")
	}
	return vmName, nil
}

func (c *Flex) nodeForCleanup(ctx context.Context, nodeClaim *karpv1.NodeClaim, vmName string) (*corev1.Node, error) {
	node := &corev1.Node{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: vmName}, node); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if node.Spec.ProviderID != nodeClaim.Annotations[flex.ProviderIDAnnotation] {
		return nil, fmt.Errorf("refusing to clean up a Node with a different provider ID")
	}
	return node, nil
}

func (c *Flex) finalize(ctx context.Context, nodeClaim *karpv1.NodeClaim, node *corev1.Node) (reconcile.Result, error) {
	if node != nil {
		if err := c.kubeClient.Delete(ctx, node); client.IgnoreNotFound(err) != nil {
			return reconcile.Result{}, err
		}
		return reconcile.Result{RequeueAfter: 5 * time.Second}, nil
	}
	updated := nodeClaim.DeepCopy()
	controllerutil.RemoveFinalizer(updated, flex.TerminationFinalizer)
	if err := c.kubeClient.Patch(ctx, updated, client.MergeFromWithOptions(nodeClaim, client.MergeFromWithOptimisticLock{})); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	nodeClaim.ObjectMeta = updated.ObjectMeta
	return reconcile.Result{}, nil
}

func (c *Flex) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("nodeclaim.flex.cleanup").
		For(&karpv1.NodeClaim{}, builder.WithPredicates(
			nodeclaimutils.UsingAKSNodeClassPredicate(),
			predicate.NewPredicateFuncs(func(object client.Object) bool {
				return controllerutil.ContainsFinalizer(object, flex.TerminationFinalizer)
			}),
		)).
		WithOptions(controller.Options{
			RateLimiter:             reasonable.RateLimiter(),
			MaxConcurrentReconciles: 10,
		}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}
