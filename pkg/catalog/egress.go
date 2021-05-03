package catalog

import (
	"fmt"
	"net"
	"strings"

	mapset "github.com/deckarep/golang-set"
	smiSpecs "github.com/servicemeshinterface/smi-sdk-go/pkg/apis/specs/v1alpha4"

	policyV1alpha1 "github.com/openservicemesh/osm/pkg/apis/policy/v1alpha1"

	"github.com/openservicemesh/osm/pkg/constants"
	"github.com/openservicemesh/osm/pkg/identity"
	"github.com/openservicemesh/osm/pkg/service"
	"github.com/openservicemesh/osm/pkg/trafficpolicy"
)

const (
	protocolHTTP = "http"
)

// GetEgressTrafficPolicy returns the Egress traffic policy associated with the given service identity
func (mc *MeshCatalog) GetEgressTrafficPolicy(serviceIdentity identity.ServiceIdentity) (*trafficpolicy.EgressTrafficPolicy, error) {
	var trafficMatches []*trafficpolicy.TrafficMatch
	var clusterConfigs []*trafficpolicy.EgressClusterConfig
	allowedDestinationPorts := mapset.NewSet()
	portToRouteConfigMap := make(map[int][]*trafficpolicy.EgressHTTPRouteConfig)

	egressResources := mc.policyController.ListEgressPoliciesForSourceIdentity(serviceIdentity.ToK8sServiceAccount())

	for _, egress := range egressResources {
		for _, portSpec := range egress.Spec.Ports {
			// ---
			// Build the HTTP route configs for the given Egress policy
			if strings.EqualFold(portSpec.Protocol, protocolHTTP) {
				httpRouteConfigs, httpClusterConfigs := mc.buildHTTPRouteConfigs(egress, portSpec.Number)
				portToRouteConfigMap[portSpec.Number] = append(portToRouteConfigMap[portSpec.Number], httpRouteConfigs...)
				clusterConfigs = append(clusterConfigs, httpClusterConfigs...)
			}

			// ---
			// TODO(#3045): Build the TCP route configs for the given Egress policy

			// ---
			// Build traffic matches for the given Egress policy.
			// Traffic matches are used to match outbound traffic as egress traffic using the port numbers
			// specified in Egress policies.
			newlyAdded := allowedDestinationPorts.Add(portSpec)
			if newlyAdded {
				trafficMatches = append(trafficMatches, &trafficpolicy.TrafficMatch{
					DestinationPort: portSpec,
				})
			}
		}
	}

	return &trafficpolicy.EgressTrafficPolicy{
		HTTPRouteConfigsPerPort: portToRouteConfigMap,
		TrafficMatches:          trafficMatches,
		ClustersConfigs:         clusterConfigs,
	}, nil
}

