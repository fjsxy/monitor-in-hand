package kubestatemetricssupport_test

import (
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	kubestatemetricssupport "github.com/fjsxy/monitor-in-hand/test/e2e/kubestatemetrics/support"
)

var _ = Describe("Deployment rollout convergence", func() {
	It("[UT-KSM-GH81-002] 不把仍由旧 Pod 提供 available 副本的 rollout 判为完成", func() {
		By("重放 CI 中 generation 已增加且旧 Pod 仍 available 的中间状态")
		deployment := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Generation: 2},
			Status: appsv1.DeploymentStatus{
				ObservedGeneration:  2,
				Replicas:            2,
				UpdatedReplicas:     1,
				ReadyReplicas:       1,
				AvailableReplicas:   1,
				UnavailableReplicas: 1,
			},
		}
		Expect(kubestatemetricssupport.SingleReplicaRolloutComplete(deployment, 1)).To(HaveOccurred())

		By("验证只有新 generation 的单副本完全收敛后才允许绑定 metrics port-forward")
		deployment.Status = appsv1.DeploymentStatus{
			ObservedGeneration: 2,
			Replicas:           1,
			UpdatedReplicas:    1,
			ReadyReplicas:      1,
			AvailableReplicas:  1,
		}
		Expect(kubestatemetricssupport.SingleReplicaRolloutComplete(deployment, 1)).To(Succeed())
	})
})
