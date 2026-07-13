package subscriber

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	routev1 "github.com/openshift/api/route/v1"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/clients"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/deployment"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/ingress"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/namespace"
	oranapi "github.com/rh-ecosystem-edge/eco-goinfra/pkg/oran/api"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/pod"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/route"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/service"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
)

// DefaultSubscriberImage is the default image for the subscriber. It is guaranteed to work in a connected deployment.
// Disconnected deployments will either need an IDMS applied or to provide a different image to [Deploy].
const DefaultSubscriberImage = "quay.io/rh-ee-klaskosk/oran-subscriber:latest"

// SubscriberServerPort is the default port for the subscriber. It is guaranteed to work with [DefaultSubscriberImage].
const SubscriberServerPort = 8080

// SubscriberLabelSelector is the default label selector for the subscriber deployment.
var SubscriberLabelSelector = map[string]string{"app": "subscriber"}

// LogLevel is the default glog verbosity level for this package.
const LogLevel klog.Level = 90

// Deploy deploys the oran-subscriber. It creates all resources in the provided namespace and exposes the subscriber
// with an edge-terminated Route on subscriberDomain. If routePath is empty, "/" is used. When routePath is not "/",
// the route rewrites matched path prefixes to "/" so the subscriber receives the trailing path segment. If the
// subscriber image is not provided, [DefaultSubscriberImage] will be used.
//
// Deploy waits until a GET to the subscriber URL returns HTTP 204, matching the O2IMS callback reachability check.
func Deploy(client *clients.Settings, nsname string, subscriberDomain string,
	subscriberImage string, routePath string) error {
	if subscriberImage == "" {
		subscriberImage = DefaultSubscriberImage
	}

	if routePath == "" {
		routePath = "/"
	}

	klog.V(LogLevel).Infof("Deploying subscriber in namespace %q with domain %q, path %q, and image %q",
		nsname, subscriberDomain, routePath, subscriberImage)

	_, err := namespace.NewBuilder(client, nsname).Create()
	if err != nil {
		return fmt.Errorf("failed to create namespace: %w", err)
	}

	klog.V(LogLevel).Infof("Successfully created namespace %q for subscriber deployment", nsname)

	deploymentBuilder := deployment.NewBuilder(client, "subscriber", nsname, SubscriberLabelSelector, corev1.Container{
		Name:  "subscriber",
		Image: subscriberImage,
		Ports: []corev1.ContainerPort{{ContainerPort: SubscriberServerPort}},
	})

	_, err = deploymentBuilder.CreateAndWaitUntilReady(5 * time.Minute)
	if err != nil {
		return fmt.Errorf("failed to create deployment: %w", err)
	}

	klog.V(LogLevel).Info("Successfully created deployment for subscriber")

	servicePort, err := service.DefineServicePort(SubscriberServerPort, SubscriberServerPort, corev1.ProtocolTCP)
	if err != nil {
		return fmt.Errorf("failed to define service port: %w", err)
	}

	serviceBuilder := service.NewBuilder(client, "subscriber", nsname, SubscriberLabelSelector, *servicePort)

	_, err = serviceBuilder.Create()
	if err != nil {
		return fmt.Errorf("failed to create service: %w", err)
	}

	klog.V(LogLevel).Info("Successfully created service for subscriber")

	routeBuilder := route.NewBuilder(client, "subscriber", nsname, "subscriber").
		WithHostDomain(subscriberDomain).
		WithTargetPortNumber(SubscriberServerPort)
	if routeBuilder.GetError() != nil {
		return fmt.Errorf("failed to create route builder: %w", routeBuilder.GetError())
	}

	routeBuilder.Definition.Spec.Path = routePath
	routeBuilder.Definition.Spec.TLS = &routev1.TLSConfig{
		Termination:                   routev1.TLSTerminationEdge,
		InsecureEdgeTerminationPolicy: routev1.InsecureEdgeTerminationPolicyRedirect,
	}

	// Strip the shared host path prefix so the subscriber sees /<uuid> rather than /oran-subscriber/<uuid>.
	if routePath != "/" {
		if routeBuilder.Definition.Annotations == nil {
			routeBuilder.Definition.Annotations = map[string]string{}
		}

		routeBuilder.Definition.Annotations["haproxy.router.openshift.io/rewrite-target"] = "/"
	}

	_, err = routeBuilder.Create()
	if err != nil {
		return fmt.Errorf("failed to create route: %w", err)
	}

	klog.V(LogLevel).Info("Successfully created route for subscriber")

	callbackBaseURL := "https://" + subscriberDomain + strings.TrimSuffix(routePath, "/")
	if err := waitForCallbackReachable(callbackBaseURL, 2*time.Minute); err != nil {
		return fmt.Errorf("failed waiting for subscriber callback to become reachable: %w", err)
	}

	klog.V(LogLevel).Infof("Subscriber callback is reachable at %s", callbackBaseURL)

	return nil
}

