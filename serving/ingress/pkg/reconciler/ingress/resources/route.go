package resources

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"

	routev1 "github.com/openshift/api/route/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"knative.dev/networking/pkg/apis/networking"
	networkingv1alpha1 "knative.dev/networking/pkg/apis/networking/v1alpha1"
	"knative.dev/pkg/kmeta"
	"knative.dev/pkg/ptr"
	"knative.dev/serving/pkg/apis/config"
)

const (
	TimeoutAnnotation      = "haproxy.router.openshift.io/timeout"
	DisableRouteAnnotation = "serving.knative.openshift.io/disableRoute"
	KourierHTTPPort        = "http2"
)

var defaultTimeout = fmt.Sprintf("%vs", config.DefaultMaxRevisionTimeoutSeconds)

// ErrNoValidLoadbalancerDomain indicates that the current ingress does not have a DomainInternal field, or
// said field does not contain a value we can work with.
var ErrNoValidLoadbalancerDomain = errors.New("unable to find Ingress LoadBalancer with DomainInternal set")

// MakeRoutes creates OpenShift Routes from a Knative Ingress
func MakeRoutes(ci *networkingv1alpha1.Ingress) ([]*routev1.Route, error) {
	routes := []*routev1.Route{}

	for _, rule := range ci.Spec.Rules {
		// Skip route creation for cluster-local visibility.
		if rule.Visibility == networkingv1alpha1.IngressVisibilityClusterLocal {
			continue
		}
		for _, host := range rule.Hosts {
			// Ignore domains like myksvc.myproject.svc.cluster.local
			// TODO: This also ignores any top-level vanity domains
			// like foo.com the user may have set. But, it tackles the
			// autogenerated name case which is the biggest pain
			// point.
			parts := strings.Split(host, ".")
			if len(parts) > 2 && parts[2] != "svc" {
				route, err := makeRoute(ci, host, rule)
				if err != nil {
					return nil, err
				}
				if route == nil {
					continue
				}
				routes = append(routes, route)
			}
		}
	}

	return routes, nil
}

func makeRoute(ci *networkingv1alpha1.Ingress, host string, rule networkingv1alpha1.IngressRule) (*routev1.Route, error) {
	// Take over annotaitons from ingress.
	annotations := ci.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}

	// Skip making route when visibility of the rule is local only.
	if rule.Visibility == networkingv1alpha1.IngressVisibilityClusterLocal {
		return nil, nil
	}

	// Skip making route when the annotation is specified.
	if _, ok := annotations[DisableRouteAnnotation]; ok {
		return nil, nil
	}

	if rule.HTTP != nil {
		for i := range rule.HTTP.Paths {
			if rule.HTTP.Paths[i].DeprecatedTimeout != nil {
				// Supported time units for openshift route annotations are microseconds (us), milliseconds (ms), seconds (s), minutes (m), hours (h), or days (d)
				// But the timeout value from ingress is in xmys(ex: 10m0s) format
				// So, in order to make openshift route to work converting it into seconds.
				annotations[TimeoutAnnotation] = fmt.Sprintf("%vs", rule.HTTP.Paths[i].DeprecatedTimeout.Duration.Seconds())
			} else {
				annotations[TimeoutAnnotation] = defaultTimeout
			}

		}
	}

	labels := kmeta.UnionMaps(ci.Labels, map[string]string{
		networking.IngressLabelKey: ci.GetName(),
	})

	name := routeName(string(ci.GetUID()), host)
	serviceName := ""
	namespace := ""
	if ci.Status.PublicLoadBalancer != nil {
		for _, lbIngress := range ci.Status.PublicLoadBalancer.Ingress {
			if lbIngress.DomainInternal != "" {
				// DomainInternal should look something like:
				// kourier.knative-serving-ingress.svc.cluster.local
				parts := strings.Split(lbIngress.DomainInternal, ".")
				if len(parts) > 2 && parts[2] == "svc" {
					serviceName = parts[0]
					namespace = parts[1]
				}
			}
		}
	}

	if serviceName == "" || namespace == "" {
		return nil, ErrNoValidLoadbalancerDomain
	}

	route := &routev1.Route{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: routev1.RouteSpec{
			Host: host,
			Port: &routev1.RoutePort{
				TargetPort: intstr.FromString(KourierHTTPPort),
			},
			To: routev1.RouteTargetReference{
				Kind:   "Service",
				Name:   serviceName,
				Weight: ptr.Int32(100),
			},
			TLS: &routev1.TLSConfig{
				Termination:                   routev1.TLSTerminationEdge,
				InsecureEdgeTerminationPolicy: routev1.InsecureEdgeTerminationPolicyAllow,
			},
			WildcardPolicy: routev1.WildcardPolicyNone,
		},
	}
	return route, nil
}

func routeName(uid, host string) string {
	return fmt.Sprintf("route-%s-%x", uid, hashHost(host))
}

func hashHost(host string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(host)))[0:6]
}
