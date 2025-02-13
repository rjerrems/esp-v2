// Copyright 2023 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package filtergen

import (
	"fmt"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/esp-v2/src/go/options"
	commonpb "github.com/GoogleCloudPlatform/esp-v2/src/go/proto/api/envoy/v12/http/common"
	"github.com/GoogleCloudPlatform/esp-v2/src/go/util"
	listenerpb "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	hcmpb "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"github.com/golang/glog"
	servicepb "google.golang.org/genproto/googleapis/api/serviceconfig"
	apipb "google.golang.org/genproto/protobuf/api"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

var (
	// skipServiceControlSelectors are selectors that should skip SC checks by default.
	skipServiceControlSelectors = map[string]bool{
		"grpc.health.v1.Health.Check": true,
		"grpc.health.v1.Health.Watch": true,
	}

	corsOperationDelimiter = fmt.Sprintf(".%s_CORS_", util.AutogeneratedOperationPrefix)
)

// MethodToSelector gets the operation name from OP API and Method.
func MethodToSelector(api *apipb.Api, method *apipb.Method) string {
	return fmt.Sprintf("%s.%s", api.GetName(), method.GetName())
}

// MethodToCORSSelector gets the corresponding autogenerated CORS selector for
// an OP API and Method.
func MethodToCORSSelector(api *apipb.Api, method *apipb.Method) string {
	return api.GetName() + corsOperationDelimiter + method.GetName()
}

// CORSSelectorToSelector reverses MethodToCORSSelector to extract the original
// selector name from a CORS selector.
func CORSSelectorToSelector(corsSelector string) (string, error) {
	if !strings.Contains(corsSelector, corsOperationDelimiter) {
		// Not a CORS selector, no op.
		return "", nil
	}

	split := strings.Split(corsSelector, corsOperationDelimiter)
	if len(split) != 2 {
		return "", fmt.Errorf("fail to parse CORS selector %q, got split %+q does not have exactly 2 elements", corsSelector, split)
	}

	originalSelector := fmt.Sprintf("%s.%s", split[0], split[1])
	return originalSelector, nil
}

func ParseDepErrorBehavior(stringVal string) (commonpb.DependencyErrorBehavior, error) {
	depErrorBehaviorInt, ok := commonpb.DependencyErrorBehavior_value[stringVal]
	if !ok {
		keys := make([]string, 0, len(commonpb.DependencyErrorBehavior_value))
		for k := range commonpb.DependencyErrorBehavior_value {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return commonpb.DependencyErrorBehavior_UNSPECIFIED, fmt.Errorf("unknown value for DependencyErrorBehavior (%v), accepted values are: %+q", stringVal, keys)
	}
	return commonpb.DependencyErrorBehavior(depErrorBehaviorInt), nil
}

func FilterConfigToHTTPFilter(filter proto.Message, name string) (*hcmpb.HttpFilter, error) {
	a, err := anypb.New(filter)
	if err != nil {
		return nil, fmt.Errorf("fail to marshal filter config to Any for filter %q: %v", name, err)
	}
	return &hcmpb.HttpFilter{
		Name: name,
		ConfigType: &hcmpb.HttpFilter_TypedConfig{
			TypedConfig: a,
		},
	}, nil
}

func FilterConfigToNetworkFilter(filter proto.Message, name string) (*listenerpb.Filter, error) {
	a, err := anypb.New(filter)
	if err != nil {
		return nil, fmt.Errorf("fail to marshal filter config to Any for filter %q: %v", name, err)
	}
	return &listenerpb.Filter{
		Name: name,
		ConfigType: &listenerpb.Filter_TypedConfig{
			TypedConfig: a,
		},
	}, nil
}

// IsAutoGenCORSRequiredForOPConfig returns true if CORS methods should be
// autogenerated.
//
// Replaces ServiceInfo::processEndpoints.
func IsAutoGenCORSRequiredForOPConfig(serviceConfig *servicepb.Service, opts options.ConfigGeneratorOptions) bool {
	for _, endpoint := range serviceConfig.GetEndpoints() {
		if endpoint.GetName() == serviceConfig.GetName() && endpoint.GetAllowCors() {
			return true
		}
	}
	return false
}

// IsGRPCSupportRequiredForOPConfig determines if any customer configuration requires gRPC features.
// If not, the generated configuration can be trimmed.
//
// Replaces ServiceInfo::buildBackendFromAddress and ServiceInfo::addBackendRuleToMethod.
func IsGRPCSupportRequiredForOPConfig(serviceConfig *servicepb.Service, opts options.ConfigGeneratorOptions) (bool, error) {
	if isLocalBackendGRPC, err := util.IsBackendGRPC(opts.BackendAddress); err != nil {
		return false, fmt.Errorf("fail to check local backend address: %v", err)
	} else if isLocalBackendGRPC {
		return true, nil
	}

	if opts.EnableBackendAddressOverride {
		return false, nil
	}

	for _, rule := range serviceConfig.GetBackend().GetRules() {
		if util.ShouldSkipOPDiscoveryAPI(rule.GetSelector(), opts.AllowDiscoveryAPIs) {
			glog.Warningf("Skip backend rule %q because discovery API is not supported.", rule.GetSelector())
			continue
		}

		if rule.GetAddress() == "" {
			glog.Infof("Skip backend rule %q because it does not have dynamic routing address.", rule.GetSelector())
			return false, nil
		}

		if isRemoteBackendGRPC, err := util.IsBackendGRPC(rule.GetAddress()); err != nil {
			return false, fmt.Errorf("fail to check remote backend address for selector %q: %v", rule.GetSelector(), err)
		} else if isRemoteBackendGRPC {
			return true, nil
		}
	}

	return false, nil
}

// GetAPINamesSetFromOPConfig returns a map of all API names (gRPC service names)
// to generate configs for.
func GetAPINamesSetFromOPConfig(serviceConfig *servicepb.Service, opts options.ConfigGeneratorOptions) map[string]bool {
	apiNames := make(map[string]bool)

	for _, api := range serviceConfig.GetApis() {
		if util.ShouldSkipOPDiscoveryAPI(api.GetName(), opts.AllowDiscoveryAPIs) {
			glog.Warningf("Skip API %q because discovery API is not supported.", api.GetName())
			continue
		}
		apiNames[api.GetName()] = true
	}

	return apiNames
}

// GetAPINamesListFromOPConfig is the same as GetAPINamesSetFromOPConfig,
// but returns a slice instead. Preserves original order of APIs in the service config.
//
// Replaces ServiceInfo::processApis.
func GetAPINamesListFromOPConfig(serviceConfig *servicepb.Service, opts options.ConfigGeneratorOptions) []string {
	var apiNames []string

	for _, api := range serviceConfig.GetApis() {
		if util.ShouldSkipOPDiscoveryAPI(api.GetName(), opts.AllowDiscoveryAPIs) {
			glog.Warningf("Skip API %q because discovery API is not supported.", api.GetName())
			continue
		}
		apiNames = append(apiNames, api.GetName())
	}

	return apiNames
}

// GetUsageRulesBySelectorFromOPConfig returns a map of selector to usage rule.
// Usage rules may be modified from original service config.
//
// Replaces ServiceInfo::processUsageRule.
func GetUsageRulesBySelectorFromOPConfig(serviceConfig *servicepb.Service, opts options.ConfigGeneratorOptions) map[string]*servicepb.UsageRule {
	rulesBySelector := make(map[string]*servicepb.UsageRule)

	for _, rule := range serviceConfig.GetUsage().GetRules() {
		if util.ShouldSkipOPDiscoveryAPI(rule.GetSelector(), opts.AllowDiscoveryAPIs) {
			glog.Warningf("Skip usage rule %q because discovery API is not supported.", rule.GetSelector())
			continue
		}

		rulesBySelector[rule.GetSelector()] = rule
	}

	for _, api := range serviceConfig.GetApis() {
		for _, method := range api.GetMethods() {
			selector := MethodToSelector(api, method)

			if shouldSkipSelector := skipServiceControlSelectors[selector]; !shouldSkipSelector {
				continue
			}

			if _, hasUserDefinedRule := rulesBySelector[selector]; hasUserDefinedRule {
				continue
			}

			rulesBySelector[selector] = &servicepb.UsageRule{
				Selector:           selector,
				SkipServiceControl: true,
			}
		}
	}

	return rulesBySelector
}

// GetAPIKeySystemParametersBySelectorFromOPConfig returns a map of selector to
// system parameter. Only includes system parameters for API Key.
//
// Replaces ServiceInfo::processApiKeyLocations.
func GetAPIKeySystemParametersBySelectorFromOPConfig(serviceConfig *servicepb.Service, opts options.ConfigGeneratorOptions) map[string][]*servicepb.SystemParameter {
	apiKeySystemParametersBySelector := make(map[string][]*servicepb.SystemParameter)

	for _, rule := range serviceConfig.GetSystemParameters().GetRules() {
		if util.ShouldSkipOPDiscoveryAPI(rule.GetSelector(), opts.AllowDiscoveryAPIs) {
			glog.Warningf("Skip SystemParameterRule %q because discovery API is not supported.", rule.GetSelector())
			continue
		}

		var apiKeySystemParameters []*servicepb.SystemParameter
		for _, parameter := range rule.GetParameters() {
			if parameter.GetName() == util.ApiKeyParameterName {
				apiKeySystemParameters = append(apiKeySystemParameters, parameter)
			}
		}
		apiKeySystemParametersBySelector[rule.GetSelector()] = apiKeySystemParameters
	}

	return apiKeySystemParametersBySelector
}
