package availability_service_loadbalancer

import (
	"context"
	"crypto/tls"
	_ "embed"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openshift/origin/pkg/monitor/backenddisruption"
	"github.com/openshift/origin/test/extended/util/disruption"

	utilnet "k8s.io/apimachinery/pkg/util/net"

	"github.com/openshift/library-go/pkg/operator/resource/resourceread"

	configv1 "github.com/openshift/api/config/v1"
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	"github.com/openshift/origin/pkg/invariants"
	"github.com/openshift/origin/pkg/monitor/monitorapi"
	"github.com/openshift/origin/pkg/test/ginkgo/junitapi"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/kubernetes/test/e2e/framework"
	"k8s.io/kubernetes/test/e2e/framework/service"
)

var (
	//go:embed namespace.yaml
	namespaceYaml []byte
)

var (
	namespace *corev1.Namespace
)

const (
	newConnectionTestName    = "[sig-network-edge] Application behind service load balancer with PDB remains available using new connections"
	reusedConnectionTestName = "[sig-network-edge] Application behind service load balancer with PDB remains available using reused connections"
)

func init() {
	namespace = resourceread.ReadNamespaceV1OrDie(namespaceYaml)
}

type availability struct {
	namespaceName                     string
	notSupportedReason                string
	kubeClient                        kubernetes.Interface
	newConnectionDisruptionSampler    *backenddisruption.BackendSampler
	reusedConnectionDisruptionSampler *backenddisruption.BackendSampler

	// TODO stop storing this and use clients so we can unit test if we want
	adminRESTConfig *rest.Config
}

func NewAvailabilityInvariant() invariants.InvariantTest {
	return &availability{}
}