// waitForCallbackReachable polls until a GET of probeURL returns HTTP 204 No Content. This mirrors the O2IMS
// subscription callback reachability check.
func waitForCallbackReachable(callbackBaseURL string, timeout time.Duration) error {
	probeURL := strings.TrimRight(callbackBaseURL, "/") + "/healthz"
	httpClient := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}, //nolint:gosec
		},
	}

	return wait.PollUntilContextTimeout(
		context.TODO(), 5*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
			if err != nil {
				return false, fmt.Errorf("failed to create probe request: %w", err)
			}

			resp, err := httpClient.Do(req)
			if err != nil {
				klog.V(LogLevel).Infof("Subscriber reachability probe failed: %v", err)

				return false, nil
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusNoContent {
				klog.V(LogLevel).Infof("Subscriber reachability probe returned status %d, want %d",
					resp.StatusCode, http.StatusNoContent)

				return false, nil
			}

			return true, nil
		})
}

// Cleanup cleans up the subscriber. It deletes the route, legacy ingress, service, and deployment, and then the
// namespace. This function is idempotent, so it will not fail if the resources do not exist.
func Cleanup(client *clients.Settings, nsname string) error {
	routeBuilder, err := route.Pull(client, "subscriber", nsname)
	if err == nil {
		_, err = routeBuilder.Delete()
		if err != nil {
			return fmt.Errorf("failed to delete route: %w", err)
		}
	}

	klog.V(LogLevel).Info("Successfully cleaned up route for subscriber")

	// Clean up legacy Ingress resources from older deployments.
	ingressBuilder, err := ingress.PullIngress(client, "subscriber-ingress", nsname)
	if err == nil {
		err = ingressBuilder.Delete()
		if err != nil {
			return fmt.Errorf("failed to delete ingress: %w", err)
		}
	}

	klog.V(LogLevel).Info("Successfully cleaned up ingress for subscriber")

	serviceBuilder, err := service.Pull(client, "subscriber", nsname)
	if err == nil {
		err = serviceBuilder.Delete()
		if err != nil {
			return fmt.Errorf("failed to delete service: %w", err)
		}
	}

	klog.V(LogLevel).Info("Successfully cleaned up service for subscriber")

	deploymentBuilder, err := deployment.Pull(client, "subscriber", nsname)
	if err == nil {
		err = deploymentBuilder.DeleteAndWait(5 * time.Minute)
		if err != nil {
			return fmt.Errorf("failed to delete deployment: %w", err)
		}
	}

	klog.V(LogLevel).Info("Successfully cleaned up deployment for subscriber")

	nsBuilder, err := namespace.Pull(client, nsname)
	if err == nil {
		err = nsBuilder.DeleteAndWait(5 * time.Minute)
		if err != nil {
			return fmt.Errorf("failed to delete namespace: %w", err)
		}
	}

	klog.V(LogLevel).Info("Successfully cleaned up namespace for subscriber")

	return nil
}

