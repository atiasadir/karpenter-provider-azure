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

package status_test

import (
	"context"
	"fmt"
	"testing"

	"sigs.k8s.io/karpenter/pkg/test/v1alpha1"

	coreoptions "sigs.k8s.io/karpenter/pkg/operator/options"
	coretest "sigs.k8s.io/karpenter/pkg/test"

	"github.com/Azure/karpenter-provider-azure/pkg/apis"
	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/consts"
	"github.com/Azure/karpenter-provider-azure/pkg/controllers/nodeclass/status"
	"github.com/Azure/karpenter-provider-azure/pkg/fake"
	"github.com/Azure/karpenter-provider-azure/pkg/operator/options"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/flex"
	"github.com/Azure/karpenter-provider-azure/pkg/test"
	"github.com/Azure/karpenter-provider-azure/pkg/test/expectations"
	"github.com/blang/semver/v4"
	"github.com/samber/lo"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

var ctx context.Context
var env *coretest.Environment
var azureEnv *test.Environment
var nodeClass *v1beta1.AKSNodeClass
var controller *status.Controller
var testOptions *options.Options

var (
	testK8sVersion string
	oldK8sVersion  string
)

func TestAKSNodeClassStatusController(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "AKSNodeClass_Status")
}

var _ = BeforeSuite(func() {
	env = coretest.NewEnvironment(coretest.WithCRDs(apis.CRDs...), coretest.WithCRDs(v1alpha1.CRDs...), coretest.WithFieldIndexers(test.AKSNodeClassFieldIndexer(ctx)))
	ctx = coreoptions.ToContext(ctx, coretest.Options())
	testOptions = test.Options()
	ctx = options.ToContext(ctx, testOptions)
	azureEnv = test.NewEnvironment(ctx, env)

	controller = status.NewController(env.Client, azureEnv.SubscriptionID, fake.Region, azureEnv.KubernetesVersionProvider, azureEnv.ImageProvider, env.KubernetesInterface, env.KubernetesInterface, azureEnv.DynamicInterface, azureEnv.SubnetsAPI, azureEnv.DiskEncryptionSetsAPI, testOptions.ParsedDiskEncryptionSetID, options.FromContext(ctx).NetworkPolicy, options.FromContext(ctx).NetworkPlugin,
		azureEnv.CapacityReservationGroupsAPI, azureEnv.CapacityReservationsAPI, azureEnv.InstanceTypesProvider, azureEnv.UnavailableOfferingsCache, testOptions.SystemNamespace, azureEnv.AZClient.Flex)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = BeforeEach(func() {
	ctx = coreoptions.ToContext(ctx, coretest.Options())
	ctx = options.ToContext(ctx, testOptions)
	nodeClass = test.AKSNodeClass(test.WithVersionBasedDefaults(env.Version))
	azureEnv.Reset(ctx)

	testK8sVersion = lo.Must(semver.ParseTolerant(lo.Must(env.KubernetesInterface.Discovery().ServerVersion()).String())).String()
	semverTestK8sVersion := lo.Must(semver.ParseTolerant(testK8sVersion))
	semverTestK8sVersion.Minor = semverTestK8sVersion.Minor - 1
	oldK8sVersion = semverTestK8sVersion.String()
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
	expectations.ExpectCleanUp(ctx, env.Client)
})

var _ = Describe("Flex readiness", func() {
	var flexCtx context.Context
	var reconciler *status.FlexReconciler

	BeforeEach(func() {
		flexCtx = options.ToContext(ctx, test.Options(test.OptionsFields{
			EnableFlexNodes: lo.ToPtr(true),
			NetworkPlugin:   lo.ToPtr(consts.NetworkPluginNone),
		}))
		test.WithFlex(azureEnv.SubscriptionID, options.FromContext(flexCtx).ClusterName)(nodeClass)
		test.ApplyDefaultStatus(nodeClass, env, false)
		test.ConfigureFlex(flexCtx, azureEnv, nodeClass)
		reconciler = status.NewFlexReconciler(azureEnv.AZClient.Flex)
	})

	It("should resolve the host image and permit an explicit external subnet", func() {
		_, err := reconciler.Reconcile(flexCtx, nodeClass)
		Expect(err).ToNot(HaveOccurred())
		_, err = status.NewSubnetReconciler(azureEnv.SubnetsAPI).Reconcile(flexCtx, nodeClass)
		Expect(err).ToNot(HaveOccurred())
		Expect(nodeClass.StatusConditions().Get(v1beta1.ConditionTypeFlexReady).IsTrue()).To(BeTrue())
		Expect(nodeClass.StatusConditions().Get(v1beta1.ConditionTypeSubnetsReady).IsTrue()).To(BeTrue())
		Expect(nodeClass.Status.Images).To(HaveLen(1))
		Expect(nodeClass.Status.Images[0].ID).To(Equal("Canonical:ubuntu-24_04-lts:server:" + test.FlexHostImageVersion))
	})

	It("should retain the native VNet boundary when the feature is disabled", func() {
		disabled := options.ToContext(ctx, test.Options())
		_, err := reconciler.Reconcile(disabled, nodeClass)
		Expect(err).ToNot(HaveOccurred())
		Expect(nodeClass.StatusConditions().Get(v1beta1.ConditionTypeFlexReady).Reason).To(Equal("FlexNodesDisabled"))
		_, err = status.NewSubnetReconciler(azureEnv.SubnetsAPI).Reconcile(disabled, nodeClass)
		Expect(err).ToNot(HaveOccurred())
		Expect(nodeClass.StatusConditions().Get(v1beta1.ConditionTypeSubnetsReady).IsFalse()).To(BeTrue())
	})

	It("should not create or call Flex clients for a native NodeClass", func() {
		nodeClass.Spec.Flex = nil
		reconciler = status.NewFlexReconciler(func() (*flex.Provider, error) {
			Fail("native readiness must not initialize Flex clients")
			return nil, nil
		})
		_, err := reconciler.Reconcile(flexCtx, nodeClass)
		Expect(err).ToNot(HaveOccurred())
	})

	It("should reject managed CNI even when Flex is enabled", func() {
		managedCtx := options.ToContext(ctx, test.Options(test.OptionsFields{EnableFlexNodes: lo.ToPtr(true)}))
		_, err := reconciler.Reconcile(managedCtx, nodeClass)
		Expect(err).ToNot(HaveOccurred())
		Expect(nodeClass.StatusConditions().Get(v1beta1.ConditionTypeFlexReady).Reason).To(Equal("FlexRequiresNoCNICluster"))
		Expect(nodeClass.StatusConditions().Get(v1beta1.ConditionTypeImagesReady).IsFalse()).To(BeTrue())
	})

	It("should retain image history but revoke readiness when the provider fails", func() {
		_, err := reconciler.Reconcile(flexCtx, nodeClass)
		Expect(err).ToNot(HaveOccurred())
		images := nodeClass.DeepCopy().Status.Images
		reconciler = status.NewFlexReconciler(func() (*flex.Provider, error) {
			return nil, fmt.Errorf("simulated Azure client failure")
		})
		_, err = reconciler.Reconcile(flexCtx, nodeClass)
		Expect(err).To(HaveOccurred())
		Expect(nodeClass.Status.Images).To(Equal(images))
		Expect(nodeClass.StatusConditions().Get(v1beta1.ConditionTypeFlexReady).IsFalse()).To(BeTrue())
		Expect(nodeClass.StatusConditions().Get(v1beta1.ConditionTypeImagesReady).IsFalse()).To(BeTrue())
	})
})