func (w *availability) StartCollection(ctx context.Context, adminRESTConfig *rest.Config, recorder monitorapi.RecorderWriter) error {
	var err error

	w.adminRESTConfig = adminRESTConfig
	w.kubeClient, err = kubernetes.NewForConfig(adminRESTConfig)
	if err != nil {
		return err
	}

	actualNamespace, err := w.kubeClient.CoreV1().Namespaces().Create(context.Background(), namespace, metav1.CreateOptions{})
	if err != nil {
		return err
	}
	w.namespaceName = actualNamespace.Name

	configClient, err := configclient.NewForConfig(adminRESTConfig)
	if err != nil {
		return err
	}
	infra, err := configClient.ConfigV1().Infrastructures().Get(context.Background(), "cluster", metav1.GetOptions{})
	if err != nil {
		return err
	}
	// ovirt does not support service type loadbalancer because it doesn't program a cloud.
	if infra.Status.PlatformStatus.Type == configv1.OvirtPlatformType || infra.Status.PlatformStatus.Type == configv1.KubevirtPlatformType || infra.Status.PlatformStatus.Type == configv1.LibvirtPlatformType || infra.Status.PlatformStatus.Type == configv1.VSpherePlatformType || infra.Status.PlatformStatus.Type == configv1.BareMetalPlatformType {
		w.notSupportedReason = fmt.Sprintf("platform %q is not supported", infra.Status.PlatformStatus.Type)
	}
	// single node clusters are not supported because the replication controller has 2 replicas with anti-affinity for running on the same node.
	if infra.Status.ControlPlaneTopology == configv1.SingleReplicaTopologyMode {
		w.notSupportedReason = fmt.Sprintf("topology %q is not supported", infra.Status.ControlPlaneTopology)
	}
	if len(w.notSupportedReason) > 0 {
		return nil
	}

	serviceName := "service-test"
	jig := service.NewTestJig(w.kubeClient, w.namespaceName, serviceName)

	fmt.Fprintf(os.Stderr, "creating a TCP service %v with type=LoadBalancer in namespace %v\n", serviceName, w.namespaceName)
	tcpService, err := jig.CreateTCPService(ctx, func(s *corev1.Service) {
		s.Spec.Type = corev1.ServiceTypeLoadBalancer
		// ServiceExternalTrafficPolicyTypeCluster performs during disruption, Local does not
		s.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyTypeCluster
		if s.Annotations == nil {
			s.Annotations = make(map[string]string)
		}
		// We tune the LB checks to match the longest intervals available so that interactions between
		// upgrading components and the service are more obvious.
		// - AWS allows configuration, default is 70s (6 failed with 10s interval in 1.17) set to match GCP
		s.Annotations["service.beta.kubernetes.io/aws-load-balancer-healthcheck-interval"] = "8"
		s.Annotations["service.beta.kubernetes.io/aws-load-balancer-healthcheck-unhealthy-threshold"] = "3"
		s.Annotations["service.beta.kubernetes.io/aws-load-balancer-healthcheck-healthy-threshold"] = "2"
		// - Azure is hardcoded to 15s (2 failed with 5s interval in 1.17) and is sufficient
		// - GCP has a non-configurable interval of 32s (3 failed health checks with 8s interval in 1.17)
		//   - thus pods need to stay up for > 32s, so pod shutdown period will will be 45s
	})
	if err != nil {
		return fmt.Errorf("error creating tcp service: %w", err)
	}
	tcpService, err = jig.WaitForLoadBalancer(ctx, service.GetServiceLoadBalancerCreationTimeout(ctx, w.kubeClient))
	if err != nil {
		return fmt.Errorf("error waiting for load balancer: %w", err)
	}

	// Get info to hit it with
	tcpIngressIP := service.GetIngressPoint(&tcpService.Status.LoadBalancer.Ingress[0])
	svcPort := int(tcpService.Spec.Ports[0].Port)

	fmt.Fprintf(os.Stderr, "creating RC to be part of service %v\n", serviceName)
	rc, err := jig.Run(ctx, func(rc *corev1.ReplicationController) {
		// ensure the pod waits long enough during update for the LB to see the newly ready pod, which
		// must be longer than the worst load balancer above (GCP at 32s)
		rc.Spec.MinReadySeconds = 33

		// use a readiness endpoint that will go not ready before the pod terminates.
		// the probe will go false when the sig-term is sent.
		rc.Spec.Template.Spec.Containers[0].ReadinessProbe.HTTPGet.Path = "/readyz"

		// delay shutdown long enough to go readyz=false before the process exits when the pod is deleted.
		// 80 second delay was found to not show disruption in testing
		rc.Spec.Template.Spec.Containers[0].Args = append(rc.Spec.Template.Spec.Containers[0].Args, "--delay-shutdown=80")

		// ensure the pod is not forcibly deleted at 30s, but waits longer than the graceful sleep
		minuteAndAHalf := int64(90)
		rc.Spec.Template.Spec.TerminationGracePeriodSeconds = &minuteAndAHalf

		jig.AddRCAntiAffinity(rc)
	})
	if err != nil {
		return fmt.Errorf("error waiting for replicaset: %w", err)
	}

	fmt.Fprintf(os.Stderr, "creating a PodDisruptionBudget to cover the ReplicationController\n")
	_, err = jig.CreatePDB(ctx, rc)
	framework.ExpectNoError(err)

	// Hit it once before considering ourselves ready
	fmt.Fprintf(os.Stderr, "hitting pods through the service's LoadBalancer\n")
	timeout := 10 * time.Minute
	// require thirty seconds of passing requests to continue (in case the SLB becomes available and then degrades)
	// TODO this seems weird to @deads2k, why is status not trustworthy
	baseURL := fmt.Sprintf("http://%s", net.JoinHostPort(tcpIngressIP, strconv.Itoa(svcPort)))
	path := "/echo?msg=hello"
	url := fmt.Sprintf("%s%s", baseURL, path)
	if err := testReachableHTTPWithMinSuccessCount(url, 30, timeout); err != nil {
		return fmt.Errorf("could not reach %v reliably: %w", url, err)
	}

	w.newConnectionDisruptionSampler =
		backenddisruption.NewSimpleBackend(
			baseURL,
			"service-load-balancer-with-pdb",
			path,
			monitorapi.NewConnectionType).
			WithExpectedBody("hello")

	w.reusedConnectionDisruptionSampler =
		backenddisruption.NewSimpleBackend(
			baseURL,
			"service-load-balancer-with-pdb",
			path,
			monitorapi.ReusedConnectionType).
			WithExpectedBody("hello")
	if err := w.newConnectionDisruptionSampler.StartEndpointMonitoring(ctx, recorder, nil); err != nil {
		return err
	}
	if err := w.reusedConnectionDisruptionSampler.StartEndpointMonitoring(ctx, recorder, nil); err != nil {
		return err
	}

	return nil
}

func (w *availability) CollectData(ctx context.Context, storageDir string, beginning, end time.Time) (monitorapi.Intervals, []*junitapi.JUnitTestCase, error) {
	if len(w.notSupportedReason) == 0 {
		return nil, nil, nil
	}

	// when it is time to collect data, we need to stop the collectors.  they both  have to drain, so stop in parallel
	wg := sync.WaitGroup{}
	wg.Add(2)
	go func() {
		defer wg.Done()
		w.newConnectionDisruptionSampler.Stop()
	}()
	go func() {
		defer wg.Done()
		w.reusedConnectionDisruptionSampler.Stop()
	}()
	wg.Wait()

	return nil, nil, nil
}

func (*availability) ConstructComputedIntervals(ctx context.Context, startingIntervals monitorapi.Intervals, recordedResources monitorapi.ResourcesMap, beginning, end time.Time) (monitorapi.Intervals, error) {
	return nil, nil
}

