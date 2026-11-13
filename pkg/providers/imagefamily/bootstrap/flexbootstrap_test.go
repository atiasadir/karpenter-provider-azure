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

package bootstrap

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

func testFlexBootstrap() Flex {
	return Flex{
		AgentPoolID:  "/subscriptions/test-sub/resourceGroups/cluster-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster/agentPools/flex",
		NodeName:     "aks-test-worker",
		ProviderID:   "azure:///subscriptions/test-sub/resourceGroups/node-rg/providers/Microsoft.Compute/virtualMachines/aks-test-worker",
		Architecture: "amd64",
		Labels:       map[string]string{"karpenter.sh/nodepool": "external"},
		Taints: []corev1.Taint{
			{Key: "karpenter.sh/unregistered", Effect: corev1.TaintEffectNoExecute},
			{Key: "example.com/worker", Value: "external", Effect: corev1.TaintEffectNoSchedule},
		},
		KubeletConfig: &KubeletConfiguration{
			MaxPods:        110,
			KubeReserved:   map[string]string{"cpu": "100m", "memory": "1Gi"},
			SystemReserved: map[string]string{"memory": "100Mi"},
		},
	}
}

func TestFlexScript(t *testing.T) {
	for _, architecture := range []string{"amd64", "arm64"} {
		t.Run(architecture, func(t *testing.T) {
			g := NewWithT(t)
			input := testFlexBootstrap()
			input.Architecture = architecture
			input.ManagedIdentityClientID = "7fda40b0-dd9a-4464-b782-11d1bdd5bcfa"

			encoded, err := input.Script()
			g.Expect(err).ToNot(HaveOccurred())
			data, err := base64.StdEncoding.DecodeString(encoded)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(string(data)).To(HavePrefix("#cloud-config\n"))

			var config flexCloudConfig
			g.Expect(yaml.Unmarshal(data, &config)).To(Succeed())
			g.Expect(config.RunCmd).To(Equal([][]string{{"bash", flexStartPath}}))
			g.Expect(config.Packages).To(ContainElements("curl", "jq"))
			files := map[string]flexCloudFile{}
			for _, file := range config.WriteFiles {
				g.Expect(file.Owner).To(Equal("root:root"))
				files[file.Path] = file
			}
			g.Expect(files[flexOverridesPath].Permissions).To(Equal("0600"))
			g.Expect(files[flexKubeletPath].Permissions).To(Equal("0600"))
			g.Expect(files[flexStartPath].Permissions).To(Equal("0700"))

			var overrides flexOverrides
			g.Expect(json.Unmarshal([]byte(files[flexOverridesPath].Content), &overrides)).To(Succeed())
			g.Expect(overrides.Azure.TargetCluster.ResourceID).To(Equal(strings.TrimSuffix(input.AgentPoolID, "/agentPools/flex")))
			g.Expect(overrides.Azure.TargetAgentPoolName).To(Equal("flex"))
			g.Expect(overrides.Azure.ManagedIdentity.ClientID).To(Equal(input.ManagedIdentityClientID))
			g.Expect(overrides.Agent.NodeName).To(Equal(input.NodeName))
			g.Expect(overrides.Agent.RequireMachineRegistration).To(BeTrue())
			g.Expect(overrides.Agent.MachineClient.Mode).To(Equal("arm"))
			g.Expect(overrides.Node.Labels).To(Equal(input.Labels))
			g.Expect(overrides.Node.MaxPods).To(Equal(input.KubeletConfig.MaxPods))
			g.Expect(overrides.Node.Kubelet.KubeReserved).To(Equal(input.KubeletConfig.KubeReserved))
			g.Expect(overrides.Node.Kubelet.SystemReserved).To(Equal(input.KubeletConfig.SystemReserved))
			g.Expect(overrides.Node.Taints).To(ConsistOf(
				"karpenter.sh/unregistered:NoExecute",
				"example.com/worker=external:NoSchedule",
			))
			g.Expect(overrides.Bootstrap.AdditionalHostMounts).To(Equal([]flexHostMount{{
				Source: flexKubeletPath, Target: "/etc/default/kubelet", ReadOnly: true,
			}}))
			kubeletArgs, err := strconv.Unquote(strings.TrimSpace(strings.TrimPrefix(files[flexKubeletPath].Content, "KUBELET_EXTRA_ARGS=")))
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(kubeletArgs).To(Equal("--provider-id=" + input.ProviderID))

			script := files[flexStartPath].Content
			g.Expect(script).To(ContainSubstring("--fetch-bootstrap-data"))
			g.Expect(script).To(ContainSubstring("--agent-sha256"))
			g.Expect(script).To(ContainSubstring("sha256sum --check --status"))
			g.Expect(script).To(ContainSubstring(`bash "$script" "${args[@]}"`))
			g.Expect(strings.Index(script, "sha256sum --check --status")).To(BeNumerically("<", strings.Index(script, `bash "$script" "${args[@]}"`)))
			g.Expect(script).ToNot(ContainSubstring("set -x"))
			g.Expect(script).To(ContainSubstring("--msi-client-id"))
			if architecture == "amd64" {
				g.Expect(script).To(ContainSubstring("f602731496a1192ae302d80172097c563580cc591e1bcfd2a527cb6791eb28b4"))
			} else {
				g.Expect(script).To(ContainSubstring("ec9912a4fea7787ff10002ff8226aa2c61c4b8fa0eddd3e8febe8a38149efeb4"))
			}
			command := exec.Command("bash", "-n")
			command.Stdin = strings.NewReader(script)
			output, err := command.CombinedOutput()
			g.Expect(err).ToNot(HaveOccurred(), string(output))
		})
	}
}

func TestFlexScriptRejectsInvalidInputs(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Flex)
	}{
		{"wrong pool resource type", func(f *Flex) { f.AgentPoolID = strings.TrimPrefix(f.ProviderID, "azure://") }},
		{"pool control characters", func(f *Flex) { f.AgentPoolID += "\n" }},
		{"unsupported architecture", func(f *Flex) { f.Architecture = "386" }},
		{"invalid node name", func(f *Flex) { f.NodeName = "worker;echo" }},
		{"different backing VM", func(f *Flex) { f.ProviderID += "-other" }},
		{"non-Azure identity", func(f *Flex) { f.ProviderID = "external:///worker" }},
		{"provider identity whitespace", func(f *Flex) { f.ProviderID += "\n" }},
		{"invalid label", func(f *Flex) { f.Labels["example.com/value"] = "$(touch /tmp/invalid)" }},
		{"invalid taint effect", func(f *Flex) { f.Taints[0].Effect = "Unknown" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			input := testFlexBootstrap()
			tc.change(&input)
			script, err := input.Script()
			g.Expect(err).To(HaveOccurred())
			g.Expect(script).To(BeEmpty())
		})
	}
}

func TestFlexScriptRejectsOversizedCustomData(t *testing.T) {
	g := NewWithT(t)
	input := testFlexBootstrap()
	for i := range 1200 {
		input.Labels[fmt.Sprintf("example.com/label-%d", i)] = strings.Repeat("x", 63)
	}
	script, err := input.Script()
	g.Expect(err).To(MatchError(ContainSubstring("65535 bytes")))
	g.Expect(script).To(BeEmpty())
}
