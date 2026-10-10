package kubestatemetricssupport_test

import (
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kubestatemetricssupport "github.com/fjsxy/monitor-in-hand/test/e2e/kubestatemetrics/support"
)

var _ = Describe("workload replace manifest", func() {
	It("[UT-KSM-GH82-003] 省略 server-owned status 并保留对象身份与 spec", func() {
		By("准备含旧版 API 不支持的 status.availableReplicas 的 typed StatefulSet")
		replicas := int32(2)
		statefulSet := appsv1.StatefulSet{
			TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"},
			ObjectMeta: metav1.ObjectMeta{
				Name:            "ksm-gh82-statefulset-sharded",
				ResourceVersion: "123",
			},
			Spec:   appsv1.StatefulSetSpec{Replicas: &replicas},
			Status: appsv1.StatefulSetStatus{AvailableReplicas: 2},
		}

		By("通过实际 E2E 共用的构造入口生成 replace manifest")
		manifest, err := kubestatemetricssupport.ReplaceObjectManifest(&statefulSet)
		Expect(err).NotTo(HaveOccurred())

		By("验证不提交 status，且完整保留 apiVersion、kind、metadata 和 spec")
		var object map[string]json.RawMessage
		Expect(json.Unmarshal(manifest, &object)).To(Succeed())
		Expect(object).NotTo(HaveKey("status"))
		var replaced appsv1.StatefulSet
		Expect(json.Unmarshal(manifest, &replaced)).To(Succeed())
		Expect(replaced.TypeMeta).To(Equal(statefulSet.TypeMeta))
		Expect(replaced.ObjectMeta).To(Equal(statefulSet.ObjectMeta))
		Expect(replaced.Spec).To(Equal(statefulSet.Spec))
	})
})