func createDisruptionJunit(testName string, allowedDisruption *time.Duration, disruptionDetails, locator string, disruptedIntervals monitorapi.Intervals) *junitapi.JUnitTestCase {
	// Indicates there is no entry in the query_results.json data file, nor a valid fallback,
	// we do not wish to run the test. (this likely implies we do not have the required number of
	// runs in 3 weeks to do a reliable P99)
	if allowedDisruption == nil {
		return &junitapi.JUnitTestCase{
			Name: testName,
			SkipMessage: &junitapi.SkipMessage{
				Message: "No historical data to calculate allowedDisruption",
			},
		}
	}

	if *allowedDisruption < 1*time.Second {
		t := 1 * time.Second
		allowedDisruption = &t
		disruptionDetails = "always allow at least one second"
	}

	disruptionDuration := disruptedIntervals.Duration(1 * time.Second)
	roundedAllowedDisruption := allowedDisruption.Round(time.Second)
	roundedDisruptionDuration := disruptionDuration.Round(time.Second)

	if roundedDisruptionDuration <= roundedAllowedDisruption {
		return &junitapi.JUnitTestCase{
			Name: testName,
		}
	}

	reason := fmt.Sprintf("%s was unreachable during disruption: %v", locator, disruptionDetails)
	describe := disruptedIntervals.Strings()
	failureMessage := fmt.Sprintf("%s for at least %s (maxAllowed=%s):\n\n%s", reason, roundedDisruptionDuration, roundedAllowedDisruption, strings.Join(describe, "\n"))

	return &junitapi.JUnitTestCase{
		Name: testName,
		FailureOutput: &junitapi.FailureOutput{
			Output: failureMessage,
		},
		SystemOut: failureMessage,
	}
}

func (w *availability) EvaluateTestsFromConstructedIntervals(ctx context.Context, finalIntervals monitorapi.Intervals) ([]*junitapi.JUnitTestCase, error) {
	if len(w.notSupportedReason) == 0 {
		return nil, nil
	}

	junits := []*junitapi.JUnitTestCase{}

	// If someone feels motivated to DRY out further, find a way to abstract for ALL disruption.  I don't see it yet.
	// DO NOT MERGE something that just makes new and reused look the same.  That's a waste of time and willmake drift worse.
	{ // block to prevent cross-contamination.
		newConnectionAllowed, newConnectionDisruptionDetails, err := disruption.HistoricalAllowedDisruption(ctx, w.newConnectionDisruptionSampler, w.adminRESTConfig)
		if err != nil {
			return nil, fmt.Errorf("unable to get new allowed disruption: %w", err)
		}
		junits = append(junits,
			createDisruptionJunit(
				newConnectionTestName, newConnectionAllowed, newConnectionDisruptionDetails, w.newConnectionDisruptionSampler.GetLocator(),
				finalIntervals.Filter(
					monitorapi.And(
						monitorapi.IsEventForLocator(w.newConnectionDisruptionSampler.GetLocator()),
						monitorapi.IsErrorEvent,
					),
				),
			),
		)
	}

	{ // block to prevent cross-contamination
		reusedConnectionAllowed, reusedConnectionDisruptionDetails, err := disruption.HistoricalAllowedDisruption(ctx, w.reusedConnectionDisruptionSampler, w.adminRESTConfig)
		if err != nil {
			return nil, fmt.Errorf("unable to get reused allowed disruption: %w", err)
		}
		junits = append(junits,
			createDisruptionJunit(
				reusedConnectionTestName, reusedConnectionAllowed, reusedConnectionDisruptionDetails, w.reusedConnectionDisruptionSampler.GetLocator(),
				finalIntervals.Filter(
					monitorapi.And(
						monitorapi.IsEventForLocator(w.reusedConnectionDisruptionSampler.GetLocator()),
						monitorapi.IsErrorEvent,
					),
				),
			),
		)
	}

	return junits, nil
}

func (*availability) WriteContentToStorage(ctx context.Context, storageDir, timeSuffix string, finalIntervals monitorapi.Intervals, finalResourceState monitorapi.ResourcesMap) error {
	return nil
}

func (w *availability) Cleanup(ctx context.Context) error {
	if len(w.namespaceName) > 0 {
		if err := w.kubeClient.CoreV1().Namespaces().Delete(ctx, w.namespaceName, metav1.DeleteOptions{}); err != nil {
			return err
		}
	}
	return nil
}

// testReachableHTTPWithMinSuccessCount tests that the given host serves HTTP on the given port for a minimum of successCount number of
// counts at a given interval. If the service reachability fails, the counter gets reset
func testReachableHTTPWithMinSuccessCount(url string, successCount int, timeout time.Duration) error {
	consecutiveSuccessCnt := 0
	err := wait.PollImmediate(1*time.Second, timeout, func() (bool, error) {
		resp, err := httpGetNoConnectionPoolTimeout(url, 10*time.Second)
		if err != nil {
			consecutiveSuccessCnt = 0
			return false, nil
		}
		resp.Body.Close()
		consecutiveSuccessCnt++
		return consecutiveSuccessCnt >= successCount, nil
	})
	if err != nil {
		return err
	}
	return nil
}

// Does an HTTP GET, but does not reuse TCP connections
// This masks problems where the iptables rule has changed, but we don't see it
func httpGetNoConnectionPoolTimeout(url string, timeout time.Duration) (*http.Response, error) {
	tr := utilnet.SetTransportDefaults(&http.Transport{
		DisableKeepAlives: true,
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
	})
	client := &http.Client{
		Transport: tr,
		Timeout:   timeout,
	}

	return client.Get(url)
}
