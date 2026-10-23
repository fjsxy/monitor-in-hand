package kubestatemetrics_test

import (
	"encoding/json"
	api "github.com/fjsxy/monitor-in-hand/api/v1alpha1"
	ksm "github.com/fjsxy/monitor-in-hand/internal/kubestatemetrics"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"strings"
)

var _ = Describe("配套采集配置规划", func() {
	It("UT-SM-GH86-001 按同一实例身份生成可选 ServiceMonitor", func() {
		By("从公开 CR 声明启用配套采集并规划实际资源")
		cr := &api.KubeStateMetrics{ObjectMeta: metav1.ObjectMeta{Name: "example", UID: types.UID("owner-86")}}
		Expect(json.Unmarshal([]byte(`{"targetNamespace":"monitoring","serviceMonitor":{"enabled":true,"labels":{"monitor-mgr.io/user-id":"user-1"}}}`), &cr.Spec)).To(Succeed())
		plan, problem := ksm.BuildPlan(cr, ksm.ClusterFacts{KubernetesVersion: "v1.28.15", TargetNamespaceExists: true, NamespaceNames: []string{"monitoring"}})
		Expect(problem).To(BeNil())
		Expect(plan.ServiceMonitor).NotTo(BeNil())
		Expect(plan.ServiceMonitor.GetName()).To(Equal("ksm-example"))
		Expect(plan.ServiceMonitor.GetNamespace()).To(Equal("monitoring"))
		Expect(plan.ServiceMonitor.GetLabels()).To(HaveKeyWithValue("monitor-mgr.io/user-id", "user-1"))
		Expect(plan.ServiceMonitor.GetOwnerReferences()[0].UID).To(Equal(types.UID("owner-86")))
		Expect(plan.ServiceMonitor.Object["spec"]).To(Equal(map[string]interface{}{
			"selector":          map[string]interface{}{"matchLabels": map[string]interface{}{"app.kubernetes.io/name": "kube-state-metrics", "app.kubernetes.io/instance": "example", "app.kubernetes.io/managed-by": "monitor-in-hand"}},
			"namespaceSelector": map[string]interface{}{"matchNames": []interface{}{"monitoring"}},
			"endpoints":         []interface{}{map[string]interface{}{"port": "http-metrics", "path": "/metrics", "interval": "30s", "scrapeTimeout": "10s"}},
		}))
		By("未声明采集时不改变旧实例的资源集合")
		cr.Spec.ServiceMonitor = nil
		plan, problem = ksm.BuildPlan(cr, ksm.ClusterFacts{KubernetesVersion: "v1.19.16", TargetNamespaceExists: true, NamespaceNames: []string{"monitoring"}})
		Expect(problem).To(BeNil())
		Expect(plan.ServiceMonitor).To(BeNil())
	})
})

var _ = Describe("配套采集声明校验", func() {
	DescribeTable("UT-SM-GH86-002 非法声明不生成可写资源", func(config string) {
		cr := &api.KubeStateMetrics{ObjectMeta: metav1.ObjectMeta{Name: "invalid"}}
		Expect(json.Unmarshal([]byte(`{"targetNamespace":"monitoring","serviceMonitor":`+config+`}`), &cr.Spec)).To(Succeed())
		plan, problem := ksm.BuildPlan(cr, ksm.ClusterFacts{KubernetesVersion: "v1.28.15", TargetNamespaceExists: true})
		Expect(plan).To(BeNil())
		Expect(problem.Reason).To(Equal(ksm.ReasonInvalidSpec))
	},
		Entry("超时超过间隔", `{"enabled":true,"interval":"5s"}`),
		Entry("负时间", `{"enabled":true,"interval":"-1s"}`),
		Entry("非法时长", `{"enabled":true,"scrapeTimeout":"later"}`),
		Entry("零时长", `{"enabled":true,"scrapeTimeout":"0s"}`),
		Entry("身份标签", `{"labels":{"app.kubernetes.io/instance":"other"}}`),
		Entry("管理标签", `{"labels":{"monitor-in-hand/role":"other"}}`),
		Entry("非法label", `{"labels":{"space key":"other"}}`))
	It("UT-SM-GH86-004 长名称与合法拓扑复用稳定选择契约", func() {
		for _, version := range []string{"v1.19.16", "v1.28.15"} {
			for _, mode := range []api.DeploymentMode{api.DeploymentModeDeployment, api.DeploymentModeStatefulSetSharded, api.DeploymentModeDaemonSetSharded} {
				cr := &api.KubeStateMetrics{ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("long", 20) + ".example", UID: types.UID("uid")}, Spec: api.KubeStateMetricsSpec{TargetNamespace: "monitoring", ServiceMonitor: &api.KubeStateMetricsServiceMonitor{Enabled: true}, Deployment: api.KubeStateMetricsDeploymentSpec{Mode: mode}}}
				if mode == api.DeploymentModeStatefulSetSharded {
					cr.Spec.Deployment.StatefulSetSharded = &api.KubeStateMetricsStatefulSetShardedSpec{Shards: 2}
				}
				if mode == api.DeploymentModeDaemonSetSharded {
					cr.Spec.Deployment.DaemonSetSharded = &api.KubeStateMetricsDaemonSetShardedSpec{}
				}
				plan, problem := ksm.BuildPlan(cr, ksm.ClusterFacts{KubernetesVersion: version, TargetNamespaceExists: true, NamespaceNames: []string{"monitoring"}})
				if version == "v1.19.16" && mode == api.DeploymentModeDaemonSetSharded {
					Expect(problem.Reason).To(Equal(ksm.ReasonUnsupportedMode))
					continue
				}
				Expect(problem).To(BeNil())
				Expect(len(plan.ServiceMonitor.GetName())).To(BeNumerically("<=", 63))
				Expect(plan.ServiceMonitor.GetName()).To(Equal(plan.Name))
			}
		}
	})
})