func (mc *MeshCatalog) buildHTTPRouteConfigs(egressPolicy *policyV1alpha1.Egress, port int) ([]*trafficpolicy.EgressHTTPRouteConfig, []*trafficpolicy.EgressClusterConfig) {
	if egressPolicy == nil {
		return nil, nil
	}

	var routeConfigs []*trafficpolicy.EgressHTTPRouteConfig
	var clusterConfigs []*trafficpolicy.EgressClusterConfig

	// Before building the route configs, pre-compute the allowed IP ranges since they
	// will be the same for every HTTP route config derived from the given Egress policy.
	var allowedDestinationIPRanges []string
	destIPSet := mapset.NewSet()
	for _, ipRange := range egressPolicy.Spec.IPAddresses {
		if _, _, err := net.ParseCIDR(ipRange); err != nil {
			log.Error().Err(err).Msgf("Invalid IP range [%s] specified in egress policy %s/%s; will be skipped", ipRange, egressPolicy.Namespace, egressPolicy.Name)
			continue
		}
		newlyAdded := destIPSet.Add(ipRange)
		if newlyAdded {
			allowedDestinationIPRanges = append(allowedDestinationIPRanges, ipRange)
		}
	}

	// Check if there are object references to HTTP routes specified
	// in the Egress policy's 'matches' attribute. If there are HTTP route
	// matches, apply these routes.
	var httpRouteMatches []trafficpolicy.HTTPRouteMatch
	httpMatchSpecified := false
	for _, match := range egressPolicy.Spec.Matches {
		if match.APIGroup != nil && *match.APIGroup == smiSpecs.SchemeGroupVersion.String() && match.Kind == httpRouteGroupKind {
			// HTTPRouteGroup resource referenced, build a routing rule from this resource
			httpMatchSpecified = true

			// A TypedLocalObjectReference (Spec.Matches) is a reference to another object in the same namespace
			httpRouteName := fmt.Sprintf("%s/%s", egressPolicy.Namespace, match.Name)
			if httpRouteGroup := mc.meshSpec.GetHTTPRouteGroup(httpRouteName); httpRouteGroup == nil {
				log.Error().Msgf("Error fetching HTTPRouteGroup resource %s referenced in Egress policy %s/%s", httpRouteName, egressPolicy.Namespace, egressPolicy.Name)
			} else {
				matches := getHTTPRouteMatchesFromHTTPRouteGroup(httpRouteGroup)
				httpRouteMatches = append(httpRouteMatches, matches...)
			}
		} else {
			log.Error().Msgf("Unsupported match object specified: %v, ignoring it", match)
		}
	}

	if !httpMatchSpecified {
		// No HTTP match specified, use a wildcard
		httpRouteMatches = append(httpRouteMatches, trafficpolicy.WildCardRouteMatch)
	}

	// Parse the hosts specified and build routing rules for the specified hosts
	for _, host := range egressPolicy.Spec.Hosts {
		// A route matching an HTTP host will include host header matching for the following:
		// 1. host (ex. foo.com)
		// 2. host:port (ex. foo.com:80)
		hostnameWithPort := fmt.Sprintf("%s:%d", host, port)
		hostnames := []string{host, hostnameWithPort}

		// Create cluster config for this host and port combination
		clusterName := hostnameWithPort
		clusterConfig := &trafficpolicy.EgressClusterConfig{
			Name: clusterName,
			Host: host,
			Port: port,
		}
		clusterConfigs = append(clusterConfigs, clusterConfig)

		// Build egress routing rules from the given HTTP route matches and allowed destination attributes
		var httpRoutingRules []*trafficpolicy.EgressHTTPRoutingRule
		for _, match := range httpRouteMatches {
			routeWeightedCluster := trafficpolicy.RouteWeightedClusters{
				HTTPRouteMatch: match,
				WeightedClusters: mapset.NewSetFromSlice([]interface{}{
					service.WeightedCluster{ClusterName: service.ClusterName(clusterName), Weight: constants.ClusterWeightAcceptAll},
				}),
			}
			routingRule := &trafficpolicy.EgressHTTPRoutingRule{
				Route:                      routeWeightedCluster,
				AllowedDestinationIPRanges: allowedDestinationIPRanges,
			}
			httpRoutingRules = append(httpRoutingRules, routingRule)
		}

		// Hostnames and routing rules are computed for the given host, build an HTTP route config for it
		hostSpecificRouteConfig := &trafficpolicy.EgressHTTPRouteConfig{
			Name:         host,
			Hostnames:    hostnames,
			RoutingRules: httpRoutingRules,
		}

		routeConfigs = append(routeConfigs, hostSpecificRouteConfig)
	}

	return routeConfigs, clusterConfigs
}

func getHTTPRouteMatchesFromHTTPRouteGroup(httpRouteGroup *smiSpecs.HTTPRouteGroup) []trafficpolicy.HTTPRouteMatch {
	if httpRouteGroup == nil {
		return nil
	}

	var matches []trafficpolicy.HTTPRouteMatch
	for _, match := range httpRouteGroup.Spec.Matches {
		httpRouteMatch := trafficpolicy.HTTPRouteMatch{
			Path:          match.PathRegex,
			PathMatchType: trafficpolicy.PathMatchRegex,
			Methods:       match.Methods,
			Headers:       match.Headers,
		}

		// When pathRegex and/or methods are not defined, they should be wildcarded
		if httpRouteMatch.Path == "" {
			httpRouteMatch.Path = constants.RegexMatchAll
		}
		if len(httpRouteMatch.Methods) == 0 {
			httpRouteMatch.Methods = []string{constants.WildcardHTTPMethod}
		}

		matches = append(matches, httpRouteMatch)
	}

	return matches
}
