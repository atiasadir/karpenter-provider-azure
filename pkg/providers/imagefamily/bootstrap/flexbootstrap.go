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
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"

	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
)

const (
	flexAgentVersion     = "v0.3.0"
	flexBootstrapURL     = "https://raw.githubusercontent.com/Azure/AKSFlexNode/4741b41e4feaaca86d0dd53014fd4a141731ea34/scripts/bootstrap.sh"
	flexBootstrapSHA     = "0c00b140a2840dccc08b0bd47daa5f8145d1dfb6a20a2d4434fbea6dcca9523f"
	flexAzureLinuxRootFS = "ghcr.io/azure/agent-azlinux3:v20261031@sha256:fd9215a0419f1c4f69ee645dff71245cd1a3354909033eb553076abea94816f5"

	flexOverridesPath = "/etc/aks-flex-node/karpenter-overrides.json"
	flexKubeletPath   = "/etc/aks-flex-node/karpenter-kubelet.env"
	flexStartPath     = "/usr/local/sbin/karpenter-flex-bootstrap"
)

// Flex prepares a dedicated Ubuntu 24.04 or Azure Linux 3 host using the pinned Flex Node agent.
// The pool, managed identity permissions, and cross-VNet networking must already exist.
type Flex struct {
	AgentPoolID             string
	ManagedIdentityClientID string
	NodeName                string
	ProviderID              string
	Architecture            string
	ImageFamily             string
	Labels                  map[string]string
	Taints                  []corev1.Taint
	KubeletConfig           *KubeletConfiguration
}

type flexOverrides struct {
	Azure struct {
		TargetCluster struct {
			ResourceID string `json:"resourceId"`
		} `json:"targetCluster"`
		TargetAgentPoolName string `json:"targetAgentPoolName"`
		ManagedIdentity     struct {
			ClientID string `json:"clientId,omitempty"`
		} `json:"managedIdentity"`
	} `json:"azure"`
	Agent struct {
		NodeName                   string `json:"nodeName"`
		RequireMachineRegistration bool   `json:"requireMachineRegistration"`
		MachineClient              struct {
			Mode string `json:"mode"`
		} `json:"machineClient"`
	} `json:"agent"`
	Bootstrap struct {
		OCIImage             string          `json:"ociImage,omitempty"`
		AdditionalHostMounts []flexHostMount `json:"additionalHostMounts"`
	} `json:"bootstrap"`
	Node struct {
		Labels  map[string]string `json:"labels,omitempty"`
		Taints  []string          `json:"taints,omitempty"`
		MaxPods int32             `json:"maxPods,omitempty"`
		Kubelet struct {
			KubeReserved   map[string]string `json:"kubeReserved,omitempty"`
			SystemReserved map[string]string `json:"systemReserved,omitempty"`
		} `json:"kubelet"`
	} `json:"node"`
}

type flexHostMount struct {
	Source   string `json:"Source"`
	Target   string `json:"Target"`
	ReadOnly bool   `json:"ReadOnly"`
}

type flexCloudConfig struct {
	PackageUpdate bool            `json:"package_update"`
	Packages      []string        `json:"packages"`
	WriteFiles    []flexCloudFile `json:"write_files"`
	RunCmd        [][]string      `json:"runcmd"`
}

type flexCloudFile struct {
	Path        string `json:"path"`
	Owner       string `json:"owner"`
	Permissions string `json:"permissions"`
	Content     string `json:"content"`
}

