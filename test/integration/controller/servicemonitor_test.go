package controller_test

import (
	"context"
	api "github.com/fjsxy/monitor-in-hand/api/v1alpha1"
	controller "github.com/fjsxy/monitor-in-hand/internal/controller"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"path/filepath"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"time"
)

var _ = Describe("配套采集真实 API", Ordered, func() {
	ctx := context.Background()
	var cr *api.KubeStateMetrics
	var r *controller.KubeStateMetricsReconciler
	gvr := schema.GroupVersionResource{Group: "monitoring.coreos.com", Version: "v1", Resource: "servicemonitors"}
	reconcile := func() {
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cr.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(testClient.Get(ctx, client.ObjectKey{Name: cr.Name}, cr)).To(Succeed())
	}
	BeforeAll(func() {
		By("通过真实 API 准备类型化 CR 与动态配套客户端")
		Expect(testClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "gh86"}})).To(Succeed())
		cr = &api.KubeStateMetrics{ObjectMeta: metav1.ObjectMeta{Name: "integration-gh86"}, Spec: api.KubeStateMetricsSpec{TargetNamespace: "gh86", ServiceMonitor: &api.KubeStateMetricsServiceMonitor{Enabled: true, Labels: map[string]string{"monitor-mgr.io/user-id": "owner-86"}}}}
		dc, err := dynamic.NewForConfig(testConfig)
		Expect(err).NotTo(HaveOccurred())
		r = &controller.KubeStateMetricsReconciler{Client: testClient, ServerVersion: fixedServerVersion{gitVersion: "v1.28.15"}, DynamicClient: dc}
	})
	It("IT-SM-GH86-001 CRD 保存新声明并默认关闭历史配套采集", func() {
		By("创建声明并核对新字段未被 API 裁剪")
		Expect(testClient.Create(ctx, cr)).To(Succeed())
		Expect(testClient.Get(ctx, client.ObjectKey{Name: cr.Name}, cr)).To(Succeed())
		Expect(cr.Spec.ServiceMonitor.Enabled).To(BeTrue())
		Expect(cr.Spec.ServiceMonitor.Labels).To(HaveKeyWithValue("monitor-mgr.io/user-id", "owner-86"))
		old := &api.KubeStateMetrics{ObjectMeta: metav1.ObjectMeta{Name: "old-gh86"}, Spec: api.KubeStateMetricsSpec{TargetNamespace: "gh86"}}
		Expect(testClient.Create(ctx, old)).To(Succeed())
		Expect(old.Spec.ServiceMonitor).To(BeNil())
		Expect(testClient.Delete(ctx, old)).To(Succeed())
	})
	It("IT-SM-GH86-003 同一 controller 在 API 延迟安装后继续收敛", func() {
		By("尚无 SM API 时仍创建 workload，独立报告依赖故障")
		reconcile()
		Expect(cr.Status.ServiceMonitor.State).To(Equal("Degraded"))
		Expect(cr.Status.ServiceMonitor.Reason).To(Equal("DependencyMissing"))
		By("安装官方固定版本 CRD 后保持原 CR UID 与 generation 再次调谐")
		uid, generation := cr.UID, cr.Generation
		path, err := filepath.Abs("../../../test/fixtures/servicemonitor")
		Expect(err).NotTo(HaveOccurred())
		_, err = envtest.InstallCRDs(testConfig, envtest.CRDInstallOptions{Paths: []string{path}, MaxTime: 30 * time.Second})
		Expect(err).NotTo(HaveOccurred())
		reconcile()
		Expect(cr.Status.ServiceMonitor.State).To(Equal("Ready"))
		Expect(cr.Status.ServiceMonitor.Resource.UID).NotTo(BeEmpty())
		Expect(cr.UID).To(Equal(uid))
		Expect(cr.Generation).To(Equal(generation))
	})
	It("IT-SM-GH86-002 真实 owner、漂移、重建与关闭清理可核验", func() {
		By("回读独立对象并制造真实 spec 漂移")
		endpoint := r.DynamicClient.Resource(gvr).Namespace("gh86")
		obj, err := endpoint.Get(ctx, "ksm-integration-gh86", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(obj.GetOwnerReferences()[0].UID).To(Equal(cr.UID))
		uid := obj.GetUID()
		obj.Object["spec"].(map[string]interface{})["endpoints"] = []interface{}{map[string]interface{}{"port": "wrong"}}
		_, err = endpoint.Update(ctx, obj, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		reconcile()
		obj, err = endpoint.Get(ctx, obj.GetName(), metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(obj.Object["spec"].(map[string]interface{})["endpoints"].([]interface{})[0].(map[string]interface{})["port"]).To(Equal("http-metrics"))
		By("删除配套对象后同一父身份重建，关闭只清理配套对象")
		Expect(endpoint.Delete(ctx, obj.GetName(), metav1.DeleteOptions{})).To(Succeed())
		reconcile()
		Expect(types.UID(cr.Status.ServiceMonitor.Resource.UID)).NotTo(Equal(uid))
		cr.Spec.ServiceMonitor.Enabled = false
		Expect(testClient.Update(ctx, cr)).To(Succeed())
		reconcile()
		reconcile()
		Expect(cr.Status.ServiceMonitor.State).To(Equal("Disabled"))
		By("停用和删除沿用 finalizer，清理所有拥有资源")
		disabled := false
		cr.Spec.Enabled = &disabled
		Expect(testClient.Update(ctx, cr)).To(Succeed())
		reconcile()
		reconcile()
		Expect(cr.Status.Phase).To(Equal(api.KubeStateMetricsPhaseDisabled))
		Expect(testClient.Delete(ctx, cr)).To(Succeed())
		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cr.Name}})
		Expect(err).NotTo(HaveOccurred())
	})
})
