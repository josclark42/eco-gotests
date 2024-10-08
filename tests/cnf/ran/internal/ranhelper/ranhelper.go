package ranhelper

import (
	"context"
	"os/exec"
	"time"

	"github.com/golang/glog"
	"github.com/openshift-kni/eco-goinfra/pkg/pod"
	"github.com/openshift-kni/eco-gotests/tests/cnf/ran/internal/ranparam"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// DoesContainerExistInPod checks if a given container exists in a given pod.
func DoesContainerExistInPod(pod *pod.Builder, containerName string) bool {
	containers := pod.Object.Status.ContainerStatuses

	for _, container := range containers {
		if container.Name == containerName {
			glog.V(ranparam.LogLevel).Infof("found %s container", containerName)

			return true
		}
	}

	return false
}

// UnmarshalRaw converts raw bytes for a K8s CR into the actual type.
func UnmarshalRaw[T any](raw []byte) (*T, error) {
	untyped := &unstructured.Unstructured{}
	err := untyped.UnmarshalJSON(raw)

	if err != nil {
		return nil, err
	}

	var typed T
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(untyped.UnstructuredContent(), &typed)

	if err != nil {
		return nil, err
	}

	return &typed, nil
}

// ExecLocalCommand runs the provided command with the provided args locally, cancelling execution if it exceeds
// timeout.
func ExecLocalCommand(timeout time.Duration, command string, args ...string) (string, error) {
	glog.V(90).Infof("Locally executing command '%s' with args '%v'", command, args)

	ctx, cancel := context.WithTimeout(context.TODO(), timeout)

	defer cancel()

	output, err := exec.CommandContext(ctx, command, args...).Output()

	return string(output), err
}