// PullPod pulls the subscriber pod from the cluster. It will fail if no pods matching the app label are found. If more
// than one pod is found, it will log a warning and return the first one.
func PullPod(client *clients.Settings, nsname string) (*pod.Builder, error) {
	matchingPods, err := pod.List(client, nsname, metav1.ListOptions{
		LabelSelector: labels.Set(SubscriberLabelSelector).String(),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods for the subscriber: %w", err)
	}

	if len(matchingPods) == 0 {
		return nil, fmt.Errorf("no pods for the subscriber found in namespace %q", nsname)
	}

	if len(matchingPods) > 1 {
		klog.V(LogLevel).Infof("Expected 1 pod for the subscriber, but found %d in namespace %q, using the first one",
			len(matchingPods), nsname)
	}

	klog.V(LogLevel).Infof("Successfully pulled pod %q in namespace %q for subscriber",
		matchingPods[0].Definition.Name, nsname)

	return matchingPods[0], nil
}

// waitForNotificationOptions are all of the options for the wait for a matching notification. It is used for an options
// pattern to the WaitForNotification function.
type waitForNotificationOptions struct {
	timeout   time.Duration
	start     time.Time
	matchFunc func(notification *oranapi.AlarmEventNotification) bool
}

// getDefaultWaitForNotificationOptions returns the default options for the wait for a matching notification. The
// defaults are a 30 second timeout, start time of now (when this function is called) and a match function that returns
// true if any notification is received.
func getDefaultWaitForNotificationOptions() *waitForNotificationOptions {
	return &waitForNotificationOptions{
		timeout:   time.Second * 30,
		start:     time.Now(),
		matchFunc: func(notification *oranapi.AlarmEventNotification) bool { return true },
	}
}

// waitForNotificationOption is a function that can be used to modify the options for the wait for a matching
// notification.
type waitForNotificationOption func(options *waitForNotificationOptions)

// WithTimeout sets the timeout for the wait for a matching notification.
func WithTimeout(timeout time.Duration) waitForNotificationOption {
	return func(options *waitForNotificationOptions) {
		options.timeout = timeout
	}
}

// WithStart sets the start time for the wait for a matching notification.
func WithStart(start time.Time) waitForNotificationOption {
	return func(options *waitForNotificationOptions) {
		options.start = start
	}
}

// WithMatchFunc sets the match function for the wait for a matching notification.
func WithMatchFunc(matchFunc func(notification *oranapi.AlarmEventNotification) bool) waitForNotificationOption {
	return func(options *waitForNotificationOptions) {
		options.matchFunc = matchFunc
	}
}

// WaitForNotification waits for a notification to be received from the subscriber. Callers may provide options,
// otherwise the defaults of 30 seconds timeout, start time of now, and a match function that returns true if any
// notification is received will be used.
func WaitForNotification(client *clients.Settings, namespace string, options ...waitForNotificationOption) error {
	appliedOptions := getDefaultWaitForNotificationOptions()

	for _, option := range options {
		option(appliedOptions)
	}

	pod, err := PullPod(client, namespace)
	if err != nil {
		return fmt.Errorf("failed to pull subscriber pod: %w", err)
	}

	return wait.PollUntilContextTimeout(
		context.TODO(), time.Second, appliedOptions.timeout, true, func(ctx context.Context) (bool, error) {
			newStart := time.Now()

			notificationsRaw, err := pod.GetLogsWithOptions(&corev1.PodLogOptions{
				SinceTime: &metav1.Time{Time: appliedOptions.start},
			})
			if err != nil {
				return false, fmt.Errorf("failed to get subscriber pod logs: %w", err)
			}

			// By setting the new start time, we avoid looking at the same notifications again. This is not
			// perfect, since we may still see duplicates, but it guarantees we do not miss any while being
			// more efficient.
			appliedOptions.start = newStart

			parsedNotifications, err := parseNotifications(notificationsRaw)
			if err != nil {
				return false, fmt.Errorf("failed to parse notifications: %w", err)
			}

			return slices.ContainsFunc(parsedNotifications, appliedOptions.matchFunc), nil
		})
}

// ListReceivedNotifications lists the notifications received by the subscriber since the given time. If sinceTime is
// zero, then all notifications will be listed.
func ListReceivedNotifications(
	client *clients.Settings, namespace string, sinceTime time.Time) ([]*oranapi.AlarmEventNotification, error) {
	pod, err := PullPod(client, namespace)
	if err != nil {
		return nil, fmt.Errorf("failed to pull subscriber pod: %w", err)
	}

	podOptionSinceTime := &metav1.Time{Time: sinceTime}
	if sinceTime.IsZero() {
		podOptionSinceTime = nil
	}

	notificationsRaw, err := pod.GetLogsWithOptions(&corev1.PodLogOptions{
		SinceTime: podOptionSinceTime,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get subscriber pod logs: %w", err)
	}

	parsedNotifications, err := parseNotifications(notificationsRaw)
	if err != nil {
		return nil, fmt.Errorf("failed to parse notifications: %w", err)
	}

	return parsedNotifications, nil
}

// parseNotifications parses the raw notifications from the subscriber pod logs. It will return any errors encountered
// in parsing. If an error is returned, the notifications will be empty. Elements of the returned slice are guaranteed
// not to be nil.
//
// Notifications are expected to be in the format of one per line with the line containing the raw JSON of the
// notification. The parsing will look for the opening curly brace to determine the start of the notification. Anything
// prior to it on the line will be ignored.
func parseNotifications(notificationsRaw []byte) ([]*oranapi.AlarmEventNotification, error) {
	var notifications []*oranapi.AlarmEventNotification

	scanner := bufio.NewScanner(bytes.NewReader(notificationsRaw))
	for scanner.Scan() {
		line := scanner.Bytes()

		jsonStart := bytes.IndexByte(line, '{')
		if jsonStart == -1 {
			continue
		}

		var notification oranapi.AlarmEventNotification

		err := json.Unmarshal(line[jsonStart:], &notification)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal notification from line %q: %w", string(line), err)
		}

		notifications = append(notifications, &notification)
	}

	err := scanner.Err()
	if err != nil {
		return nil, fmt.Errorf("error scanning notifications: %w", err)
	}

	return notifications, nil
}
