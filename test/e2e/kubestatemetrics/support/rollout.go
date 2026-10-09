package kubestatemetricssupport

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
)

// SingleReplicaRolloutComplete 要求新 generation 的单副本 Deployment 不再保留旧 Pod。
func SingleReplicaRolloutComplete(deployment *appsv1.Deployment, previousGeneration int64) error {
	status := deployment.Status
	if deployment.Generation <= previousGeneration || status.ObservedGeneration != deployment.Generation ||
		status.Replicas != 1 || status.UpdatedReplicas != 1 || status.ReadyReplicas != 1 ||
		status.AvailableReplicas != 1 || status.UnavailableReplicas != 0 {
		return fmt.Errorf(
			"rollout 尚未完成: generation=%d previous=%d observed=%d replicas=%d updated=%d ready=%d available=%d unavailable=%d",
			deployment.Generation,
			previousGeneration,
			status.ObservedGeneration,
			status.Replicas,
			status.UpdatedReplicas,
			status.ReadyReplicas,
			status.AvailableReplicas,
			status.UnavailableReplicas,
		)
	}
	return nil
}