func (f Flex) Script() (string, error) {
	pool, err := f.validate()
	if err != nil {
		return "", err
	}
	archiveSHA := "f602731496a1192ae302d80172097c563580cc591e1bcfd2a527cb6791eb28b4"
	if f.Architecture == "arm64" {
		archiveSHA = "ec9912a4fea7787ff10002ff8226aa2c61c4b8fa0eddd3e8febe8a38149efeb4"
	}

	overrides := flexOverrides{}
	overrides.Azure.TargetCluster.ResourceID = pool.Parent.String()
	overrides.Azure.TargetAgentPoolName = pool.Name
	overrides.Azure.ManagedIdentity.ClientID = f.ManagedIdentityClientID
	overrides.Agent.NodeName = f.NodeName
	overrides.Agent.RequireMachineRegistration = true
	overrides.Agent.MachineClient.Mode = "arm"
	if f.ImageFamily == v1beta1.AzureLinuxImageFamily {
		overrides.Bootstrap.OCIImage = flexAzureLinuxRootFS
	}
	// The pinned agent supports host mounts and reads KUBELET_EXTRA_ARGS from
	// /etc/default/kubelet; there is no Flex node.kubelet.providerID setting.
	overrides.Bootstrap.AdditionalHostMounts = []flexHostMount{{
		Source: flexKubeletPath, Target: "/etc/default/kubelet", ReadOnly: true,
	}}
	overrides.Node.Labels = f.Labels
	if f.KubeletConfig != nil {
		overrides.Node.MaxPods = f.KubeletConfig.MaxPods
		overrides.Node.Kubelet.KubeReserved = f.KubeletConfig.KubeReserved
		overrides.Node.Kubelet.SystemReserved = f.KubeletConfig.SystemReserved
	}
	for _, taint := range f.Taints {
		overrides.Node.Taints = append(overrides.Node.Taints, taint.ToString())
	}
	overrideJSON, err := json.Marshal(overrides)
	if err != nil {
		return "", fmt.Errorf("marshaling Flex bootstrap overrides: %w", err)
	}

	startScript := fmt.Sprintf(`#!/bin/bash
set -euo pipefail
. /etc/os-release
case %q:"$ID:$VERSION_ID" in
    Ubuntu2404:ubuntu:24.04|AzureLinux:azurelinux:3.*|AzureLinux:azlinux:3.*) ;;
    *) echo "Flex host OS does not match the selected image family" >&2; exit 1 ;;
esac
case "$(uname -m)" in
    x86_64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) echo "Unsupported Flex host architecture" >&2; exit 1 ;;
esac
if [[ "$arch" != %q ]]; then
    echo "Flex host architecture does not match the selected instance type" >&2
    exit 1
fi
install -d -m 0700 /run/karpenter-flex-bootstrap
script=/run/karpenter-flex-bootstrap/bootstrap.sh
trap 'rm -f "$script"' EXIT
curl --fail --silent --show-error --location --retry 5 --connect-timeout 10 --max-time 120 \
    --output "$script" %q
printf '%%s  %%s\n' %q "$script" | sha256sum --check --status
chmod 0700 "$script"
bash -n "$script"
overrides=%q
args=(
    --auth msi
    --fetch-bootstrap-data
    --cluster-resource-id "$(jq -er '.azure.targetCluster.resourceId' "$overrides")"
    --agent-pool-name "$(jq -er '.azure.targetAgentPoolName' "$overrides")"
    --agent-version %q
    --agent-sha256 %q
    --config-overrides "$(cat "$overrides")"
)
client_id=$(jq -r '.azure.managedIdentity.clientId // ""' "$overrides")
if [[ -n "$client_id" ]]; then
    args+=(--msi-client-id "$client_id")
fi
bash "$script" "${args[@]}"
`, f.ImageFamily, f.Architecture, flexBootstrapURL, flexBootstrapSHA, flexOverridesPath, flexAgentVersion, archiveSHA)

	config := flexCloudConfig{
		PackageUpdate: true,
		Packages:      []string{"ca-certificates", "curl", "jq"},
		WriteFiles: []flexCloudFile{
			{Path: flexOverridesPath, Owner: "root:root", Permissions: "0600", Content: string(overrideJSON)},
			{Path: flexKubeletPath, Owner: "root:root", Permissions: "0600", Content: "KUBELET_EXTRA_ARGS=" + strconv.Quote("--provider-id="+f.ProviderID) + "\n"},
			{Path: flexStartPath, Owner: "root:root", Permissions: "0700", Content: startScript},
		},
		RunCmd: [][]string{{"bash", flexStartPath}},
	}
	data, err := yaml.Marshal(config)
	if err != nil {
		return "", fmt.Errorf("marshaling Flex cloud-init: %w", err)
	}
	customData := append([]byte("#cloud-config\n"), data...)
	if len(customData) > 65535 {
		return "", fmt.Errorf("flex custom data exceeds the Azure limit of 65535 bytes")
	}
	return base64.StdEncoding.EncodeToString(customData), nil
}

func (f Flex) validate() (*arm.ResourceID, error) {
	if !slices.Contains([]string{v1beta1.Ubuntu2404ImageFamily, v1beta1.AzureLinuxImageFamily}, f.ImageFamily) {
		return nil, fmt.Errorf("unsupported Flex image family %q", f.ImageFamily)
	}
	pool, err := arm.ParseResourceID(f.AgentPoolID)
	if err != nil || pool.Parent == nil || !strings.EqualFold(pool.ResourceType.String(), "Microsoft.ContainerService/managedClusters/agentPools") ||
		strings.ContainsFunc(f.AgentPoolID, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return nil, fmt.Errorf("flex agentPoolID must identify an AKS agent pool")
	}
	if f.Architecture != "amd64" && f.Architecture != "arm64" {
		return nil, fmt.Errorf("unsupported Flex architecture %q", f.Architecture)
	}
	if err := f.validateNodeIdentity(); err != nil {
		return nil, err
	}
	if err := f.validateLabelsAndTaints(); err != nil {
		return nil, err
	}
	return pool, nil
}

func (f Flex) validateNodeIdentity() error {
	if errs := validation.IsDNS1123Subdomain(f.NodeName); len(errs) != 0 || len(f.NodeName) > 64 {
		return fmt.Errorf("flex node name must be a lowercase DNS name of at most 64 characters")
	}
	if !strings.HasPrefix(f.ProviderID, "azure:///") || strings.ContainsFunc(f.ProviderID, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) {
		return fmt.Errorf("flex providerID must be an Azure VM provider ID without whitespace")
	}
	vm, err := arm.ParseResourceID(strings.TrimPrefix(f.ProviderID, "azure://"))
	if err != nil || !strings.EqualFold(vm.ResourceType.String(), "Microsoft.Compute/virtualMachines") || vm.Name != f.NodeName {
		return fmt.Errorf("flex providerID must identify the backing VM with the same name as the node")
	}
	return nil
}

func (f Flex) validateLabelsAndTaints() error {
	for key, value := range f.Labels {
		if len(validation.IsQualifiedName(key)) != 0 || len(validation.IsValidLabelValue(value)) != 0 {
			return fmt.Errorf("invalid Flex node label %q", key)
		}
	}
	for _, taint := range f.Taints {
		if len(validation.IsQualifiedName(taint.Key)) != 0 || len(validation.IsValidLabelValue(taint.Value)) != 0 {
			return fmt.Errorf("invalid Flex node taint %q", taint.Key)
		}
		switch taint.Effect {
		case corev1.TaintEffectNoSchedule, corev1.TaintEffectPreferNoSchedule, corev1.TaintEffectNoExecute:
		default:
			return fmt.Errorf("invalid Flex node taint effect %q", taint.Effect)
		}
	}
	return nil
}
